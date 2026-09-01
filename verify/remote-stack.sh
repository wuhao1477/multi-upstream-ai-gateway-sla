#!/usr/bin/env bash
# 内网真库的只读界面验收（35 项）的栈包装：编前端 → 起 sla-core → 跑
# verify/ui/verify-remote.mjs → 拆。
#
# 用法：
#   DATABASE_URL='postgres://…@<internal-db-host>:5432/SLA_DB?sslmode=disable' \
#     ./verify/remote-stack.sh
#
# 为什么这份包装是必需的（不是省几行手工命令）：
#
# 这套验收此前**没有任何生命周期管理** —— 手起 `./bin/sla-core &`、手跑 node、
# 手 kill。2026-08-31 评估分支时发现 8-30 22:47 起的那个 core **一直挂着**占用
# 18390，没人收。泄漏本身还算小事，真正的问题是它让这套验收可以**跑在旧二进制
# 上并全绿**：脚本只连 `BASE`，从不校验答话的是谁。`test-arm-cloud.sh` 第 4 步
# 早就防了同一类假绿（注释原话：「会在本地有未推送提交时悄悄验到旧产物上，是
# 这类脚本最典型的假绿」），而这一侧没防。
#
# 于是本脚本做三件手工做不到的事：
#   1. trap 拆栈（同 8a7f5d7 的教训：cleanup 里一条命令都不许非零退出）；
#   2. 起栈前**拒绝**在已被占用的端口上跑，并打出占用者是谁 —— 而不是连上去；
#   3. 起栈后断言 `$PORT` 上听的 pid **就是本脚本刚起的那个**。
#
# 真库是只读的：本脚本不写任何测试行，迁移一致时 sla-core 也不动 schema
# （启动日志 applied:0 即证据）。DSN 由调用者给 —— 真凭证不写进仓库。
set -euo pipefail
cd "$(dirname "$0")/.."

PORT="${PORT:-18390}"
TOKEN="${ADMIN_TOKEN:-remote-verify-token}"
CORELOG="${CORELOG:-/tmp/sla-remote-core.log}"
TOTAL=5
STEP=0
step() { STEP=$((STEP + 1)); echo "── ${STEP}/${TOTAL} $1 ──"; }

CORE_PID=""; CLEANED=""
# ⚠️ 与 ui-stack.sh 同一条纪律：trap 处理函数里一律用 if 块，不用 `&&` 串 ——
#    set -e 在函数里同样生效，`[ -n "$X" ] && kill` 在 X 为空时整体返回 1，
#    函数就此中止，后面的清理根本不执行。本仓库为此泄漏过整栈容器（8a7f5d7）。
cleanup() {
  if [ -n "$CLEANED" ]; then return 0; fi
  CLEANED=1
  if [ -n "$CORE_PID" ]; then
    kill "$CORE_PID" 2>/dev/null || true
    wait "$CORE_PID" 2>/dev/null || true
  fi
  return 0
}
on_signal() { cleanup; exit 130; }
trap cleanup EXIT
trap on_signal INT TERM

command -v node >/dev/null || { echo "❌ 未找到 node"; exit 1; }
command -v pnpm >/dev/null || {
  echo "❌ 未找到 pnpm（编前端需要，可用 corepack enable pnpm）"; exit 1; }
[ -n "${DATABASE_URL:-}" ] || {
  echo "❌ 需要 DATABASE_URL 指向内网真库（真凭证不写进仓库）"
  echo "   用法：DATABASE_URL='postgres://…@<internal-db-host>:5432/SLA_DB?sslmode=disable' $0"
  exit 2; }

CHROME="${CHROME:-}"
if [ -z "$CHROME" ]; then
  for c in "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
           "$(command -v google-chrome || true)" \
           "$(command -v google-chrome-stable || true)"; do
    if [ -n "$c" ] && [ -x "$c" ]; then CHROME="$c"; break; fi
  done
fi
[ -n "$CHROME" ] && [ -x "$CHROME" ] || {
  echo "❌ 未找到 Chrome（设 CHROME=/path/to/chrome 可指定）"; exit 1; }
export CHROME

# ── 1/5 端口必须空闲 ──
#
# 这一步是本脚本存在的首要理由，所以它**拒绝**而不是复用：端口上已经有东西时，
# 若直接往下跑，验收会打在那个东西上 —— 它可能是上一轮泄漏的旧二进制，
# 而 35 项断言全都会绿。
step "端口 ${PORT} 必须空闲（防验收打在旧进程上）"
if lsof -nP -iTCP:"${PORT}" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "❌ ${PORT} 已被占用 —— 拒绝在它上面跑验收（会验到别人身上且全绿）"
  echo "   占用者："
  lsof -nP -iTCP:"${PORT}" -sTCP:LISTEN | tail -n +2 | sed 's/^/     /'
  echo "   确认是残留就 kill 掉它，或用 PORT=… 换端口。"
  exit 1
fi
echo "   ✅ 空闲"

# ── 2/5 前端 ──
# 顺序不能反：产物由 go:embed 打进二进制，先 go build 会把只有占位文件的空目录
# 编进去，症状是 /admin/ui 返回 500。
step "编前端（go:embed 的输入）"
make web >/dev/null
echo "   ✅ webdist 就绪"

# ── 3/5 起 core ──
step "起 sla-core（只读真库）"
go build -o bin/sla-core ./cmd/sla-core
ADMIN_TOKEN="$TOKEN" ./bin/sla-core -addr ":${PORT}" >"$CORELOG" 2>&1 &
CORE_PID=$!
ready=false
for _ in $(seq 1 40); do
  if [ "$(curl -s -o /dev/null -w '%{http_code}' \
        "http://127.0.0.1:${PORT}/healthz" 2>/dev/null)" = "200" ]; then
    ready=true; break
  fi
  sleep 1
done
$ready || { echo "❌ sla-core 未就绪"; tail -20 "$CORELOG"; exit 1; }
# 迁移一致性的证据留在日志里：applied:0 说明没动真库 schema。
grep -o '"msg":"迁移完成"[^}]*' "$CORELOG" | sed 's/^/   /' || true
echo "   ✅ sla-core 就绪（pid ${CORE_PID}）"

# ── 4/5 答话的必须是自己 ──
#
# 只探 /healthz 证明不了这件事：一个旧 core 同样回 200。所以直接问操作系统
# ${PORT} 上听的 pid 是谁，与 $CORE_PID 比。
step "确认 ${PORT} 上答话的就是刚起的那个 pid"
LISTEN_PID=$(lsof -nP -iTCP:"${PORT}" -sTCP:LISTEN -t 2>/dev/null | head -1 || true)
[ -n "$LISTEN_PID" ] || { echo "❌ ${PORT} 上没有 LISTEN —— core 起了但没听住"; exit 1; }
[ "$LISTEN_PID" = "$CORE_PID" ] || {
  echo "❌ ${PORT} 上听的是 pid ${LISTEN_PID}，不是本脚本起的 ${CORE_PID}"
  echo "   —— 验收会跑在别的进程上。这正是本脚本要防的假绿。"
  exit 1; }
echo "   ✅ pid ${LISTEN_PID} = 本脚本起的 core"

# ── 5/5 验收 ──
step "真库只读浏览器验收"
# ⚠️ 截图目录必须由**本脚本**建，与 ui-stack.sh:281 / test-arm-cloud.sh:237 同法。
#    verify-remote.mjs 只读 SHOTS 环境变量、不自建目录 —— 原先这行漏了，
#    靠 /tmp 下碰巧还留着上一轮的目录才没暴露。2026-09-01 那个目录被清掉后：
#    第 9 项的 page.screenshot 抛 ENOENT → 整轮在跑完 8 项后中断，
#    而收尾写 results.json 也 ENOENT，于是**报错指向 results.json，
#    真正断掉的是截图**。红得离原因两步远。
mkdir -p /tmp/sla-remote-shots
cd verify/ui
ADMIN_TOKEN="$TOKEN" BASE="http://127.0.0.1:${PORT}" node verify-remote.mjs
