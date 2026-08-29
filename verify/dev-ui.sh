#!/usr/bin/env bash
# 起一套**常驻**的本地栈供人工点验管理界面（PG + sla-core + mock 上游）。
#
# 与 test-ui.sh 的区别：那个脚本 trap EXIT 会把三个进程连数据目录一起拆掉，
# 跑完什么都不剩 —— 它是给 CI 判定用的。这里起完就停在前台等你，
# Ctrl-C 才清理，于是你能在浏览器里自己加渠道、登记凭证、点采集。
#
# 端口与 test-ui.sh **故意错开**，两者可同时跑（一边自动验收一边手点）。
#
# 需要：Docker 或本地 postgres@16。不需要 Chrome/node（你用自己的浏览器）。
set -euo pipefail
cd "$(dirname "$0")/.."

PORT="${PORT:-18290}"
MOCKPORT="${MOCKPORT:-18299}"
PGPORT="${PGPORT:-55442}"
# 固定令牌而非 $$：你要把它粘到界面里，每次都变就没法照着文档点。
# 这是本地一次性栈，Ctrl-C 后数据库即销毁，不涉及任何真实凭证。
TOKEN="${ADMIN_TOKEN:-dev-ui-token}"
PGDATA=/tmp/sla-dev-pg
export LC_ALL=C LANG=C

CORE_PID=""; MOCK_PID=""; USE_DOCKER=""
cleanup() {
  echo ""
  echo "── 清理 ──"
  [ -n "$CORE_PID" ] && kill "$CORE_PID" 2>/dev/null || true
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  if [ -n "$USE_DOCKER" ]; then
    docker rm -f sladevpg >/dev/null 2>&1 || true
  else
    "$PGBIN/pg_ctl" -D "$PGDATA" stop >/dev/null 2>&1 || true
    rm -rf "$PGDATA"
  fi
  echo "   ✅ 已拆除（数据库一并销毁）"
}
trap cleanup EXIT INT TERM

echo "── 1/3 起 PostgreSQL ──"
if docker info >/dev/null 2>&1; then
  USE_DOCKER=1
  docker rm -f sladevpg >/dev/null 2>&1 || true
  docker run -d --name sladevpg -e POSTGRES_PASSWORD=x -e POSTGRES_DB=sla \
    -p "${PGPORT}:5432" postgres:16 >/dev/null
  for _ in $(seq 1 60); do
    docker exec sladevpg pg_isready -U postgres >/dev/null 2>&1 && break
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
    -l /tmp/sla-dev-pg.log start >/dev/null
  sleep 3
  "$PGBIN/psql" -h 127.0.0.1 -p "$PGPORT" -U postgres -tAc "CREATE DATABASE sla" >/dev/null
  DSN="postgres://postgres@127.0.0.1:${PGPORT}/sla?sslmode=disable"
fi
echo "   ✅ PG 就绪（:${PGPORT}）"

echo "── 2/3 起 mock 上游（NewAPI 系）──"
python3 verify/mock_newapi.py "$MOCKPORT" >/tmp/sla-dev-mock.log 2>&1 &
MOCK_PID=$!
for _ in $(seq 1 20); do
  curl -sf "http://127.0.0.1:${MOCKPORT}/api/status" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -sf "http://127.0.0.1:${MOCKPORT}/api/status" >/dev/null || {
  echo "❌ mock 上游未就绪"; cat /tmp/sla-dev-mock.log; exit 1; }
echo "   ✅ mock 就绪（:${MOCKPORT}）"

echo "── 3/3 起 sla-core ──"
go build -o bin/sla-core ./cmd/sla-core
DATABASE_URL="$DSN" ADMIN_TOKEN="$TOKEN" ./bin/sla-core -addr ":${PORT}" \
  >/tmp/sla-dev-core.log 2>&1 &
CORE_PID=$!
ready=false
for _ in $(seq 1 40); do
  if [ "$(curl -s -o /dev/null -w '%{http_code}' \
        "http://127.0.0.1:${PORT}/healthz" 2>/dev/null)" = "200" ]; then
    ready=true; break
  fi
  sleep 1
done
$ready || { echo "❌ sla-core 未就绪"; tail -20 /tmp/sla-dev-core.log; exit 1; }
echo "   ✅ sla-core 就绪"

cat <<EOF

══════════════════════════════════════════════════════════
  管理界面   http://127.0.0.1:${PORT}/admin/ui
  管理令牌   ${TOKEN}          ← 粘到页面顶部「管理令牌」
  mock 上游  http://127.0.0.1:${MOCKPORT}     ← 建渠道时填这个 base_url

  手动验证路线（对应 P1-evidence §4 的缺陷 9~15）：
   1. 不填令牌就点「加载渠道」→ 应明确拒绝，而不是转圈或空列表
   2. 新建渠道，base_url 填 mock 地址 → 站型应**自动探测**为 newapi，
      并读出 quota_per_unit（不是写死的 500000）
   3. 此时列表应主动报「尚未登记采集凭证」——渠道建好≠能采
      先别登记，直接点一次「立即采集」→ 应报缺凭证（422），
      且**不占限流窗口**：登记完能立刻采，不用等 60 秒（缺陷 15）
   4. 登记凭证（访问令牌随便填），再点「立即采集」
   5. 看采集结果表：subscription_quotas 应显式标 unsupported，
      而不是静默跳过或报 ok
   6. 展开「分组可用模型」→ 标题应是上游分组名（default/vip/svip），
      不是内部 id「分组 2」
   7. 看模型目录：kling-video-pro 应标「/次」且排在所有「×倍率」行**之后**；
      若它插在 claude-4-sonnet(6) 前面就是跨口径比价的老 bug
   8. 60 秒内再点一次「立即采集」→ 应被限流拒绝并说还需等多少秒

  日志  core=/tmp/sla-dev-core.log  mock=/tmp/sla-dev-mock.log
  Ctrl-C 拆除（数据库一并销毁）
══════════════════════════════════════════════════════════

EOF

# 停在前台等人工操作；子进程死了就退出，别留个假活着的壳
while kill -0 "$CORE_PID" 2>/dev/null; do sleep 2; done
echo "❌ sla-core 退出了，见 /tmp/sla-dev-core.log"
tail -20 /tmp/sla-dev-core.log
