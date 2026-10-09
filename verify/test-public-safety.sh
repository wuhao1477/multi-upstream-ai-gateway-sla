#!/usr/bin/env bash
# Scan tracked working-tree files only; never print matched text or private rules.
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "❌ $1"; exit 1; }
if ! root=$(git rev-parse --show-toplevel 2>/dev/null); then
  die '无法读取 Git 工作区'
fi
cd "$root"
fail=0

scan() {
  local matches status
  if matches=$(git grep --no-color -n -I -E "$@" -- . 2>/dev/null); then
    echo '❌ 命中公开安全规则：'
    printf '%s\n' "$matches" | sed -E 's/:([0-9]+):.*/:\1/'
    fail=1
  else
    status=$?
    [ "$status" -eq 1 ] || die '安全扫描失败（搜索错误；未输出规则或正文）'
  fi
}

scan \
  -e 'eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}' \
  -e '(postgres|mysql|redis)://[^[:space:]$]+:[^[:space:]$@]{16,}@' \
  -e '-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----' \
  -e 'Bearer[[:space:]]+[A-Za-z0-9._~+/-]{32,}'

rules=${SLA_PUBLIC_SAFETY_RULES_FILE:-}
if [ -z "$rules" ]; then
  echo '仅运行通用规则，私有规则未覆盖'
else
  [ -f "$rules" ] && [ -r "$rules" ] || die '私有规则文件不可读'
  private_path=$(cd "$(dirname "$rules")" && printf '%s/%s\n' "$(pwd -P)" "$(basename "$rules")")
  case "$private_path" in
    "$root"/*)
      if ! tracked_rules=$(git ls-files -- "${private_path#"$root"/}" 2>/dev/null); then
        die '无法检查私有规则的 Git 跟踪状态'
      fi
      [ -z "$tracked_rules" ] || die '私有规则文件不得被 Git 跟踪'
      ;;
  esac
  # git reads one extended regular expression per line, without shell evaluation.
  scan -f "$private_path"
fi

if ! tracked=$(git ls-files 2>/dev/null); then
  die '无法读取 Git 文件列表'
fi
while IFS= read -r file; do
  case "$file" in
    *.env|all-api-hub-backup-*.json|*/all-api-hub-backup-*.json)
      echo '❌ 敏感文件被 Git 跟踪：'
      printf '%s\n' "$file"
      fail=1
      ;;
  esac
done <<< "$tracked"

for sample in all-api-hub-backup-2026-09-01.json docs/ARCHIFY-P1.html docs/superpowers/plans/2026-09-10-open-source-deployment-docs.md; do
  if git check-ignore -q "$sample" 2>/dev/null; then
    continue
  else
    status=$?
    [ "$status" -eq 1 ] || die '无法检查 Git 忽略规则'
    printf '❌ 缺少忽略规则：%s\n' "$sample"
    fail=1
  fi
done

[ "$fail" -eq 0 ] || exit 1
echo '✅ 公开安全扫描通过（当前已跟踪工作区文件；不含 Git 历史）'
