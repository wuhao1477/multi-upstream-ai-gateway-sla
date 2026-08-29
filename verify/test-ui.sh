#!/usr/bin/env bash
# P1 管理界面的真实浏览器验收（#9/#10/#12 的可执行判定）。
#
# 起真 PG + 真 sla-core + **真上游**（探活选站，CLAUDE.md §1），再用真 Chrome 点。
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
PGPORT=55441
TOKEN="ui-verify-$$"
PGDATA=/tmp/sla-ui-pg
export LC_ALL=C LANG=C

CORE_PID=""; USE_DOCKER=""
cleanup() {
  [ -n "$CORE_PID" ] && kill "$CORE_PID" 2>/dev/null || true
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
# 代价可接受：装依赖只在缺 node_modules 时跑，之后 build 含 lint 与类型检查约两秒。
[ -d web/node_modules ] || (cd web && pnpm install --frozen-lockfile --silent)
(cd web && pnpm run build >/tmp/sla-ui-web.log 2>&1) || {
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

echo "── 3/6 选一个真上游（CLAUDE.md §1：不用假上游）──"
# 从导出里现场探活挑一个能用的真站点。不写死 URL —— 站点会挂、会限流、
# 会换证书,写死等于把"它一定可用"这个假设又搬回来。
#
# ⚠️ HUB_FILE 缺失时**不报错退出**，而是降级成"只跑免密的 SPA 那 14 项"。
#    这是 CI 唯一能跑的形态：真上游令牌不进 GitHub secrets（CLAUDE.md §1 的
#    CI 表），所以云上拿不到凭证。降级必须**吵**——把跳过了什么、为什么跳过
#    打出来，否则"CI 绿了"会被读成"功能验过了"，那正是本规则要防的假绿。
SPA_ONLY=""
if [ -z "${HUB_FILE:-}" ]; then
  SPA_ONLY=1
  echo "   ⚠️ 未给 HUB_FILE —— 降级为**只跑 SPA 免密验收**"
  echo "      跳过的是功能与数据那 51 项（建渠道 / 探测站型 / 登记凭证 / 采集 /"
  echo "      分组 / 目录 / Key / 限流 / 批量导入试运行）——它们要真上游凭证。"
  echo "      本地跑全量：HUB_FILE=~/Downloads/all-api-hub-backup-*.json $0"
  echo "      （CLAUDE.md §1：验不了就如实说验不了，不拿 mock 填绿）"
elif ! UPJSON=$(HUB_FILE="$HUB_FILE" node verify/pick-upstream.mjs); then
  echo "❌ 没挑到可用的真上游（上面列了每个候选的失败原因）"; exit 1
fi
if [ -z "$SPA_ONLY" ]; then
UP_URL=$(printf '%s' "$UPJSON" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>process.stdout.write(JSON.parse(s).url))')
UP_TOKEN=$(printf '%s' "$UPJSON" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>process.stdout.write(JSON.parse(s).token))')
UP_UID=$(printf '%s' "$UPJSON" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>process.stdout.write(JSON.parse(s).uid))')
UP_QPU=$(printf '%s' "$UPJSON" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>process.stdout.write(String(JSON.parse(s).quotaPerUnit)))')
UP_MODELS=$(printf '%s' "$UPJSON" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>process.stdout.write(String(JSON.parse(s).models)))')
UP_PER_CALL=$(printf '%s' "$UPJSON" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>process.stdout.write(String(JSON.parse(s).perCallModels)))')
UP_KEYREF=$(printf '%s' "$UPJSON" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>process.stdout.write(JSON.parse(s).keyRef))')
fi

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
(cd verify/ui && pnpm install --frozen-lockfile --silent >/dev/null 2>&1)
echo "   ✅ 依赖就绪"

echo "── 6/6 真 Chrome 验收 ──"
# 两份脚本分工：verify-ui.mjs 验功能与数据，verify-spa.mjs 验前端工程化后
# 新增的那几条性质（history 路由刷新、900px 断点、静态资源缓存与占位文件不可取）。
# 后者先跑：它不写库、几秒钟出结果，路由挂了的话功能验收全都白跑。
mkdir -p /tmp/sla-ui-shots
# 批量导入试运行同样用同一份导出（只 dry_run,不落库）。它会真的去探测备份里的
# 上百个陌生站点（实测 106 站 24 秒）—— 慢,但那是真实结果。
if [ -z "$SPA_ONLY" ]; then
  echo "   （真上游：$UP_URL / 批量导入试运行：$HUB_FILE）"
fi
cd verify/ui

BASE="http://127.0.0.1:${PORT}" node verify-spa.mjs

if [ -n "$SPA_ONLY" ]; then
  echo ""
  echo "=========================================================="
  echo "⚠️  只跑了 SPA 免密验收（14 项）。功能与数据那 51 项**未验**。"
  echo "    原因：无 HUB_FILE，拿不到真上游凭证；令牌不进 GitHub secrets。"
  echo "    这不等于功能通过 —— 全量结论只能来自本地跑。"
  echo "=========================================================="
  exit 0
fi

# 这把假 Key 的明文由这里给,verify-ui.mjs 读同一个值 —— 它断言 DOM 里没有,
# 下面第 7 步断言 core 日志里也没有。两处必须是同一个字面量,否则日志那条会
# 变成"grep 一个谁都不会写进日志的字符串",永远绿。
UI_KEY_SECRET='sk-ui-secret-should-never-be-echoed-9f3a'

BASE="http://127.0.0.1:${PORT}" \
ADMIN_TOKEN="$TOKEN" \
UP_URL="$UP_URL" \
UP_TOKEN="$UP_TOKEN" \
UP_UID="$UP_UID" \
UP_QPU="$UP_QPU" \
UP_MODELS="$UP_MODELS" \
UP_PER_CALL="$UP_PER_CALL" \
UP_KEYREF="$UP_KEYREF" \
UI_KEY_SECRET="$UI_KEY_SECRET" \
SHOTS=/tmp/sla-ui-shots \
HUB_FILE="$HUB_FILE" \
  node verify-ui.mjs

# ── 7/7 Key 明文不得进日志（P1 退出标准③）──
#
# 退出标准③ 的原文是"Key 明文在**响应/日志/抓包**中一处都不出现"。
# verify-ui.mjs 覆盖了响应那一端（整份 DOM grep）；抓包那一端等价于响应体
# （容器网内是明文 HTTP，包体就是响应体）；**日志那一端此前没有任何断言** ——
# 2026-08-29 评估分支完成度时发现，补在这里。
#
# 为什么放在 shell 而不是 mjs：日志是 sla-core 的 stdout，只有起进程的这一侧
# 看得到；浏览器里取不到。
#
# 注意此刻 cwd 是 verify/ui（上面 cd 过去跑 node），但下面只用绝对路径，
# 不需要 cd 回去 —— 脚本里没有 $ROOT 这个变量，写 cd "$ROOT" 会在 set -u 下直接中止。
echo "── 7/7 Key 明文不进日志 ──"
if grep -qF "$UI_KEY_SECRET" /tmp/sla-ui-core.log; then
  echo "❌ core 日志里出现了 Key 明文 —— 违反 FR-094 与 P1 退出标准③"
  echo "   命中行（已截断，不打完整明文）："
  grep -nF "$UI_KEY_SECRET" /tmp/sla-ui-core.log | head -3 | cut -c1-60
  exit 1
fi
# 反向自检：这个 grep 必须真的能在该文件里找到东西，否则"没找到明文"可能只是
# 因为日志是空的、或路径写错了 —— 那样这条断言永远绿，等于没有。
if [ ! -s /tmp/sla-ui-core.log ]; then
  echo "❌ /tmp/sla-ui-core.log 是空的 —— 上面那条「日志无明文」是空断言"
  exit 1
fi
echo "   ✅ core 日志无 Key 明文（日志 $(wc -l < /tmp/sla-ui-core.log | tr -d ' ') 行，非空）"
