#!/usr/bin/env bash
# 同一份源码在 Vue 3.6 预发布版与 3.5 稳定版下都要能 lint + 类型检查 + 构建通过。
#
# 这是"为 3.6 提前适配"的**可检验**含义：光钉住 3.6.0-rc.6 只能证明它今天能编，
# 证明不了 3.6 正式版发布前后都不需要改代码。两个方向都要成立：
#   - 3.6.0-rc.6 过 → 3.6 正式版发布时直接抬版本号即可；
#   - 3.5.42 也过 → 万一 3.6 出问题，退回稳定版不用改一行业务代码。
#
# 做法是临时改版本号、装、检、构建，最后无条件还原。
# 跑完会把 node_modules 与 lockfile 恢复成仓库里的状态。
#
# ⚠️ 版本号写在**两个**文件里，必须一起改：
#      package.json        dependencies.vue     —— 直接依赖
#      pnpm-workspace.yaml overrides.vue        —— 最终生效的那份
#    pnpm 11 起不再读 package.json 的 `pnpm` 字段，overrides 只认 workspace 文件。
#    只改一处的后果是静默的：pinia/vue-router 的 peer 是 ^3.5.11，预发布版不落在
#    这个范围内，解析出第二份 vue 时会得到两个 Vue 实例 —— 编译期不报错，
#    运行时 inject 拿不到东西。
set -euo pipefail
cd "$(dirname "$0")/.."

PINNED='3.6.0-rc.6'
STABLE='3.5.42'

# 无条件还原：脚本中途失败也不能把仓库留在改过版本的状态。
#
# 先把三个文件抄到临时目录，结束时抄回来 —— 不用 git checkout：
#   1. lockfile 与 workspace 文件可能还没进版本库（迁移期就是这样），
#      `git checkout -- <未跟踪文件>` 报 pathspec 错，而一条 checkout 里混了
#      已跟踪与未跟踪路径会**整条失败**，于是三个文件一个都没还原 ——
#      静默留下 3.5.42，接着 make build 就把稳定版前端编进了二进制。
#   2. 就算都已提交，checkout 还原到的是 HEAD，会连带吞掉工作区里
#      其他未提交的改动。
BAK="$(mktemp -d)"
cp package.json pnpm-workspace.yaml pnpm-lock.yaml "$BAK"/
restore() {
  cp "$BAK"/package.json "$BAK"/pnpm-workspace.yaml "$BAK"/pnpm-lock.yaml ./
  rm -rf "$BAK"
  pnpm install --frozen-lockfile --silent >/dev/null 2>&1 || true
  # 顺带重编一遍：本脚本最后跑的是 3.5 稳定版，不重编的话 webdist/ 里留下的
  # 是稳定版产物，紧接着 go build 就把它打进二进制 —— 又是一次静默的
  # "前端与钉住的版本不一致"。
  pnpm run build >/dev/null 2>&1 || true
}
trap restore EXIT

set_version() {
  local ver="$1"

  node -e "
    const fs = require('node:fs')
    const p = JSON.parse(fs.readFileSync('package.json', 'utf8'))
    p.dependencies.vue = process.argv[1]
    fs.writeFileSync('package.json', JSON.stringify(p, null, 2) + '\n')
  " "$ver"

  # overrides 在 YAML 里，用 sed 直接替 `  vue: <ver>` 那一行，注释原样保留。
  # 不走 node：正则要穿 bash 双引号再进 JS 字符串，两层转义踩过一次坑。
  sed -E "s|^( *vue: ).*|\1${ver}|" pnpm-workspace.yaml > pnpm-workspace.yaml.tmp
  mv pnpm-workspace.yaml.tmp pnpm-workspace.yaml
  grep -qF "  vue: ${ver}" pnpm-workspace.yaml || {
    echo "  ✗ pnpm-workspace.yaml 没改成 ${ver}，overrides 结构变了"
    return 1
  }
}

run_with() {
  local ver="$1"
  echo "── Vue ${ver} ──"
  set_version "$ver"
  # 不能用 --frozen-lockfile：我们**故意**让清单与 lockfile 不一致
  pnpm install --no-frozen-lockfile --silent
  local got
  got=$(node -p "require('./node_modules/vue/package.json').version")
  if [ "$got" != "$ver" ]; then
    echo "  ✗ 装到的是 ${got}，不是 ${ver}"
    return 1
  fi
  # 确认 pinia / vue-router 与根解析到**同一个** vue —— 这正是 overrides 要防的
  # 那件事：两个 Vue 实例编译期不报错，运行时 inject 拿不到东西。
  #
  # 比的是 realpath，不是数 node_modules/.pnpm 下的 vue@* 目录 —— 那里是
  # "曾经链过的一切"，重复跑本脚本必然留上一轮的版本，数出来一定 >1。
  node -e "
    const fs = require('node:fs'), { createRequire } = require('node:module')
    const root = fs.realpathSync('./node_modules/vue')
    for (const p of ['pinia', 'vue-router']) {
      const r = createRequire(require.resolve(p + '/package.json'))
      const seen = fs.realpathSync(r.resolve('vue/package.json').replace(/[/]package[.]json\$/, ''))
      if (seen !== root) {
        console.error('  ✗ ' + p + ' 解析到另一份 vue：' + seen)
        process.exit(1)
      }
    }
  " || return 1
  pnpm run lint
  pnpm run typecheck
  pnpm run build
  echo "  ✓ Vue ${got}：单份实例、lint、类型检查与构建均通过"
}

run_with "$PINNED"
run_with "$STABLE"

echo
echo "两个版本均通过：3.6 正式版发布后抬版本号即可，必要时也能退回 3.5 稳定版。"
