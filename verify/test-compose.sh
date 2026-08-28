#!/usr/bin/env bash
# compose 全栈冒烟（#3 完成标准）：起栈 → /healthz 全绿 → 停一个实例仍可服务
# （AC-27 的 M0/P1 形态）→ 确认 /admin/* 从宿主机不可达。
set -euo pipefail
cd "$(dirname "$0")/.."

export ADMIN_TOKEN="smoke-$(date +%s)"
export POSTGRES_PASSWORD="smoke-pg"
COMPOSE=(docker compose -f deploy/docker-compose.yml)

cleanup() { "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "── 1/5 起栈 ──"
"${COMPOSE[@]}" up -d --build >/dev/null
# Caddy 用自签证书，故 curl 加 -k
CURL=(curl -sk --max-time 5)

# ⚠️ 判据必须是**真的拿到 200**，不能只看 curl 退出码：
#    重定向、404 文本、空体都会让 curl 退 0，于是"就绪"通过而下一步炸在
#    JSON 解析上（本轮就这样，报了个看不出病因的 Expecting value）。
ready=false
for _ in $(seq 1 90); do
  if [ "$("${CURL[@]}" -o /dev/null -w '%{http_code}' https://localhost/healthz 2>/dev/null)" = "200" ]; then
    ready=true; break
  fi
  sleep 2
done
$ready || {
  echo "❌ 90 次重试后 /healthz 仍未返回 200"
  echo "── 最后一次响应 ──"
  "${CURL[@]}" -i https://localhost/healthz 2>&1 | head -20 || true
  echo "── caddy 日志 ──"; "${COMPOSE[@]}" logs --tail=25 caddy
  echo "── core 日志 ──"; "${COMPOSE[@]}" logs --tail=15 sla-core-a sla-core-b
  exit 1; }
echo "   ✅ 栈已就绪"

echo "── 2/5 /healthz 内容正确 ──"
# 状态码与响应体分开取：先断言 200 再解析 JSON。
# 直接 json.load 会在非 JSON 响应（重定向空体、404 文本）上抛栈，
# 报"Expecting value: line 2 column 1"这种看不出病因的错。
HCODE=$("${CURL[@]}" -o /tmp/healthz.json -w "%{http_code}" https://localhost/healthz)
if [ "$HCODE" != "200" ]; then
  echo "❌ /healthz 返回 $HCODE，期望 200"
  echo "   响应体："; head -c 500 /tmp/healthz.json; echo
  "${COMPOSE[@]}" logs --tail=30 caddy
  exit 1
fi
python3 - <<'PYEOF'
import json, sys
raw = open('/tmp/healthz.json').read()
try:
    d = json.loads(raw)
except Exception as e:
    print(f"❌ /healthz 响应不是 JSON: {e}")
    print("   原始响应:", repr(raw[:300]))
    sys.exit(1)
assert d['status'] == 'ok', d
assert d['db'] == 'ok', d
assert d['config_snapshot'] == 'loaded', d
# 06 §6 纪律：响应须声明本端点不代表上游可用性
assert d.get('note'), '缺少 note 声明（06 §6 健康语义分层）'
print('   ✅', d['status'], '| db', d['db'], '| snapshot', d['config_snapshot'])
PYEOF

echo "── 3/5 选主：迁移只执行一遍，落败者等待而非跳过 ──"
# ⚠️ 这一步抓到过真 bug：首版落败者 return nil 直接去灌种子，而抢到锁的
#    实例还没建出 config_params → "relation config_params does not exist"。
#    单实例集成测试因无竞争而完全看不出来，只有双实例 compose 能暴露。
WANT_MIG=$(ls migrations/*.sql | wc -l | tr -d ' ')
LOGS=$("${COMPOSE[@]}" logs sla-core-a sla-core-b collector 2>/dev/null)
APPLIED=$(echo "$LOGS" | grep -c "已应用迁移" || true)
# 每个文件恰好被应用一次：多于此说明选主失效（两个实例都跑了）
[ "$APPLIED" = "$WANT_MIG" ] || {
  echo "❌ 「已应用迁移」日志 $APPLIED 行，期望恰好 $WANT_MIG（每文件一次）"
  echo "   多于期望 = 选主失效；少于 = 迁移不完整"
  exit 1; }
echo "   ✅ $WANT_MIG 个迁移各执行一次（选主生效）"

# 落败者必须**成功完成初始化**，不能因为表还没建好而报错退出
INIT_OK=$(echo "$LOGS" | grep -c "初始化完成" || true)
[ "$INIT_OK" -ge 2 ] || {
  echo "❌ 只有 $INIT_OK 个实例完成初始化，期望 ≥2（core-a/b 都要成功）"
  echo "$LOGS" | grep -iE "error|失败" | head -5
  exit 1; }
echo "   ✅ 两个实例都完成初始化（落败者等待后继续，不是跳过）"

# 不得出现建表竞态的痕迹
if echo "$LOGS" | grep -q "does not exist"; then
  echo "❌ 日志出现 \"does not exist\" —— 建表与用表存在竞态"
  echo "$LOGS" | grep "does not exist" | head -3
  exit 1
fi
echo "   ✅ 无建表竞态"

# 种子竞态：两个实例同时灌 71 键，WHERE NOT EXISTS 挡不住并发
# （检查与插入之间有窗口，两侧都判"不存在"→ 同一 version=1 撞唯一约束）。
# 这是本脚本抓到的第二个 bug，修法是 ON CONFLICT DO NOTHING。
if echo "$LOGS" | grep -qi "duplicate key"; then
  echo "❌ 日志出现 \"duplicate key\" —— 种子在并发下不幂等"
  echo "$LOGS" | grep -i "duplicate key" | head -3
  exit 1
fi
echo "   ✅ 种子并发安全（无唯一约束冲突）"

# 键数必须恰好等于可种子化项：多了说明重复插入，少了说明有实例插一半就崩
WANT_KEYS=$(grep -c 'EnvSourced: false}' internal/config/params_gen.go)
GOT_KEYS=$("${COMPOSE[@]}" exec -T postgres psql -U postgres -d sla -tAc \
  "select count(*) from config_params where scope_type='global'" | tr -d ' \r')
[ "$GOT_KEYS" = "$WANT_KEYS" ] || {
  echo "❌ 双实例启动后配置键数 = $GOT_KEYS，期望 $WANT_KEYS"
  exit 1; }
echo "   ✅ 配置键数 $WANT_KEYS（双实例并发灌种子后仍准确）"

# collector 容器必须真的跑 collector。
# ⚠️ 抓到过：compose 用 command 覆盖，而 Dockerfile 的 ENTRYPOINT 是 /sla-core，
#    command 只覆盖 CMD → 实际执行 `/sla-core /collector`，sla-core 忽略多余
#    参数照常启动。症状是 collector 容器打出"sla-core 启动"，采集器根本没跑。
CLOG=$("${COMPOSE[@]}" logs collector 2>/dev/null)
if echo "$CLOG" | grep -q "sla-core 启动"; then
  echo "❌ collector 容器在跑 sla-core —— entrypoint 未生效（见本段注释）"
  exit 1
fi
echo "$CLOG" | grep -q "collector 启动" || {
  echo "❌ collector 容器没有打出 \"collector 启动\""
  echo "$CLOG" | tail -5; exit 1; }
echo "   ✅ collector 容器跑的是 collector"

echo "── 4/5 AC-27（M0/P1 形态）：停一个实例，30 次请求全成功 ──"
"${COMPOSE[@]}" stop sla-core-a >/dev/null
sleep 6   # 等 Caddy 健康探测摘除（health_interval 2s）
FAIL=0
for i in $(seq 1 30); do
  "${CURL[@]}" -o /dev/null -w "" https://localhost/healthz || FAIL=$((FAIL+1))
done
[ "$FAIL" = "0" ] || { echo "❌ 停一个实例后有 $FAIL/30 次失败（AC-27 要求全部成功）"; exit 1; }
echo "   ✅ 30/30 成功"
"${COMPOSE[@]}" start sla-core-a >/dev/null
sleep 6
"${CURL[@]}" https://localhost/healthz >/dev/null || { echo "❌ 恢复后不可达"; exit 1; }
echo "   ✅ 实例恢复后自动纳入"

echo "── 5/5 边界：/admin 与 /metrics 从宿主机不可达（06 §1）──"
for p in /admin/config /metrics; do
  C=$("${CURL[@]}" -o /dev/null -w "%{http_code}" "https://localhost$p" || echo "000")
  # 期望 404（Caddyfile 的 handle 兜底），绝不能是 200/401
  #（401 也意味着请求到了 sla-core —— 那说明代理了它）
  [ "$C" = "404" ] || { echo "❌ $p 返回 $C，应为 404（未代理）"; exit 1; }
  echo "   ✅ $p → 404（未代理）"
done
# 反向确认：管理面在容器网络内是可用的（否则上面的 404 可能只是服务挂了）
IN=$("${COMPOSE[@]}" exec -T sla-core-b sh -c \
  'wget -qO- --header="Authorization: Bearer '"$ADMIN_TOKEN"'" http://localhost:8080/admin/config' \
  2>/dev/null | head -c 20 || echo "")
if [ -z "$IN" ]; then
  # distroless 无 shell/wget，改用 core 自身不可行 → 用 caddy 容器发请求
  IN=$("${COMPOSE[@]}" exec -T caddy sh -c \
    'wget -qO- --header="Authorization: Bearer '"$ADMIN_TOKEN"'" http://sla-core-b:8080/admin/config' \
    2>/dev/null | head -c 20 || echo "")
fi
[ -n "$IN" ] || { echo "❌ 管理面在容器网络内也不可达——上面的 404 说明不了边界生效"; exit 1; }
echo "   ✅ 管理面在容器网络内可用（证明 404 来自未代理而非服务不可用）"

echo
echo "✅ compose 全栈冒烟通过"
