#!/usr/bin/env bash
# 管理界面的本地栈：真 PG + 真 sla-core + **真上游**（探活选站，CLAUDE.md §1）。
#
# 两种模式，同一套栈：
#
#   ui-stack.sh          跑完自动验收后拆栈（原 test-ui.sh）——给 CI 与门禁判定用
#   ui-stack.sh --keep   起完停在前台等你手点（原 dev-ui.sh）——Ctrl-C 才拆
#
# 为什么合成一份：两个脚本共同前缀约 47 行（编前端、起 PG、探活选站、起 core、
# trap 清理），差别只有三处 —— 端口、结束时拆不拆、要不要 Chrome。分成两份的
# 代价不是那 47 行重复，而是**分叉**：解析 pick-upstream 输出的写法已经岔开了
# （一边 7 行复制粘贴 110 字符的 node -e，一边抽了 3 行 rd()），这类分叉会继续长。
#
# 两种模式的**端口故意错开**，可以同时跑（一边自动验收一边手点）。
#
# 需要：node + pnpm（编 web/ 前端）、Docker 或本地 postgres@16。
# --keep 不需要 Chrome（你用自己的浏览器点）；默认模式需要。
#
# 前端是独立的 Vite 工程（web/），产物由 go:embed 打进二进制 —— 所以本脚本
# 必须先编前端再 go build，顺序反了就会把只有占位文件的空目录编进二进制，
# 症状是 /admin/ui 返回 500「前端产物缺失」。
#
# 想边改前端边看效果的话，别用本脚本 —— 它起的是编好的静态产物，改一行要重跑。
# 用 `cd web && pnpm dev`（默认 :5173，带热更新），它的 /admin/* 请求会代理到
# SLA_DEV_BACKEND（默认 127.0.0.1:8080）；想指到 --keep 起的这套就
# SLA_DEV_BACKEND=http://127.0.0.1:18290 pnpm dev。
set -euo pipefail
cd "$(dirname "$0")/.."

KEEP=""
case "${1:-}" in
  --keep) KEEP=1 ;;
  "") ;;
  -h|--help) sed -n '2,27p' "$0"; exit 0 ;;
  *) echo "❌ 未知参数：$1（只认 --keep）"; exit 2 ;;
esac

# 模式相关的默认值。两套端口/容器名/数据目录/日志路径全部错开，
# 于是 --keep 起的栈与自动验收的栈互不干扰。
# PG 端口用 184xx 而非 554xx：后者在临时端口段里，会被出站连接借走
# （理由见 test-migrate.sh 顶部）。
if [ -n "$KEEP" ]; then
  PORT="${PORT:-18290}"; PGPORT="${PGPORT:-18442}"
  PGNAME=sladevpg; PREFIX=sla-dev; TOTAL=4
  # 固定令牌而非 $$：你要把它粘到界面里，每次都变就没法照着文档点。
  # 这是本地一次性栈，Ctrl-C 后数据库即销毁，不涉及任何真实凭证。
  TOKEN="${ADMIN_TOKEN:-dev-ui-token}"
else
  PORT="${PORT:-18190}"; PGPORT="${PGPORT:-18441}"
  PGNAME=slauipg; PREFIX=sla-ui; TOTAL=7
  TOKEN="ui-verify-$$"
fi
PGDATA="/tmp/${PREFIX}-pg"
CORELOG="/tmp/${PREFIX}-core.log"
WEBLOG="/tmp/${PREFIX}-web.log"
export LC_ALL=C LANG=C

STEP=0
step() { STEP=$((STEP + 1)); echo "── ${STEP}/${TOTAL} $1 ──"; }

CORE_PID=""; USE_DOCKER=""; CLEANED=""
# ⚠️ 这个函数里**一条命令都不许以非零退出**。它是 trap 的处理函数，而 set -e
#    在函数里同样生效：一条 `[ -n "$X" ] && echo` 在 X 为空时整体返回 1，函数就此
#    中止，后面的拆容器根本不执行 —— 于是每轮泄漏一整栈。本仓库已经踩过一次
#    （8a7f5d7「trap cleanup 静默失效，每轮泄漏整栈 5 个容器」），所以这里
#    一律用 if 块，不用 && 串。
cleanup() {
  if [ -n "$CLEANED" ]; then return 0; fi
  CLEANED=1
  if [ -n "$KEEP" ]; then echo ""; echo "── 清理 ──"; fi
  if [ -n "$CORE_PID" ]; then kill "$CORE_PID" 2>/dev/null || true; fi
  if [ -n "$USE_DOCKER" ]; then
    # ⚠️ `-v` 不可省。postgres 镜像自带 VOLUME 声明，`docker rm` 不带 -v
    #    只删容器、**留下匿名数据卷**。2026-08-31 清出 80 个这样的孤儿卷、
    #    共 4.1GB —— 全是本仓库四个验收脚本历次运行的残留。
    #    与 8a7f5d7 那次同类（清理没做干净），只是漏在卷这一层：
    #    容器层每轮都收干净了，所以 `docker ps` 一直是空的，看不出来。
    docker rm -f -v "$PGNAME" >/dev/null 2>&1 || true
    # 收干净了没有，当场验一次：本脚本起的容器不该留下任何卷。
    if [ -n "${VOL_BEFORE:-}" ]; then
      VOL_AFTER=$(docker volume ls -q 2>/dev/null | wc -l | tr -d ' ')
      if [ "$VOL_AFTER" -gt "$VOL_BEFORE" ]; then
        echo "   ⚠️ 卷泄漏：跑之前 ${VOL_BEFORE} 个，拆完 ${VOL_AFTER} 个"
        echo "      —— docker rm 少了 -v，或又多了一处起容器的地方"
      fi
    fi
  else
    # PGBIN 只在走本地 postgres 那条分支里才赋值；容器路径下它未定义，
    # 而 set -u 会让 "$PGBIN" 直接中止函数。用 ${PGBIN:-} 兜住。
    if [ -n "${PGBIN:-}" ]; then
      "$PGBIN/pg_ctl" -D "$PGDATA" stop >/dev/null 2>&1 || true
    fi
    rm -rf "$PGDATA"
  fi
  if [ -n "$KEEP" ]; then echo "   ✅ 已拆除（数据库一并销毁）"; fi
  return 0
}
# INT/TERM 单独接：--keep 模式下 Ctrl-C 是**正常退出方式**，
# 不能让它落到最后「sla-core 退出了」那条错误分支上（那会以 exit 1 收场，
# 而用户只是按了 Ctrl-C）。cleanup 的 CLEANED 幂等位就是为这里准备的：
# on_signal 调完 cleanup 后 exit，EXIT trap 还会再触发一次。
on_signal() { cleanup; exit 130; }
trap cleanup EXIT
trap on_signal INT TERM

# ── 前置检查：缺什么就明确说缺什么，不要跑一半才失败 ──
command -v node >/dev/null || { echo "❌ 未找到 node"; exit 1; }
command -v pnpm >/dev/null || {
  echo "❌ 未找到 pnpm（编前端需要，可用 corepack enable pnpm）"; exit 1; }

# 真上游来源（CLAUDE.md §1）。
#
# ⚠️ 两种模式对缺 HUB_FILE 的处理**故意不同**：
#    默认模式降级成"只跑免密的 SPA 那 17 项"——这是 CI 唯一能跑的形态，
#    真上游令牌不进 GitHub secrets（CLAUDE.md §1 的 CI 表），云上拿不到凭证。
#    降级必须**吵**，否则"CI 绿了"会被读成"功能验过了"，那正是本规则要防的假绿。
#    --keep 则直接拒绝：手点的全部意义就是对真站点点，没上游起来也没得点。
SPA_ONLY=""
if [ -z "${HUB_FILE:-}" ] || [ ! -f "${HUB_FILE:-}" ]; then
  if [ -n "$KEEP" ]; then
    echo "需要 HUB_FILE 指向 all-api-hub 导出 JSON（真上游来源，CLAUDE.md §1 禁止 mock）"
    echo "用法：HUB_FILE=/path/to/all-api-hub-backup.json $0 --keep"
    exit 2
  fi
  SPA_ONLY=1
fi

# Chrome 只有默认模式要（--keep 用你自己的浏览器）。
# 路径按平台探测：macOS 在 .app 里，Linux/CI 在 PATH 上。
if [ -z "$KEEP" ]; then
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
  [ -n "$CHROME" ] && [ -x "$CHROME" ] || {
    echo "❌ 未找到 Chrome（设 CHROME=/path/to/chrome 可指定）"; exit 1; }
  echo "   使用 Chrome: $CHROME"
fi

step "编前端（web/ → internal/admin/webdist，供 go:embed）"
# **每次都重编**，不做"产物已存在就跳过"的优化。
# 理由：这是验收脚本。复用一份可能与当前源码不一致的产物，等于让"全绿"变成
# 对旧代码的验收，而这种不一致完全静默 —— 恰恰是验收最该防的那类错误。
# 代价可接受：装依赖只在缺 node_modules 时跑，之后 build 含 lint 与类型检查约两秒。
[ -d web/node_modules ] || (cd web && pnpm install --frozen-lockfile --silent)
(cd web && pnpm run build >"$WEBLOG" 2>&1) || {
  echo "❌ 前端构建失败"; tail -30 "$WEBLOG"; exit 1; }
[ -f internal/admin/webdist/index.html ] || {
  echo "❌ 前端产物缺失：internal/admin/webdist/index.html"; exit 1; }
echo "   ✅ 前端产物就绪"

step "起 PostgreSQL"
if docker info >/dev/null 2>&1; then
  USE_DOCKER=1
  # 记下起容器前的卷数，cleanup 里拿它验"拆干净了没有"
  VOL_BEFORE=$(docker volume ls -q 2>/dev/null | wc -l | tr -d ' ')
  docker rm -f -v "$PGNAME" >/dev/null 2>&1 || true
  docker run -d --name "$PGNAME" -e POSTGRES_PASSWORD=x -e POSTGRES_DB=sla \
    -p "${PGPORT}:5432" postgres:16 >/dev/null
  for _ in $(seq 1 60); do
    docker exec "$PGNAME" pg_isready -U postgres >/dev/null 2>&1 && break
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
    -l "/tmp/${PREFIX}-pg.log" start >/dev/null
  sleep 3
  "$PGBIN/psql" -h 127.0.0.1 -p "$PGPORT" -U postgres -tAc "CREATE DATABASE sla" >/dev/null
  DSN="postgres://postgres@127.0.0.1:${PGPORT}/sla?sslmode=disable"
fi
echo "   ✅ PG 就绪（:${PGPORT}）"

step "探活选真上游（CLAUDE.md §1：不用假上游）"
# 从导出里现场探活挑一个能用的真站点。不写死 URL —— 站点会挂、会限流、
# 会换证书，写死等于把"它一定可用"这个假设又搬回来。
if [ -n "$SPA_ONLY" ]; then
  echo "   ⚠️ 未给 HUB_FILE —— 降级为**只跑 SPA 免密验收**"
  echo "      跳过的是功能与数据那 58 项（建渠道 / 探测站型 / 登记凭证 / 采集 /"
  echo "      分组 / 目录 / Key / 限流 / 批量导入试运行 / 改名停用启用）——它们要真上游凭证。"
  echo "      本地跑全量：HUB_FILE=~/Downloads/all-api-hub-backup-*.json $0"
  echo "      （CLAUDE.md §1：验不了就如实说验不了，不拿 mock 填绿）"
else
  UPJSON="$(HUB_FILE="$HUB_FILE" node verify/pick-upstream.mjs)" || {
    echo "❌ 没挑到可用的真上游（上面列了每个候选的失败原因）"; exit 1; }
  # 字段名走 argv 而不是拼进脚本字符串：拼字符串要穿 bash 引号再进 JS，
  # 两层转义的坑本仓库踩过（见 P1-evidence §5）。
  rd() { printf '%s' "$UPJSON" | node -e '
let s="";process.stdin.on("data",d=>s+=d).on("end",()=>
  process.stdout.write(String(JSON.parse(s)[process.argv[1]])))' "$1"; }
  UP_NAME="$(rd name)";     UP_URL="$(rd url)"
  UP_TOKEN="$(rd token)";   UP_UID="$(rd uid)"
  UP_KEYREF="$(rd keyRef)"; UP_QPU="$(rd quotaPerUnit)"
  UP_MODELS="$(rd models)"; UP_PER_CALL="$(rd perCallModels)"
  echo "   ✅ 选中 ${UP_NAME}"
fi

step "起 sla-core"
go build -o bin/sla-core ./cmd/sla-core
DATABASE_URL="$DSN" ADMIN_TOKEN="$TOKEN" ./bin/sla-core -addr ":${PORT}" \
  >"$CORELOG" 2>&1 &
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
echo "   ✅ sla-core 就绪"

# ── --keep：印出手点路线，停在前台 ──
#
# 人工点验不是自动验收的冗余。P1-evidence §4 第 15 项（先点采集再登记凭证，
# 本地失败也起算 60s 窗口）就是手点翻出来的 —— 当时脚本化验收全绿。
# 手点会走脚本不会走的路线，这条价值已经兑现过一次。
if [ -n "$KEEP" ]; then
  cat <<EOF

══════════════════════════════════════════════════════════
  管理界面   http://127.0.0.1:${PORT}/admin/ui
  管理令牌   ${TOKEN}          ← 粘到页面顶部「管理令牌」
  真上游     ${UP_URL}     ← 建渠道时填这个 base_url（${UP_NAME}）
  访问令牌   ${UP_TOKEN}
  用户 ID    ${UP_UID}          ← 登记凭证时填这两个，是真的
  Key 标识   ${UP_KEYREF}          ← external_ref，上游真实 token id

  手动验证路线（对应 P1-evidence §4 的缺陷 9~15）：
   1. 不填令牌就点「加载渠道」→ 应明确拒绝，而不是转圈或空列表
   2. 新建渠道，base_url 填上面那个真上游地址 → 站型应**自动探测**为 newapi，
      并读出 quota_per_unit（不是写死的 500000）
   3. 此时列表应主动报「尚未登记采集凭证」——渠道建好≠能采
      先别登记，直接点一次「立即采集」→ 应报缺凭证（422），
      且**不占限流窗口**：登记完能立刻采，不用等 60 秒（缺陷 15）
   4. 登记凭证（用上面打印的真令牌与用户 ID），再点「立即采集」
   5. 看采集结果表：P1 五项能力均有明确 status/support，
      degraded 项必须带说明，不支持项不应静默伪装为成功
   6. 展开「分组可用模型」→ 标题应是上游分组名（人家自己起的名字），
      不是内部 id「分组 2」
   7. 看模型目录：点「按次」分段 → 该段每行都应标「/次」且不混入「×倍率」。
      两种口径的数值区间重叠（"\$3.5/次" 与 "倍率 3.5"），所以必须先分段
      再比价；跨口径排在一起就是老 bug
   8. 60 秒内再点一次「立即采集」→ 应被限流拒绝并说还需等多少秒
   9. 改渠道名 / 停用渠道（停用要填原因）→ 列表状态与原因应立刻回显

  日志  core=${CORELOG}
  Ctrl-C 拆除（数据库一并销毁）
══════════════════════════════════════════════════════════
EOF
  # 停在前台等人工操作；子进程死了就退出，别留个假活着的壳
  while kill -0 "$CORE_PID" 2>/dev/null; do sleep 2; done
  echo "❌ sla-core 退出了，见 ${CORELOG}"
  tail -20 "$CORELOG"
  exit 1
fi

step "装浏览器验收依赖"
(cd verify/ui && pnpm install --frozen-lockfile --silent >/dev/null 2>&1)
echo "   ✅ 依赖就绪"

step "真 Chrome 验收"
# 两份脚本分工：verify-ui.mjs 验功能与数据，verify-spa.mjs 验前端工程化后
# 新增的那几条性质（history 路由刷新、900px 断点、静态资源缓存与占位文件不可取）。
# 后者先跑：它不写库、几秒钟出结果，路由挂了的话功能验收全都白跑。
mkdir -p /tmp/sla-ui-shots
# 批量导入试运行同样用同一份导出（只 dry_run，不落库）。它会真的去探测备份里的
# 上百个陌生站点（实测 106 站 24 秒）—— 慢，但那是真实结果。
if [ -z "$SPA_ONLY" ]; then
  echo "   （真上游：$UP_URL / 批量导入试运行：$HUB_FILE）"
fi
cd verify/ui

BASE="http://127.0.0.1:${PORT}" node verify-spa.mjs

if [ -n "$SPA_ONLY" ]; then
  echo ""
  echo "=========================================================="
  echo "⚠️  只跑了 SPA 免密验收（17 项）。功能与数据那 58 项**未验**。"
  echo "    原因：无 HUB_FILE，拿不到真上游凭证；令牌不进 GitHub secrets。"
  echo "    这不等于功能通过 —— 全量结论只能来自本地跑。"
  echo "=========================================================="
  exit 0
fi

# 这把假 Key 的明文由这里给，verify-ui.mjs 读同一个值 —— 它断言 DOM 里没有,
# 下面最后一步断言 core 日志里也没有。两处必须是同一个字面量，否则日志那条会
# 变成"grep 一个谁都不会写进日志的字符串"，永远绿。
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

# ── Key 明文不得进日志（P1 退出标准③）──
#
# 退出标准③ 的原文是"Key 明文在**响应/日志/抓包**中一处都不出现"。
# verify-ui.mjs 覆盖了响应那一端（整份 DOM grep）；抓包那一端等价于响应体
# （容器网内是明文 HTTP，包体就是响应体）；**日志那一端此前没有任何断言** ——
# 2026-08-29 评估分支完成度时发现，补在这里。
#
# 为什么放在 shell 而不是 mjs：日志是 sla-core 的 stdout，只有起进程的这一侧
# 看得到；浏览器里取不到。
#
# 注意此刻 cwd 是 verify/ui（上面 cd 过去跑 node），下面只用绝对路径。
step "Key 明文不进日志"
if grep -qF "$UI_KEY_SECRET" "$CORELOG"; then
  echo "❌ core 日志里出现了 Key 明文 —— 违反 FR-094 与 P1 退出标准③"
  echo "   命中行（已截断，不打完整明文）："
  grep -nF "$UI_KEY_SECRET" "$CORELOG" | head -3 | cut -c1-60
  exit 1
fi
# 反向自检：这个 grep 必须真的能在该文件里找到东西，否则"没找到明文"可能只是
# 因为日志是空的、或路径写错了 —— 那样这条断言永远绿，等于没有。
if [ ! -s "$CORELOG" ]; then
  echo "❌ ${CORELOG} 是空的 —— 上面那条「日志无明文」是空断言"
  exit 1
fi
echo "   ✅ core 日志无 Key 明文（日志 $(wc -l < "$CORELOG" | tr -d ' ') 行，非空）"
