#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

fail=0
scan() {
  local label=$1
  local pattern=$2
  local matches
  matches=$(git grep -n -I -E "$pattern" -- . ':!*.lock' ':!*.svg' ':!verify/test-public-safety.sh' 2>/dev/null \
    | sed -E 's/:[0-9]+:.*/:[line redacted]/' || true)
  if [ -n "$matches" ]; then
    echo "❌ $label"
    echo "$matches"
    fail=1
  fi
}

scan 'JWT 令牌' 'eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}'
scan '疑似真实数据库连接串' '(postgres|mysql|redis)://[^[:space:]$]+:[^[:space:]$@]{16,}@'
scan '私钥' '-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----'
scan '长 Bearer 令牌' 'Bearer[[:space:]]+[A-Za-z0-9._-]{40,}'
scan '内网数据库地址' '100\.60\.0\.1|SLA_DB'
scan '真实验收站点地址' 'api2\.aigcbest\.top|api\.lyy\.team|molifangapi\.xyz|7x\.hk|api\.hyhawang\.com|api\.asxs\.top'

tracked_sensitive=$(git ls-files | rg '(^|/)(\.env$|.*\.env$|all-api-hub-backup-.*\.json$)' || true)
if [ -n "$tracked_sensitive" ]; then
  echo "❌ 敏感文件被 Git 跟踪："
  echo "$tracked_sensitive"
  fail=1
fi

for sample in all-api-hub-backup-2026-09-01.json docs/ARCHIFY-P1.html docs/superpowers/plans/2026-09-10-open-source-deployment-docs.md; do
  git check-ignore -q "$sample" 2>/dev/null || {
    echo "❌ 缺少忽略规则：$sample"
    fail=1
  }
done

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "✅ 公开仓库安全基线通过（当前 Git 可达内容）"
