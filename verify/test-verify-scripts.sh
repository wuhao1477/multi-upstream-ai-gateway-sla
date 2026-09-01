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
if grep -q 'cap!="subscription_quotas" and st=="unsupported"' "$ac38"; then
  fail "ac38-sub2api.sh 不得写死除 subscription 外不能 unsupported"
fi

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

echo "✅ verify 脚本静态检查通过"
