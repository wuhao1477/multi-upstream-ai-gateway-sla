#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

fail() {
  echo "❌ $1"
  exit 1
}

ac38=verify/ac38-sub2api.sh
ui=verify/ui-stack.sh
remote=verify/remote-stack.sh
test_migrate=verify/test-migrate.sh
test_compose=verify/test-compose.sh
ddl=verify/ddl-extracted.sql
data_model=docs/dev/02-data-model.md

grep -q 'umask 077' "$ac38" || fail "ac38-sub2api.sh 必须设置 umask 077"
grep -Eq 'mktemp -d' "$ac38" || fail "ac38-sub2api.sh 必须使用 mktemp -d 创建私有临时目录"
grep -q 'trap cleanup EXIT' "$ac38" || fail "ac38-sub2api.sh 必须用 EXIT trap 调 cleanup"
grep -q 'rm -rf "$AC38_TMP"' "$ac38" || fail "ac38-sub2api.sh cleanup 必须删除整个临时目录"
grep -q 'TOKEN_FILE="$AC38_TMP/access-token"' "$ac38" ||
  fail "ac38-sub2api.sh 必须用私有临时文件传递上游 token"
if grep -q 'ATOK' "$ac38"; then
  fail "ac38-sub2api.sh 不得把上游 token 放进进程 argv"
fi
if grep -Eq '"token"[[:space:]]*:' "$ac38"; then
  fail "ac38-sub2api.sh 的选站结果不得包含上游 token"
fi
grep -q '/api/v1/settings/public' "$ac38" ||
  fail "ac38-sub2api.sh 选站必须验证 Sub2API 公开指纹"
grep -q 'turnstile-enabled' "$ac38" ||
  fail "ac38-sub2api.sh 选站必须排除开启人机验证的站点"
grep -q 'AC38_ACCESS_TOKEN_FILE' "$ac38" ||
  fail "ac38-sub2api.sh 必须只通过私有文件接收显式验收令牌"
grep -q 'turnstile_enabled") is True and not explicit' "$ac38" ||
  fail "ac38-sub2api.sh 只可在显式授权验收时接受开启人机验证的站点"
grep -q 'auth-not-profile' "$ac38" ||
  fail "ac38-sub2api.sh 不得把 HTTP 200 的非账号响应当作鉴权成功"

for name in resp sync cred key acc keyreq; do
  if grep -q "/tmp/ac38-${name}\\.json" "$ac38"; then
    fail "ac38-sub2api.sh 不得把 ac38-${name}.json 写到固定 /tmp"
  fi
done
grep -q '"capability","support","status"' "$ac38" ||
  fail "ac38-sub2api.sh 必须断言每项都有 support"
grep -q 'supported 能力必须产生数据' "$ac38" || fail "ac38-sub2api.sh 必须断言 supported 有数据"
grep -q '零行只允许 degraded/unsupported 且必须有说明' "$ac38" ||
  fail "ac38-sub2api.sh 必须断言零行规则"
# 凭证接口按账号关联；账号必须先建好并把 account_id 写入凭证请求。
acc_create_line=$(grep -n 'POST "\$A/accounts"' "$ac38" | head -1 | cut -d: -f1 || true)
cred_submit_line=$(grep -n 'POST "\$A/collector/credentials"' "$ac38" | head -1 | cut -d: -f1 || true)
[ -n "$acc_create_line" ] && [ -n "$cred_submit_line" ] ||
  fail "ac38-sub2api.sh 必须同时创建账号并登记凭证"
[ "$acc_create_line" -lt "$cred_submit_line" ] ||
  fail "ac38-sub2api.sh 必须先创建账号，再登记凭证"
grep -Eq "json\.dump\(\{'account_id':int\(sys\.argv\[1\]\)" "$ac38" ||
  fail "ac38-sub2api.sh 凭证请求必须使用 account_id"

grep -Eq 'sla-core .* -read-only([[:space:]]|$)' "$remote" ||
  fail "remote-stack.sh 必须用 -read-only 启动 sla-core"

awk '
  $0 == "EOF" && prev == "" {
    print "❌ ui-stack.sh 的 EOF 前不应保留空行"
    exit 1
  }
  { prev = $0 }
' "$ui"

for needle in \
  catalog_sync_seq \
  last_seen_seq \
  channels_catalog_sync_seq_nonnegative \
  channel_model_catalog_last_seen_seq_nonnegative; do
  grep -q "$needle" "$test_migrate" || fail "test-migrate.sh 必须检查 $needle"
done
for needle in catalog_sync_seq last_seen_seq; do
  grep -q "$needle" "$data_model" || fail "02-data-model.md 必须记录 $needle"
  grep -q "$needle" "$ddl" || fail "ddl-extracted.sql 必须包含 $needle"
done
grep -q 'TestCatalogStaleAfterReliableMissingRounds' "$test_migrate" ||
  fail "test-migrate.sh 必须纳入目录轮次真库测试"
grep -q 'TestImportRepairUpdatesCredentialFamily' "$test_migrate" ||
  fail "test-migrate.sh 必须纳入凭证站型同步更新测试"

# Bash 在 UTF-8 locale 下会把紧邻的全角括号视作变量名的一部分。
# 必须用花括号明确 WANT_KEYS 的边界，否则 compose 冒烟稳定报 unbound variable。
grep -Fq '${WANT_KEYS}（' "$test_compose" ||
  fail "test-compose.sh 输出 WANT_KEYS 后接全角括号时必须使用花括号"

echo "✅ verify 脚本静态检查通过"
