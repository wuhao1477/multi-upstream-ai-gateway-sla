#!/usr/bin/env bash
# 云端 arm64 产物的本地真机验收：把 GitHub Actions 构建的镜像拉回本地，
# 起真 Docker 栈，再用真 Chrome 点管理界面。
#
# 本脚本要证明的**不是**"代码能跑"（test-ui.sh 已经证过），而是：
#   1. 云端交叉编译出的 arm64 镜像里，装的确实是 arm64 二进制；
#   2. 跑起来的容器确实来自当前 HEAD 那次云端构建（版本号比对）；
#   3. 该镜像在 arm64 上完成迁移选主、健康检查、以及 web 端加渠道商 + 采集。
#
# 因此有两条铁律，改脚本时别破坏：
#   · 绝不本地 docker build 兜底 —— 找不到云端产物就失败退出。
#     一旦兜底，全绿也证明不了任何与"云端 arm 构建"有关的事。
#   · 架构断言读 ELF，不读 image config 的 architecture 字段（理由见
#     verify/assert_image_arch.py 的模块注释）。
#
# 需要：gh（已登录，repo scope 足够）、docker（arm64 宿主）、node、Chrome。
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
export LC_ALL=C LANG=C

# ⚠️ CLICOLOR_FORCE=1（某些 shell 配置会全局设它）会让 gh 即使输出到**管道**
#    也注入 ANSI 色码，于是 JSON 解析当场失败，报
#    "Invalid numeric literal at line 1, column 2" —— 看着像 gh 没返回 JSON，
#    实际是返回了带色的 JSON。NO_COLOR / GH_NO_COLOR 都压不住它（实测），
#    只能 unset。下面统一用 gh 内置 --jq 取值，顺带不依赖外部 jq。
unset CLICOLOR_FORCE

# 真上游从这份导出里探活选取（CLAUDE.md §1：不许 mock）。必填 ——
# 没有它就没有可采集的上游，第 8 步的 Detect/采集全都验不了。
HUB_FILE="${HUB_FILE:-}"
if [ -z "$HUB_FILE" ] || [ ! -f "$HUB_FILE" ]; then
  echo "需要 HUB_FILE 指向 all-api-hub 导出 JSON（真上游来源，CLAUDE.md §1 禁止 mock）"
  echo "用法：HUB_FILE=/path/to/all-api-hub-backup.json $0"
  exit 2
fi

WORKFLOW=build-arm.yml
PORT="${CORE_PORT:-18290}"
TOKEN="arm-verify-$$"
WORKDIR=/tmp/sla-arm-verify
# ⚠️ compose 文件必须用**绝对路径**：最后一步要在 verify/ui 里跑 node，
#    相对路径在那之后就解析不到了，而 cleanup 里的失败被 `|| true` 吞掉 →
#    trap 静默失效，每跑一轮泄漏整栈 5 个容器（含占着 18290 端口的那个），
#    下一轮起栈直接端口冲突。抓到过一次，别改回相对路径。
COMPOSE=(docker compose -f "$ROOT/verify/docker-compose.arm.yml")

cleanup() {
  # down -v 而非 stop：postgres 用 tmpfs 不留数据，但容器与网络要收干净，
  # 否则下一轮 compose up 撞同名容器。
  SLA_IMAGE="${SLA_IMAGE:-none}" ADMIN_TOKEN="$TOKEN" \
    "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ── 0/8 前置：缺什么当场说清，别跑一半才炸 ──
echo "── 0/8 前置检查 ──"
HOST_ARCH=$(uname -m)
[ "$HOST_ARCH" = "arm64" ] || [ "$HOST_ARCH" = "aarch64" ] || {
  echo "❌ 宿主机是 $HOST_ARCH，不是 arm64。本验收要求在 arm64 上真跑 arm64 镜像"
  echo "   （在 amd64 上跑会落到 QEMU 模拟，证明不了产物在真 arm 上可用）"
  exit 1; }
command -v gh >/dev/null || { echo "❌ 未找到 gh CLI"; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "❌ gh 未登录（gh auth login）"; exit 1; }
docker info >/dev/null 2>&1 || {
  echo "❌ Docker 不可用。若装了 OrbStack：open -a OrbStack && docker context use orbstack"
  exit 1; }
command -v node >/dev/null || { echo "❌ 未找到 node"; exit 1; }

CHROME="${CHROME:-}"
if [ -z "$CHROME" ]; then
  for c in "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
           "$(command -v google-chrome || true)" \
           "$(command -v google-chrome-stable || true)" \
           "$(command -v chromium || true)"; do
    [ -n "$c" ] && [ -x "$c" ] && CHROME="$c" && break
  done
fi
[ -n "$CHROME" ] && [ -x "$CHROME" ] || {
  echo "❌ 未找到 Chrome（设 CHROME=/path/to/chrome 可指定）"; exit 1; }
export CHROME
echo "   宿主机 $HOST_ARCH / docker $(docker version -f '{{.Server.Arch}}' 2>/dev/null)"
echo "   Chrome: $CHROME"

# ── 1/8 定位当前 HEAD 对应的云端构建 ──
# 用 HEAD 的完整 SHA 去筛 run，而不是"取最新一次成功的 run"：
# 后者会在本地有未推送提交时悄悄验到旧产物上，是这类脚本最典型的假绿。
echo "── 1/8 定位云端构建（HEAD = $(git rev-parse --short HEAD)）──"
SHA=$(git rev-parse HEAD)
SHA7=$(echo "$SHA" | cut -c1-7)

git diff --quiet && git diff --cached --quiet || {
  echo "⚠️  工作区有未提交改动 —— 云端构建的是 $SHA7，本地代码与之不同。"
  echo "    验收对象仍是云端产物（这是本脚本的目的），但别据此断言未提交的改动。"; }

# 用 gh 内置 --jq 直出制表符分隔的三元组，避免二次解析
find_run() {
  gh run list --workflow "$WORKFLOW" --commit "$SHA" --limit 1 \
    --json databaseId,status,conclusion,url \
    --jq '.[0] | [.databaseId, .status, (.conclusion // "-"), .url] | @tsv' 2>/dev/null
}
IFS=$'\t' read -r RUN_ID ST CONC RUN_URL <<<"$(find_run)"

# ⚠️ 必须校验 RUN_ID 是数字，不能只判输出串非空：无匹配 run 时 gh 返回空数组，
#    `.[0]` 求值得 null，`@tsv` 把 null 渲染成**空字段**，于是拿到 "\t\t-\t"
#    —— 非空串，判空会通过，脚本会带着空 RUN_ID 进入等待循环，把
#    "没有这次构建" 显示成 "云端状态 ，等待中…" 转 20 分钟。
case "$RUN_ID" in
  ''|*[!0-9]*)
    echo "❌ 没有找到 commit $SHA7 对应的 $WORKFLOW 运行记录。"
    echo "   先推送当前分支触发云端构建："
    echo "     git push -u origin $(git branch --show-current)"
    echo "   （刻意不本地兜底构建：那样验收就与「云端 arm 构建」无关了）"
    exit 1;;
esac
echo "   run #$RUN_ID  $RUN_URL"

# 云端还在跑就等（最多 20 分钟）。等待是必要的：刚 push 完立刻验收是常态。
for _ in $(seq 1 120); do
  [ "$ST" = "completed" ] && break
  printf '\r   云端状态 %s，等待中…' "$ST"
  sleep 10
  IFS=$'\t' read -r RUN_ID ST CONC RUN_URL <<<"$(find_run)"
done
echo ""
[ "$CONC" = "success" ] || {
  echo "❌ 云端构建未成功（conclusion=$CONC）。看日志：gh run view $RUN_ID --log-failed"
  exit 1; }
echo "   ✅ 云端构建成功"

# ── 2/8 下载产物 ──
echo "── 2/8 下载 arm64 镜像产物 ──"
rm -rf "$WORKDIR"; mkdir -p "$WORKDIR"
gh run download "$RUN_ID" -n "sla-core-arm64-${SHA7}" -D "$WORKDIR" >/dev/null
TAR="$WORKDIR/sla-core-arm64.tar"
[ -f "$TAR" ] || { echo "❌ 产物里没有 sla-core-arm64.tar"; ls -R "$WORKDIR"; exit 1; }
echo "   ✅ $(du -h "$TAR" | cut -f1)  $TAR"

# ── 3/8 架构断言（读 ELF，落在下载到的字节上）──
# 顺序有意：先验 tar 再 load。若这里失败，说明云端产出的镜像架构错了，
# 此时不该把它 load 进本地 docker 去"试试能不能跑"。
echo "── 3/8 断言产物内二进制是 arm64（ELF e_machine）──"
python3 verify/assert_image_arch.py "$TAR" arm64

# ── 4/8 载入并核对版本号 ──
echo "── 4/8 docker load + 版本号比对 ──"
docker load -i "$TAR" | sed 's/^/   /'
SLA_IMAGE="sla-core:arm64-${SHA7}"
export SLA_IMAGE
docker image inspect "$SLA_IMAGE" >/dev/null 2>&1 || {
  echo "❌ load 后找不到镜像 $SLA_IMAGE"; docker images sla-core; exit 1; }

CFG_ARCH=$(docker image inspect "$SLA_IMAGE" -f '{{.Architecture}}')
# 真跑一次二进制拿版本号：这一步同时是"arm64 二进制能在本机原生执行"的证明
# —— 架构不符会直接 exec format error，而不是打出版本号。
GOT_VER=$(docker run --rm "$SLA_IMAGE" -version | tr -d '\r\n')
echo "   config 架构：$CFG_ARCH   容器内 -version：$GOT_VER   本地 HEAD：$SHA7"
[ "$GOT_VER" = "$SHA7" ] || {
  echo "❌ 版本号不匹配 —— 跑起来的镜像不是 HEAD($SHA7) 的云端产物"; exit 1; }
echo "   ✅ 镜像来自 HEAD 的云端 arm64 构建"

# ── 5/8 起栈 ──
echo "── 5/8 起 compose 栈（PG + 2×core + collector）──"
export ADMIN_TOKEN="$TOKEN"
cleanup   # 清掉上一轮可能的残留，再起
"${COMPOSE[@]}" up -d >/dev/null 2>&1 || {
  echo "❌ compose up 失败"; "${COMPOSE[@]}" logs --tail 40; exit 1; }

ready=false
for _ in $(seq 1 60); do
  # 断言真的收到 200，而不是"curl 退出码为 0"：连不上时 curl 也可能
  # 因重定向/空响应而退 0，那样这道等待就成了摆设。
  if [ "$(curl -s -o /dev/null -w '%{http_code}' \
        "http://127.0.0.1:${PORT}/healthz" 2>/dev/null)" = "200" ]; then
    ready=true; break
  fi
  sleep 1
done
$ready || {
  echo "❌ sla-core 未就绪（/healthz 未返回 200）"
  "${COMPOSE[@]}" logs --tail 40; exit 1; }
echo "   ✅ /healthz 200"

# ── 6/8 迁移与选主（在 arm64 上重验一遍）──
echo "── 6/8 迁移与选主断言 ──"
WANT_MIG=$(ls migrations/*.sql | wc -l | tr -d ' ')
LOGS=$("${COMPOSE[@]}" logs sla-core-a sla-core-b 2>/dev/null)
APPLIED=$(echo "$LOGS" | grep -c "已应用迁移" || true)
[ "$APPLIED" = "$WANT_MIG" ] || {
  echo "❌ 「已应用迁移」$APPLIED 行，期望恰好 $WANT_MIG（每文件一次）"
  echo "   多于期望 = PG 咨询锁选主失效（两实例都跑了迁移）；少于 = 迁移不完整"
  exit 1; }
echo "   ✅ 迁移恰好 $WANT_MIG 次（选主生效）"

INITED=$(echo "$LOGS" | grep -c "初始化完成" || true)
[ "$INITED" -ge 2 ] || { echo "❌ 只有 $INITED 个实例初始化完成，期望 ≥2"; exit 1; }
echo "   ✅ $INITED 个实例初始化完成"

for bad in "does not exist" "duplicate key"; do
  ! echo "$LOGS" | grep -q "$bad" || {
    echo "❌ 日志出现「$bad」"; echo "$LOGS" | grep "$bad" | head -3; exit 1; }
done
echo "   ✅ 无缺表 / 主键冲突"

# collector 用同一个镜像换 entrypoint 跑：证明另一个二进制也是可执行的 arm64。
# 抓过的坑：compose 用 command 覆盖时 ENTRYPOINT 仍是 /sla-core，实际执行
# `/sla-core -once` → 打出"sla-core 启动"，采集器根本没跑。
CLOG=$("${COMPOSE[@]}" logs collector 2>/dev/null)
! echo "$CLOG" | grep -q "sla-core 启动" || {
  echo "❌ collector 容器在跑 sla-core —— entrypoint 未生效"; exit 1; }
echo "$CLOG" | grep -q "collector 启动" || {
  echo "❌ collector 容器没打出「collector 启动」"; echo "$CLOG" | tail -5; exit 1; }
echo "   ✅ collector 二进制在 arm64 上执行正常"

# ── 7/8 浏览器依赖 ──
echo "── 7/8 装浏览器验收依赖 ──"
(cd verify/ui && pnpm install --frozen-lockfile --silent >/dev/null 2>&1)
echo "   ✅ 依赖就绪"

# ── 8/8 真 Chrome 验收 ──
# 上游地址被填进 #ch-url，随后由**服务端**去 Detect/采集 —— 所以它必须在
# **容器**里可达。真上游走公网，容器只要能出网就行（这点比原先的 mock 简单：
# 不再需要 MOCK_BIND=0.0.0.0 那类容器网络的绑定讲究）。
# BASE 反之必须是宿主机地址：Chrome 跑在宿主机上。
echo "── 8/8 真 Chrome 验收（BASE=宿主机 / 上游=公网真站点）──"

# 探活选站：与 test-ui.sh 同一份脚本，保证两条验收路径选站口径一致。
UPJSON="$(HUB_FILE="$HUB_FILE" node "$ROOT/verify/pick-upstream.mjs")"
rd() { printf '%s' "$UPJSON" | node -e '
let s="";process.stdin.on("data",d=>s+=d).on("end",()=>
  process.stdout.write(String(JSON.parse(s)["'"$1"'"])))'; }
UP_URL="$(rd url)";   UP_TOKEN="$(rd token)"
UP_UID="$(rd uid)";   UP_QPU="$(rd quotaPerUnit)"
UP_MODELS="$(rd models)"; UP_PER_CALL="$(rd perCallModels)"
UP_KEYREF="$(rd keyRef)"
SHOTS=/tmp/sla-arm-shots
rm -rf "$SHOTS"; mkdir -p "$SHOTS"
# 子 shell 里 cd：verify-ui.mjs 要在自己目录跑（node_modules 在那），
# 但主 shell 的 cwd 不能变，否则后续任何相对路径（含 trap 里的）都会失效。
(
  cd "$ROOT/verify/ui"
  # 先跑 SPA 那份：它顺带验了前端产物**真的被打进了这个 arm64 镜像** ——
  # 镜像里的 web 阶段跑在 BUILDPLATFORM 上（产物与架构无关），
  # 但"跨架构 COPY 有没有落到位"只有真取一次带 hash 的资源才知道。
  BASE="http://127.0.0.1:${PORT}" node verify-spa.mjs

  BASE="http://127.0.0.1:${PORT}" \
  ADMIN_TOKEN="$TOKEN" \
  UP_URL="$UP_URL" \
  UP_TOKEN="$UP_TOKEN" \
  UP_UID="$UP_UID" \
  UP_QPU="$UP_QPU" \
  UP_MODELS="$UP_MODELS" \
  UP_PER_CALL="$UP_PER_CALL" \
  UP_KEYREF="$UP_KEYREF" \
  HUB_FILE="$HUB_FILE" \
  SHOTS="$SHOTS" \
    node verify-ui.mjs
)

echo ""
echo "✅ 云端 arm64 产物本地验收通过"
echo "   镜像 $SLA_IMAGE（云端 run #$RUN_ID，commit $SHA7）"
echo "   截图 $SHOTS"
