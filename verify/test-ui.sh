#!/usr/bin/env bash
# P1 管理界面的真实浏览器验收（#9/#10/#12 的可执行判定）。
#
# 起真 PG + 真 sla-core + mock 上游，再用**真 Chrome** 点击/填表/截图。
# 与 test-config-api.sh（curl 打接口）的区别：这里验的是"运维真能在 web 端
# 加渠道商并采集"，而不是"接口返回了正确 JSON"。
#
# 需要：Chrome、node、以及 Docker 或本地 postgres@16。
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

echo "── 1/5 起 PostgreSQL ──"
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

echo "── 2/5 起 mock 上游（NewAPI 系）──"
python3 verify/mock_newapi.py "$MOCKPORT" >/tmp/sla-ui-mock.log 2>&1 &
MOCK_PID=$!
for _ in $(seq 1 20); do
  curl -sf "http://127.0.0.1:${MOCKPORT}/api/status" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -sf "http://127.0.0.1:${MOCKPORT}/api/status" >/dev/null || {
  echo "❌ mock 上游未就绪"; cat /tmp/sla-ui-mock.log; exit 1; }
echo "   ✅ mock 就绪"

echo "── 3/5 起 sla-core ──"
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

echo "── 4/5 装浏览器验收依赖 ──"
(cd verify/ui && npm install --silent --no-audit --no-fund >/dev/null 2>&1)
echo "   ✅ 依赖就绪"

echo "── 5/5 真 Chrome 验收 ──"
mkdir -p /tmp/sla-ui-shots
cd verify/ui
BASE="http://127.0.0.1:${PORT}" \
ADMIN_TOKEN="$TOKEN" \
MOCK="http://127.0.0.1:${MOCKPORT}" \
SHOTS=/tmp/sla-ui-shots \
  node verify-ui.mjs
