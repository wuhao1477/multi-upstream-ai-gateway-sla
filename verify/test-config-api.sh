#!/usr/bin/env bash
# /admin/config 的端到端测试（#4 完成标准）。
#
# 起真 PG + 真 sla-core，用 curl 走完整流程。单测覆盖不到的部分在这里验：
# 乐观锁 409、幂等去重、表外键 400、环境项只读、令牌鉴权。
set -euo pipefail
cd "$(dirname "$0")/.."

CT=cfgtestpg
PORT=18434   # 不用 554xx：那在临时端口段里，会被出站连接借走（见 test-migrate.sh 顶部）
APIPORT=18081
DSN="postgres://postgres:x@127.0.0.1:${PORT}/sla"
TOKEN="test-admin-token-$$"

CORE_PID=""
cleanup() {
  [ -n "$CORE_PID" ] && kill "$CORE_PID" 2>/dev/null || true
  docker rm -f -v "$CT" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

echo "── 起 PG 与 sla-core ──"
docker run -d --name "$CT" -e POSTGRES_PASSWORD=x -e POSTGRES_DB=sla \
  -p "${PORT}:5432" postgres:16 >/dev/null
for _ in $(seq 1 60); do
  docker exec "$CT" pg_isready -U postgres >/dev/null 2>&1 && break
  sleep 1
done
# ⚠️ 循环跑完必须查一次。原先不查就往下走 —— PG 没起来时 core 连不上库直接退出，
#    而第一条断言表现成「无令牌应 401」失败、并打印**上一轮遗留的** /tmp/resp.json
#    （2026-09-01 实测：打出来的是上一轮配置测试的历史）。红是对的，
#    但红的理由指向鉴权，人会去查鉴权。同 CLAUDE.md 那条：红要红在真原因上。
docker exec "$CT" pg_isready -U postgres >/dev/null 2>&1 || {
  echo "❌ PG 60s 内没起来（端口 ${PORT} 被占？镜像拉不到？）"
  docker logs "$CT" 2>&1 | tail -20 | sed 's/^/   /'
  exit 1; }
sleep 2

go build -o bin/sla-core ./cmd/sla-core
DATABASE_URL="$DSN" ADMIN_TOKEN="$TOKEN" ./bin/sla-core -addr ":${APIPORT}" \
  >/tmp/core-test.log 2>&1 &
CORE_PID=$!
for _ in $(seq 1 30); do
  curl -sf "http://127.0.0.1:${APIPORT}/healthz" >/dev/null 2>&1 && break
  sleep 1
done
curl -sf "http://127.0.0.1:${APIPORT}/healthz" >/dev/null 2>&1 || {
  echo "❌ sla-core 30s 内没就绪，日志如下："
  tail -20 /tmp/core-test.log | sed 's/^/   /'
  exit 1; }

A="http://127.0.0.1:${APIPORT}/admin"
auth=(-H "Authorization: Bearer $TOKEN")
code() { : >/tmp/resp.json; curl -s -o /tmp/resp.json -w "%{http_code}" "$@"; }
body() { cat /tmp/resp.json; }

fail() { echo "❌ $1"; echo "   响应: $(body)"; exit 1; }

echo "── 1/6 鉴权 ──"
[ "$(code "$A/config")" = "401" ] || fail "无令牌应 401"
[ "$(code -H 'Authorization: Bearer wrong' "$A/config")" = "401" ] || fail "错令牌应 401"
[ "$(code "${auth[@]}" "$A/config")" = "200" ] || fail "正确令牌应 200"
# 令牌不得回显（FR-094 同源纪律）
if grep -q "$TOKEN" /tmp/resp.json; then fail "响应回显了管理令牌"; fi
echo "   ✅ 401/401/200，令牌不回显"

echo "── 2/6 列表：全部键可见，EnvSourced 项不回显值 ──"
code "${auth[@]}" "$A/config" >/dev/null
TOTAL=$(grep -c '{Key: ' internal/config/params_gen.go)
N=$(python3 -c "import json;print(json.load(open('/tmp/resp.json'))['count'])")
[ "$N" = "$TOTAL" ] || fail "配置项数 = $N，期望 $TOTAL"
# admin_token 应可见但不回显值/默认值（其"值"就是当前鉴权令牌，FR-094）
python3 - <<'PYEOF'
import json
items={i["key"]:i for i in json.load(open('/tmp/resp.json'))["items"]}
t=items.get("admin_token")
assert t is not None, "admin_token 应在清单里可见（09 §4bis 登记其存在）"
assert t.get("env_sourced") is True, "admin_token 应标 env_sourced"
assert not t.get("value"), "admin_token 不得回显值：其值就是当前鉴权令牌"
assert not t.get("default"), "admin_token 不得回显默认值（那是一句说明）"
PYEOF
echo "   ✅ $TOTAL 键可见，admin_token 不回显值"

echo "── 3/6 表外键必须 400（09 §3 白名单语义）──"
C=$(code "${auth[@]}" -X POST "$A/config/preview" \
  -d '{"param_key":"no_such_key","new_value":"1","changed_by":"t","change_reason":"t"}')
[ "$C" = "400" ] || fail "表外键应 400，得到 $C"
echo "   ✅ 表外键 400"

echo "── 4/6 非关键项可直接 apply ──"
C=$(code "${auth[@]}" -X POST "$A/config/apply" \
  -d '{"param_key":"sync_min_interval_s","new_value":"90","changed_by":"ops","change_reason":"测试"}')
[ "$C" = "200" ] || fail "非关键项直接 apply 应 200，得到 $C"
V=$(docker exec "$CT" psql -U postgres -d sla -tAc \
  "select param_value#>>'{}' from config_params where param_key='sync_min_interval_s' order by version desc limit 1")
[ "$V" = "90" ] || fail "库中值 = $V，期望 90"
echo "   ✅ 非关键项 200，库中已生效"

echo "── 5/6 乐观锁：过期版本必须 409 ──"
C=$(code "${auth[@]}" -X POST "$A/config/apply" \
  -d '{"param_key":"sync_min_interval_s","new_value":"120","changed_by":"ops",
       "change_reason":"用旧版本号","expected_current_version":1}')
[ "$C" = "409" ] || fail "expected_current_version 过期应 409，得到 $C"
echo "   ✅ 版本冲突 409"

echo "── 6/6 幂等：同 key 重复投递只生效一次 ──"
IDK="idem-$$"
code "${auth[@]}" -X POST "$A/config/apply" -d "{
  \"param_key\":\"collector_request_interval_ms\",\"new_value\":\"250\",\"changed_by\":\"ops\",
  \"change_reason\":\"幂等测试\",\"idempotency_key\":\"$IDK\"}" >/dev/null
code "${auth[@]}" -X POST "$A/config/apply" -d "{
  \"param_key\":\"collector_request_interval_ms\",\"new_value\":\"250\",\"changed_by\":\"ops\",
  \"change_reason\":\"幂等测试\",\"idempotency_key\":\"$IDK\"}" >/dev/null
python3 -c "
import json; d=json.load(open('/tmp/resp.json'))
assert d.get('idempotent_replay') is True, '第二次应标记为幂等重放: '+json.dumps(d,ensure_ascii=False)
"
CNT=$(docker exec "$CT" psql -U postgres -d sla -tAc \
  "select count(*) from config_params where param_key='collector_request_interval_ms'")
[ "$CNT" = "2" ] || fail "collector_request_interval_ms 版本数 = $CNT，期望 2（种子 + 一次改动）"
echo "   ✅ 重复投递只产生一个新版本"

echo "── 附加：admin_token 不可修改且不落库（09 §4bis 避免自己改自己）──"
C=$(code "${auth[@]}" -X POST "$A/config/apply" \
  -d '{"param_key":"admin_token","new_value":"replacement","changed_by":"ops","change_reason":"测试"}')
[ "$C" = "400" ] || fail "admin_token 应拒绝修改，得到 $C"
CNT=$(docker exec "$CT" psql -U postgres -d sla -tAc \
  "select count(*) from config_params where param_key='admin_token'")
[ "$CNT" = "0" ] || fail "admin_token 落库了 $CNT 行——管理 API 能改自己的鉴权令牌 = 提权"
echo "   ✅ admin_token 拒绝修改且未落 config_params"

echo "── 附加：历史可查（FR-099 前后值/责任人/原因）──"
code "${auth[@]}" "$A/config/history?param_key=sync_min_interval_s" >/dev/null
python3 -c "
import json; d=json.load(open('/tmp/resp.json'))
vs=d['versions']
assert len(vs)>=2, '应有至少 2 个版本'
top=vs[0]
assert top['ChangedBy']=='ops', 'changed_by 未记录: '+json.dumps(top,ensure_ascii=False)
assert top['PrevValue']=='60', 'prev_value 应为 60（原默认值），实际 '+str(top['PrevValue'])
"
echo "   ✅ 历史含前后值与责任人"

echo
echo "✅ /admin/config 端到端测试全部通过"
