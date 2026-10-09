#!/usr/bin/env bash
# Synthetic inputs exercise the scanner itself, never real addresses or credentials.
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/repo/verify"
cp verify/test-public-safety.sh "$fixture/repo/verify/"
cp .gitignore "$fixture/repo/"
git init -q "$fixture/repo"
git -C "$fixture/repo" add .gitignore verify/test-public-safety.sh
rules="$fixture/private.rules"
marker=synthetic-private-safety-marker
checks=0
failures=0

check() {
  local expected=$1 label=$2 status
  if output=$(SLA_PUBLIC_SAFETY_RULES_FILE="$rules_path" bash "$fixture/repo/verify/test-public-safety.sh" 2>&1); then
    status=0
  else
    status=$?
  fi
  checks=$((checks + 1))
  if [ "$status" -ne "$expected" ] || [[ "$output" == *"$marker"* ]]; then
    echo "❌ ${label}（退出状态或脱敏不符）"
    failures=$((failures + 1))
  fi
}

rules_path=
check 0 '通用扫描成功'
if [[ "$output" != *'仅运行通用规则，私有规则未覆盖'* ]]; then
  echo '❌ 未配置规则时缺少覆盖范围提示'
  failures=$((failures + 1))
fi

printf '%s\n' "$marker" > "$rules"
rules_path=$rules
printf '%s\n' "$marker" > "$fixture/repo/payload.txt"
git -C "$fixture/repo" add payload.txt
check 1 '私有规则命中'
if [[ "$output" != *'payload.txt:1'* ]]; then
  echo '❌ 命中未给出文件及真实行号'
  failures=$((failures + 1))
fi
printf '%s\n' safe > "$fixture/repo/payload.txt"
printf '# %s\n' "$marker" >> "$fixture/repo/verify/test-public-safety.sh"
check 1 '扫描器自身不能排除'
cp verify/test-public-safety.sh "$fixture/repo/verify/"

rules_path="$fixture/missing.rules"
check 1 '规则读取错误必须失败'
rules_path=$rules
printf '[%s\n' "$marker" > "$rules"
check 1 '非法正则必须失败且不泄露规则'

printf '%s\n' '$(touch execution-marker)' > "$rules"
check 0 '规则只作数据读取'
if [ -e "$fixture/repo/execution-marker" ]; then
  echo '❌ 规则被执行为 shell 代码'
  failures=$((failures + 1))
fi

printf '%s\n' "$marker" > "$fixture/repo/private.rules"
git -C "$fixture/repo" add private.rules
rules_path="$fixture/repo/private.rules"
check 1 '拒绝已跟踪私有规则'
git -C "$fixture/repo" rm -q --cached private.rules

# Assemble the token at runtime so this test file does not carry a credential-shaped literal.
rules_path=
marker=$(printf 'eyJ%s.%s.%s' 01234567890123 01234567890123 01234567890123)
printf '%s\n' "$marker" > "$fixture/repo/payload.txt"
check 1 '通用 JWT 规则仍生效'

[ "$failures" -eq 0 ] || exit 1
echo "✅ 扫描器行为验证通过（${checks} 项）"
