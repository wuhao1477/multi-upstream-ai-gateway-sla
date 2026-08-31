#!/usr/bin/env bash
# /admin/config 的端到端测试（#4 完成标准）。
#
# 起真 PG + 真 sla-core，用 curl 走完整流程。单测覆盖不到的部分在这里验：
# 二次确认两步提交、乐观锁 409、幂等去重、表外键 400、令牌鉴权。
set -euo pipefail
cd "$(dirname "$0")/.."

CT=cfgtestpg
PORT=55434
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
sleep 2

go build -o bin/sla-core ./cmd/sla-core
DATABASE_URL="$DSN" ADMIN_TOKEN="$TOKEN" ./bin/sla-core -addr ":${APIPORT}" \
  >/tmp/core-test.log 2>&1 &
CORE_PID=$!
for _ in $(seq 1 30); do
  curl -sf "http://127.0.0.1:${APIPORT}/healthz" >/dev/null 2>&1 && break
  sleep 1
done

A="http://127.0.0.1:${APIPORT}/admin"
auth=(-H "Authorization: Bearer $TOKEN")
code() { curl -s -o /tmp/resp.json -w "%{http_code}" "$@"; }
body() { cat /tmp/resp.json; }

fail() { echo "❌ $1"; echo "   响应: $(body)"; exit 1; }

echo "── 1/9 鉴权 ──"
[ "$(code "$A/config")" = "401" ] || fail "无令牌应 401"
[ "$(code -H 'Authorization: Bearer wrong' "$A/config")" = "401" ] || fail "错令牌应 401"
[ "$(code "${auth[@]}" "$A/config")" = "200" ] || fail "正确令牌应 200"
# 令牌不得回显（FR-094 同源纪律）
if grep -q "$TOKEN" /tmp/resp.json; then fail "响应回显了管理令牌"; fi
echo "   ✅ 401/401/200，令牌不回显"

echo "── 2/9 列表：全部键可见，EnvSourced 项不回显值 ──"
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

echo "── 3/9 表外键必须 400（09 §3 白名单语义）──"
C=$(code "${auth[@]}" -X POST "$A/config/preview" \
  -d '{"param_key":"no_such_key","new_value":"1","changed_by":"t","change_reason":"t"}')
[ "$C" = "400" ] || fail "表外键应 400，得到 $C"
echo "   ✅ 表外键 400"

echo "── 4/9 非关键项可直接 apply ──"
C=$(code "${auth[@]}" -X POST "$A/config/apply" \
  -d '{"param_key":"max_hops","new_value":"4","changed_by":"ops","change_reason":"测试"}')
[ "$C" = "200" ] || fail "非关键项直接 apply 应 200，得到 $C"
V=$(docker exec "$CT" psql -U postgres -d sla -tAc \
  "select param_value#>>'{}' from config_params where param_key='max_hops' order by version desc limit 1")
[ "$V" = "4" ] || fail "库中值 = $V，期望 4"
echo "   ✅ 非关键项 200，库中已生效"

echo "── 5/9 关键项无令牌必须 400 ──"
C=$(code "${auth[@]}" -X POST "$A/config/apply" \
  -d '{"param_key":"data_policy_enabled","new_value":"true","changed_by":"ops","change_reason":"测试"}')
[ "$C" = "400" ] || fail "关键项缺 confirm_token 应 400，得到 $C"
echo "   ✅ 关键项强制两步提交"

echo "── 6/9 关键项两步提交走通 ──"
code "${auth[@]}" -X POST "$A/config/preview" \
  -d '{"param_key":"data_policy_enabled","new_value":"true","changed_by":"ops","change_reason":"启用数据许可"}' >/dev/null
TOK=$(python3 -c "import json;print(json.load(open('/tmp/resp.json'))['confirm_token'])")
VER=$(python3 -c "import json;print(json.load(open('/tmp/resp.json'))['current_version'])")
python3 -c "
import json; d=json.load(open('/tmp/resp.json'))
assert d['is_critical'] is True, 'is_critical 应为 true'
assert '关键项' in d['impact'], 'impact 应标明关键项：'+d['impact']
"
C=$(code "${auth[@]}" -X POST "$A/config/apply" -d "{
  \"param_key\":\"data_policy_enabled\",\"new_value\":\"true\",
  \"changed_by\":\"ops\",\"change_reason\":\"启用数据许可\",
  \"confirm_token\":\"$TOK\",\"expected_current_version\":$VER}")
[ "$C" = "200" ] || fail "两步提交应 200，得到 $C"
CONF=$(docker exec "$CT" psql -U postgres -d sla -tAc \
  "select confirmed_twice from config_params where param_key='data_policy_enabled' order by version desc limit 1")
[ "$CONF" = "t" ] || fail "confirmed_twice 应为 true，得到 $CONF"
echo "   ✅ preview→apply 走通，confirmed_twice=true"

echo "── 7/9 令牌一次性 ──"
C=$(code "${auth[@]}" -X POST "$A/config/apply" -d "{
  \"param_key\":\"data_policy_enabled\",\"new_value\":\"true\",
  \"changed_by\":\"ops\",\"change_reason\":\"重放\",
  \"confirm_token\":\"$TOK\",\"expected_current_version\":$VER}")
[ "$C" = "400" ] || fail "令牌重用应 400，得到 $C"
echo "   ✅ 令牌不可重用"

echo "── 8/9 乐观锁：过期版本必须 409 ──"
C=$(code "${auth[@]}" -X POST "$A/config/apply" \
  -d '{"param_key":"max_hops","new_value":"9","changed_by":"ops",
       "change_reason":"用旧版本号","expected_current_version":1}')
[ "$C" = "409" ] || fail "expected_current_version 过期应 409，得到 $C"
echo "   ✅ 版本冲突 409"

echo "── 9/9 幂等：同 key 重复投递只生效一次 ──"
IDK="idem-$$"
code "${auth[@]}" -X POST "$A/config/apply" -d "{
  \"param_key\":\"min_hop_ms\",\"new_value\":\"1600\",\"changed_by\":\"ops\",
  \"change_reason\":\"幂等测试\",\"idempotency_key\":\"$IDK\"}" >/dev/null
code "${auth[@]}" -X POST "$A/config/apply" -d "{
  \"param_key\":\"min_hop_ms\",\"new_value\":\"1600\",\"changed_by\":\"ops\",
  \"change_reason\":\"幂等测试\",\"idempotency_key\":\"$IDK\"}" >/dev/null
python3 -c "
import json; d=json.load(open('/tmp/resp.json'))
assert d.get('idempotent_replay') is True, '第二次应标记为幂等重放: '+json.dumps(d,ensure_ascii=False)
"
CNT=$(docker exec "$CT" psql -U postgres -d sla -tAc \
  "select count(*) from config_params where param_key='min_hop_ms'")
[ "$CNT" = "2" ] || fail "min_hop_ms 版本数 = $CNT，期望 2（种子 + 一次改动）"
echo "   ✅ 重复投递只产生一个新版本"

echo "── 附加：admin_token 不落库（09 §4bis 避免自己改自己）──"
CNT=$(docker exec "$CT" psql -U postgres -d sla -tAc \
  "select count(*) from config_params where param_key='admin_token'")
[ "$CNT" = "0" ] || fail "admin_token 落库了 $CNT 行——管理 API 能改自己的鉴权令牌 = 提权"
echo "   ✅ admin_token 未落 config_params"

echo "── 附加：历史可查（FR-099 前后值/责任人/原因）──"
code "${auth[@]}" "$A/config/history?param_key=max_hops" >/dev/null
python3 -c "
import json; d=json.load(open('/tmp/resp.json'))
vs=d['versions']
assert len(vs)>=2, '应有至少 2 个版本'
top=vs[0]
assert top['ChangedBy']=='ops', 'changed_by 未记录: '+json.dumps(top,ensure_ascii=False)
assert top['PrevValue']=='3', 'prev_value 应为 3（原默认值），实际 '+str(top['PrevValue'])
"
echo "   ✅ 历史含前后值与责任人"

echo
echo "✅ /admin/config 端到端测试全部通过"
