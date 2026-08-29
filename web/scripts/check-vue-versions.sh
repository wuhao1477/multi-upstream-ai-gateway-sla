#!/usr/bin/env bash
# 同一份源码在 Vue 3.6 预发布版与 3.5 稳定版下都要能类型检查 + 构建通过。
#
# 这是"为 3.6 提前适配"的**可检验**含义：光钉住 3.6.0-rc.6 只能证明它今天能编，
# 证明不了 3.6 正式版发布前后都不需要改代码。两个方向都要成立：
#   - 3.6.0-rc.6 过 → 3.6 正式版发布时直接抬版本号即可；
#   - 3.5.42 也过 → 万一 3.6 出问题，退回稳定版不用改一行业务代码。
#
# 做法是临时改 package.json 的 vue 版本、装、检、构建，最后无条件还原。
# 跑完会把 node_modules 与 lockfile 恢复成仓库里的状态。
set -euo pipefail
cd "$(dirname "$0")/.."

PINNED='3.6.0-rc.6'
STABLE='3.5.42'

# 无条件还原：脚本中途失败也不能把仓库留在改过版本的状态
restore() {
  git checkout -- package.json package-lock.json 2>/dev/null || true
  npm ci --silent >/dev/null 2>&1 || true
}
trap restore EXIT

run_with() {
  local ver="$1"
  echo "── Vue ${ver} ──"
  # dependencies 与 overrides 两处都要改：overrides 才是最终生效的那份
  node -e "
    const fs = require('node:fs')
    const p = JSON.parse(fs.readFileSync('package.json', 'utf8'))
    p.dependencies.vue = process.argv[1]
    p.overrides.vue = process.argv[1]
    fs.writeFileSync('package.json', JSON.stringify(p, null, 2) + '\n')
  " "$ver"
  npm install --silent
  local got
  got=$(node -p "require('./node_modules/vue/package.json').version")
  if [ "$got" != "$ver" ]; then
    echo "  ✗ 装到的是 ${got}，不是 ${ver}"
    return 1
  fi
  npm run typecheck
  npm run build
  echo "  ✓ Vue ${got}：类型检查与构建均通过"
}

run_with "$PINNED"
run_with "$STABLE"

echo
echo "两个版本均通过：3.6 正式版发布后抬版本号即可，必要时也能退回 3.5 稳定版。"
