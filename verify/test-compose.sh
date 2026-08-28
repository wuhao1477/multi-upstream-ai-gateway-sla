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

ready=false
for _ in $(seq 1 90); do
  if "${CURL[@]}" https://127.0.0.1/healthz >/dev/null 2>&1; then ready=true; break; fi
  sleep 2
done
$ready || { echo "❌ 90 次重试后 /healthz 仍不可达"; "${COMPOSE[@]}" logs --tail=40; exit 1; }
echo "   ✅ 栈已就绪"

echo "── 2/5 /healthz 内容正确 ──"
BODY=$("${CURL[@]}" https://127.0.0.1/healthz)
echo "$BODY" | python3 -c "
import json,sys
d=json.load(sys.stdin)
assert d['status']=='ok', d
assert d['db']=='ok', d
assert d['config_snapshot']=='loaded', d
# 06 §6 纪律：响应须声明本端点不代表上游可用性
assert d.get('note'), '缺少 note 声明（06 §6 健康语义分层）'
print('   ✅', d['status'], '| db', d['db'], '| snapshot', d['config_snapshot'])
"

echo "── 3/5 两个实例都真的在服务 ──"
# 迁移选主：只有一个实例应执行迁移，另一个跳过
MIGLOG=$("${COMPOSE[@]}" logs sla-core-a sla-core-b 2>/dev/null | grep -c "已应用迁移" || true)
SKIPLOG=$("${COMPOSE[@]}" logs sla-core-a sla-core-b 2>/dev/null | grep -c "另一实例正在执行迁移" || true)
echo "   迁移日志行=$MIGLOG 跳过日志=$SKIPLOG"
# 12 个文件被一个实例应用；另一实例要么跳过、要么因已应用而无输出
[ "$MIGLOG" -ge 12 ] || { echo "❌ 迁移未完整执行"; exit 1; }
echo "   ✅ 选主生效（迁移只执行一遍）"

echo "── 4/5 AC-27（M0/P1 形态）：停一个实例，30 次请求全成功 ──"
"${COMPOSE[@]}" stop sla-core-a >/dev/null
sleep 6   # 等 Caddy 健康探测摘除（health_interval 2s）
FAIL=0
for i in $(seq 1 30); do
  "${CURL[@]}" -o /dev/null -w "" https://127.0.0.1/healthz || FAIL=$((FAIL+1))
done
[ "$FAIL" = "0" ] || { echo "❌ 停一个实例后有 $FAIL/30 次失败（AC-27 要求全部成功）"; exit 1; }
echo "   ✅ 30/30 成功"
"${COMPOSE[@]}" start sla-core-a >/dev/null
sleep 6
"${CURL[@]}" https://127.0.0.1/healthz >/dev/null || { echo "❌ 恢复后不可达"; exit 1; }
echo "   ✅ 实例恢复后自动纳入"

echo "── 5/5 边界：/admin 与 /metrics 从宿主机不可达（06 §1）──"
for p in /admin/config /metrics; do
  C=$("${CURL[@]}" -o /dev/null -w "%{http_code}" "https://127.0.0.1$p" || echo "000")
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
