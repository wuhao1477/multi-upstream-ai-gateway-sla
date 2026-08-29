#!/usr/bin/env bash
# P1 管理界面的真实浏览器验收（#9/#10/#12 的可执行判定）。
#
# 起真 PG + 真 sla-core + mock 上游，再用**真 Chrome** 点击/填表/截图。
# 与 test-config-api.sh（curl 打接口）的区别：这里验的是"运维真能在 web 端
# 加渠道商并采集"，而不是"接口返回了正确 JSON"。
#
# 需要：Chrome、node（含 npm，用于编 web/ 前端）、以及 Docker 或本地 postgres@16。
#
# 前端是独立的 Vite 工程（web/），产物由 go:embed 打进二进制 —— 所以本脚本
# 必须先编前端再 go build，顺序反了就会把只有占位文件的空目录编进二进制，
# 症状是 /admin/ui 返回 500「前端产物缺失」。
set -euo pipefail
cd "$(dirname "$0")/.."

# Chrome 路径按平台探测：macOS 在 .app 里，Linux/CI 在 PATH 上
CHROME="${CHROME:-}"
if [ -z "$CHROME" ]; then
  for c in "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
           "$(command -v google-chrome || true)" \
           "$(command -v google-chrome-stable || true)" \
           "$(command -v chromium || true)"; do
    [ -n "$c" ] && [ -x "$c" ] && CHROME="$c" && break
  done
fi
export CHROME
PORT=18190
MOCKPORT=18199
PGPORT=55441
TOKEN="ui-verify-$$"
PGDATA=/tmp/sla-ui-pg
export LC_ALL=C LANG=C

CORE_PID=""; MOCK_PID=""; USE_DOCKER=""
cleanup() {
  [ -n "$CORE_PID" ] && kill "$CORE_PID" 2>/dev/null || true
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  if [ -n "$USE_DOCKER" ]; then
    docker rm -f slauipg >/dev/null 2>&1 || true
  else
    "$PGBIN/pg_ctl" -D "$PGDATA" stop >/dev/null 2>&1 || true
    rm -rf "$PGDATA"
  fi
}
trap cleanup EXIT

# ── 前置检查：缺什么就明确说缺什么，不要跑一半才失败 ──
[ -n "$CHROME" ] && [ -x "$CHROME" ] || {
  echo "❌ 未找到 Chrome（设 CHROME=/path/to/chrome 可指定）"; exit 1; }
echo "   使用 Chrome: $CHROME"
command -v node >/dev/null || { echo "❌ 未找到 node"; exit 1; }

echo "── 1/6 编前端（web/ → internal/admin/webdist，供 go:embed）──"
# **每次都重编**，不做"产物已存在就跳过"的优化。
# 理由：这是验收脚本。复用一份可能与当前源码不一致的产物，等于让"43 项全绿"
# 变成对旧代码的验收，而这种不一致完全静默 —— 恰恰是验收最该防的那类错误。
# 代价可接受：npm ci 只在缺 node_modules 时跑，之后 build 含类型检查约两秒。
[ -d web/node_modules ] || (cd web && npm ci --silent --no-audit --no-fund)
(cd web && npm run build >/tmp/sla-ui-web.log 2>&1) || {
  echo "❌ 前端构建失败"; tail -30 /tmp/sla-ui-web.log; exit 1; }
[ -f internal/admin/webdist/index.html ] || {
  echo "❌ 前端产物缺失：internal/admin/webdist/index.html"; exit 1; }
echo "   ✅ 前端产物就绪"

echo "── 2/6 起 PostgreSQL ──"
if docker info >/dev/null 2>&1; then
  USE_DOCKER=1
  docker rm -f slauipg >/dev/null 2>&1 || true
  docker run -d --name slauipg -e POSTGRES_PASSWORD=x -e POSTGRES_DB=sla \
    -p "${PGPORT}:5432" postgres:16 >/dev/null
  for _ in $(seq 1 60); do
    docker exec slauipg pg_isready -U postgres >/dev/null 2>&1 && break
    sleep 1
  done
  sleep 2
  DSN="postgres://postgres:x@127.0.0.1:${PGPORT}/sla?sslmode=disable"
else
  PGBIN=/opt/homebrew/opt/postgresql@16/bin
  [ -x "$PGBIN/initdb" ] || {
    echo "❌ 既无 Docker 也无 postgresql@16（brew install postgresql@16）"; exit 1; }
  rm -rf "$PGDATA"
  "$PGBIN/initdb" -D "$PGDATA" -U postgres --encoding=UTF8 --locale=C >/dev/null 2>&1
  "$PGBIN/pg_ctl" -D "$PGDATA" \
    -o "-p $PGPORT -k /tmp -c listen_addresses=127.0.0.1" \
    -l /tmp/sla-ui-pg.log start >/dev/null
  sleep 3
  "$PGBIN/psql" -h 127.0.0.1 -p "$PGPORT" -U postgres -tAc "CREATE DATABASE sla" >/dev/null
  DSN="postgres://postgres@127.0.0.1:${PGPORT}/sla?sslmode=disable"
fi
echo "   ✅ PG 就绪"

echo "── 3/6 起 mock 上游（NewAPI 系）──"
python3 verify/mock_newapi.py "$MOCKPORT" >/tmp/sla-ui-mock.log 2>&1 &
MOCK_PID=$!
for _ in $(seq 1 20); do
  curl -sf "http://127.0.0.1:${MOCKPORT}/api/status" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -sf "http://127.0.0.1:${MOCKPORT}/api/status" >/dev/null || {
  echo "❌ mock 上游未就绪"; cat /tmp/sla-ui-mock.log; exit 1; }
echo "   ✅ mock 就绪"

echo "── 4/6 起 sla-core ──"
go build -o bin/sla-core ./cmd/sla-core
DATABASE_URL="$DSN" ADMIN_TOKEN="$TOKEN" ./bin/sla-core -addr ":${PORT}" \
  >/tmp/sla-ui-core.log 2>&1 &
CORE_PID=$!
ready=false
for _ in $(seq 1 40); do
  if [ "$(curl -s -o /dev/null -w '%{http_code}' \
        "http://127.0.0.1:${PORT}/healthz" 2>/dev/null)" = "200" ]; then
    ready=true; break
  fi
  sleep 1
done
$ready || { echo "❌ sla-core 未就绪"; tail -20 /tmp/sla-ui-core.log; exit 1; }
echo "   ✅ sla-core 就绪"

echo "── 5/6 装浏览器验收依赖 ──"
(cd verify/ui && npm install --silent --no-audit --no-fund >/dev/null 2>&1)
echo "   ✅ 依赖就绪"

echo "── 6/6 真 Chrome 验收 ──"
# 两份脚本分工：verify-ui.mjs 验功能与数据，verify-spa.mjs 验前端工程化后
# 新增的那几条性质（history 路由刷新、900px 断点、静态资源缓存与占位文件不可取）。
# 后者先跑：它不写库、几秒钟出结果，路由挂了的话功能验收全都白跑。
mkdir -p /tmp/sla-ui-shots
# HUB_FILE：给一份 all-api-hub 导出文件，就额外跑一遍批量导入试运行
#（只 dry_run，不落库）。默认不跑 —— 那一段会真的去探测备份里的上百个陌生
# 站点（实测 106 站 24 秒），不该出现在每次例行验收里。
# 用法：HUB_FILE=~/Downloads/all-api-hub-backup-*.json verify/test-ui.sh
[ -n "${HUB_FILE:-}" ] && echo "   （含 all-api-hub 试运行：$HUB_FILE）"
cd verify/ui

BASE="http://127.0.0.1:${PORT}" node verify-spa.mjs

BASE="http://127.0.0.1:${PORT}" \
ADMIN_TOKEN="$TOKEN" \
MOCK="http://127.0.0.1:${MOCKPORT}" \
SHOTS=/tmp/sla-ui-shots \
HUB_FILE="${HUB_FILE:-}" \
  node verify-ui.mjs
