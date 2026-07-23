#!/usr/bin/env bash
# ISSUE-001 六项假设的运行时探测。读取 setup.py 生成的 state.json。
# 输出到 stdout，请整体粘贴回对话，由分析方判定通过/不通过。
set -uo pipefail
cd "$(dirname "$0")"

BASE="http://localhost:8090"
KEY=$(python3 -c "import json;print(json.load(open('state.json'))['apiKey'])" 2>/dev/null)
TOKEN=$(python3 -c "import json;print(json.load(open('state.json'))['token'])" 2>/dev/null)
CHA=$(python3 -c "import json;print(json.load(open('state.json'))['channelA'])" 2>/dev/null)
if [ -z "${KEY:-}" ]; then echo "state.json 缺失，请先跑 python3 setup.py"; exit 1; fi

chat () { # $1=model; remaining args are passed through to curl (e.g. --max-time 2)
  local model="$1"; shift
  curl -sS -N -X POST "$BASE/v1/chat/completions" \
    -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
    "$@" \
    -d "{\"model\":\"$model\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
}

# portable millisecond timestamp (macOS/BSD `date` lacks %N; use gdate or python fallback)
ts () {
  if command -v gdate >/dev/null 2>&1; then gdate +%H:%M:%S.%3N
  else python3 -c 'from datetime import datetime as d; print(d.now().strftime("%H:%M:%S.%f")[:-3])'
  fi
}

echo "===================================================================="
echo "假设3 首字定义：mock-normal 先发 role-only delta，0.5s 后才发内容。"
echo "观察 SSE 到达时序；随后查 execution 记录的 metrics_first_token_latency_ms。"
echo "若记录值≈0（贴近 role delta）而非≈500ms（首内容），即证实首字=首事件。"
echo "--- SSE (带本地时间戳) ---"
chat mock-normal | while IFS= read -r line; do printf '%s  %s\n' "$(ts)" "$line"; done
echo

echo "===================================================================="
echo "假设3b heartbeat：mock-heartbeat 先发 SSE 注释心跳再发内容。"
chat mock-heartbeat | while IFS= read -r line; do printf '%s  %s\n' "$(ts)" "$line"; done
echo

echo "===================================================================="
echo "假设1 单渠道隔离：Key 的 active profile 仅含渠道A。让渠道A对应的"
echo "mock-500 失败，确认不会切到渠道B（无越权回退）。期望：返回错误，不成功。"
chat mock-500
echo; echo "(期望上面是错误，而非来自渠道B的成功响应)"
echo

echo "===================================================================="
echo "假设3c 空响应：mock-empty-sse 只发 role delta 后即结束，无内容。"
chat mock-empty-sse | while IFS= read -r line; do printf '%s  %s\n' "$(ts)" "$line"; done
echo

echo "===================================================================="
echo "假设2 取消对账：发起 mock-slow-first（首事件前等待 8s），2s 后本地断开。"
echo "随后查该请求的 execution.status 是否为 canceled。"
( chat mock-slow-first --max-time 2 ) ; echo "(client 已在 2s 超时断开)"
echo

echo "===================================================================="
echo "假设2/4 账本关联：查询最近的 requests 及其 executions（status/首字延迟/external_id/用量）。"
curl -sS -X POST "$BASE/admin/graphql" -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"query":"query{ requests(first:8, orderBy:{field:CREATED_AT,direction:DESC}){ edges{ node{ id status modelID channelID externalID metricsFirstTokenLatencyMs metricsLatencyMs executions{ edges{ node{ id status channelID externalID responseStatusCode errorMessage metricsFirstTokenLatencyMs metricsLatencyMs } } } usageLogs{ edges{ node{ totalTokens promptTokens completionTokens totalCost costPriceReferenceID promptCachedTokens } } } } } } }","variables":{}}' \
  | python3 -m json.tool 2>/dev/null || echo "(若上面 GraphQL 字段名报错，请贴回，我据你的版本修正查询)"
echo
echo "===================================================================="
echo "假设5 权限顺序 & 假设6 备选：见 ISSUE-001 文档，需配额/ccLoad 专门场景，"
echo "本脚本聚焦 1/2/3/4。全部输出请整体粘贴回对话。"
