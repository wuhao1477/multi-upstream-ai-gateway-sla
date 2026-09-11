#!/usr/bin/env bash
# 迁移与种子的集成测试：起临时 PG，跑真实 bootstrap，断言结果。
#
# 与 ddl-check.sh 的区别：那个只验「02 的 DDL 能否在 PG 上跑通」，
# 本脚本验的是**迁移系统本身** —— 幂等、选主、种子、分区、以及
# 02 §9.1bis 明确说 ddl-check 不覆盖的分区子表创建。
set -euo pipefail
cd "$(dirname "$0")/.."
export LC_ALL=C LANG=C

CT=migtestpg
# ⚠️ PG 的宿主端口必须在 32768 以下。原先是 55433 —— 那落在 Linux 默认临时端口段
# （net.ipv4.ip_local_port_range = 32768 60999）里，早前步骤的一条出站连接随时会
# 借走同一个号，随后 docker-proxy 绑不上就红在 "address already in use"。
# 2026-09-01 CI run #33432698039 就是这么红的（test-config-api 的 55434）——
# 与被测代码无关的假红。core 那几个口一直是 18xxx，所以从没撞过。
# ui-stack.sh / test-config-api.sh 的 PG 端口同理，理由不再各抄一遍。
PORT=18433
DSN="postgres://postgres:x@127.0.0.1:${PORT}/sla"

cleanup() { docker rm -f -v "$CT" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "── 起临时 PG ──"
docker run -d --name "$CT" -e POSTGRES_PASSWORD=x -e POSTGRES_DB=sla \
  -p "${PORT}:5432" postgres:16 >/dev/null
for _ in $(seq 1 60); do
  docker exec "$CT" pg_isready -U postgres >/dev/null 2>&1 && break
  sleep 1
done
sleep 2

q() { docker exec "$CT" psql -U postgres -d sla -tAc "$1"; }

echo "── 1/8 首次迁移 ──"
go run ./cmd/migrate -dsn "$DSN"

# 对象数从**迁移文本算出**再逐个比对，不再用 `≥45` 的下界。
# 下界的毛病是每次阶段增删都得手改那个数字，而改它的方向永远是"调到能绿"——
# 016 删掉 28 张表时它就正好还是绿的（45 之上），等于这条断言当时什么都没验。
# 逐个比对则两个方向都会红：库里多一张（迁移外的手工 DDL）或少一张都报名字。
WANT_RELS=$(python3 verify/check_migrations.py --relations)
GOT_RELS=$(q "select table_name from information_schema.tables
              where table_schema='public' and table_name<>'schema_migrations'
              order by table_name")
if [ "$WANT_RELS" != "$GOT_RELS" ]; then
  echo "❌ 库里的表/视图与迁移不一致"
  diff <(echo "$WANT_RELS") <(echo "$GOT_RELS") | sed 's/^/   /' || true
  echo "   （< 迁移该建的  > 库里实际的）"
  exit 1
fi
echo "   ✅ $(echo "$WANT_RELS" | wc -l | tr -d ' ') 个表/视图逐个对上（+ schema_migrations）"

echo "── 2/8 种子：可种子化的键全部灌入 ──"
# 期望值从生成物算出：EnvSourced 的 admin_token 不落表。
# 写死数字会在下次新增 EnvSourced 项时又挂一次。
WANT=$(grep -c 'EnvSourced: false}' internal/config/params_gen.go)
KEYS=$(q "select count(*) from config_params where scope_type='global'")
[ "$KEYS" = "$WANT" ] || { echo "❌ 配置键数 = $KEYS，期望 $WANT（可种子化项）"; exit 1; }
echo "   ✅ $WANT 键已灌入（EnvSourced 项按设计不落表）"

# 同样派生：关键项里 EnvSourced 的不落表（admin_token 既关键又 env 来源）
WANT_CRIT=$(grep -c 'Critical: true, Group: "[^"]*", EnvSourced: false}' \
  internal/config/params_gen.go || true)
CRIT=$(q "select count(*) from config_params where is_critical")
[ "$CRIT" = "$WANT_CRIT" ] || { echo "❌ 关键项 = $CRIT，期望 $WANT_CRIT"; exit 1; }
echo "   ✅ $WANT_CRIT 个关键项（EnvSourced 的关键项不落表）"

# 关键项种子必须 confirmed_twice=true，否则决策路径读不到它们（09 §3）
UNCONF=$(q "select count(*) from config_params where is_critical and not confirmed_twice")
[ "$UNCONF" = "0" ] || { echo "❌ 有 $UNCONF 个关键项未确认，快照会回落默认值"; exit 1; }
echo "   ✅ 关键项均已标记确认（出厂默认值不需人为二次确认）"

# 第 45 轮补的键必须在，且默认值正确 —— 取 0 会让每轮采集都判模型下架
WANT_ROUNDS=$(sed -n 's/.*{Key: "catalog_missing_rounds", Default: "\([^"]*\)".*/\1/p' \
  internal/config/params_gen.go)
MISSING=$(q "select coalesce((select param_value#>>'{}' from config_params
  where param_key='catalog_missing_rounds'),'ABSENT')")
[ "$MISSING" = "$WANT_ROUNDS" ] || {
  echo "❌ catalog_missing_rounds = $MISSING，期望 $WANT_ROUNDS"; exit 1; }
echo "   ✅ catalog_missing_rounds = ${WANT_ROUNDS}（取 0 会让每轮都判模型下架）"

echo "── 3/8 幂等：重复迁移 ──"
go run ./cmd/migrate -dsn "$DSN" >/dev/null
KEYS2=$(q "select count(*) from config_params where scope_type='global'")
[ "$KEYS2" = "$WANT" ] || { echo "❌ 重复迁移后键数变成 $KEYS2，种子不幂等"; exit 1; }
WANT_MIG=$(ls migrations/*.sql | wc -l | tr -d ' ')
APPLIED=$(q "select count(*) from schema_migrations")
[ "$APPLIED" = "$WANT_MIG" ] || { echo "❌ schema_migrations = $APPLIED，期望 $WANT_MIG"; exit 1; }
echo "   ✅ 重复执行不重复插入（键 $WANT、迁移记录 $WANT_MIG）"

echo "── 4/8 运维改过的值不被重启抹回 ──"
q "update config_params set param_value='99'::jsonb
    where param_key='catalog_missing_rounds'" >/dev/null
go run ./cmd/migrate -dsn "$DSN" >/dev/null
KEPT=$(q "select param_value#>>'{}' from config_params where param_key='catalog_missing_rounds'")
[ "$KEPT" = "99" ] || { echo "❌ 改过的值被抹回 $KEPT —— 那会让改配置只在重启前有效"; exit 1; }
echo "   ✅ 已有值不被覆盖"
q "update config_params set param_value='3'::jsonb
    where param_key='catalog_missing_rounds'" >/dev/null

echo "── 5/8 P1 库不保留后续阶段运行表 ──"
for t in requests attempts attempt_usage ledger_outbox session_prefix_ledger \
         probe_templates bindings cache_scopes model_aliases multiplier_versions \
         routing_policies gateway_clients; do
  n=$(q "select count(*) from information_schema.tables where table_schema='public' and table_name='$t'")
  [ "$n" = "0" ] || { echo "❌ P1 库仍保留表 $t"; exit 1; }
done
echo "   ✅ 12 张 P2/P3 运行表不存在"

# P1 三张新表与 upstream_keys 六列。
# 与上面的逐个比对**不重复**：那条只保证"库 == 迁移文本"，两边一起少掉一张表时
# 它照样绿（比如误删了 013 的建表）。这里点名 P1 真的要用的表，是独立的一道。
for t in channel_groups group_models channel_model_catalog; do
  n=$(q "select count(*) from information_schema.tables where table_name='$t'")
  [ "$n" = "1" ] || { echo "❌ 缺 P1 表 $t"; exit 1; }
done
COLS=$(q "select count(*) from information_schema.columns where table_name='upstream_keys'
          and column_name in ('channel_group_id','remain_quota_usd','used_quota_usd',
                              'rpm_limit','concurrency_limit','quota_synced_at')")
[ "$COLS" = "6" ] || { echo "❌ upstream_keys 的 P1 列 = $COLS，期望 6"; exit 1; }
coldef() {
  q "select a.attnotnull::text||'/'||coalesce(pg_get_expr(d.adbin,d.adrelid),'')
     from pg_class c
     join pg_attribute a on a.attrelid=c.oid
     left join pg_attrdef d on d.adrelid=c.oid and d.adnum=a.attnum
     where c.relname='$1' and a.attname='$2' and not a.attisdropped"
}
CSEQ=$(coldef channels catalog_sync_seq)
LSEQ=$(coldef channel_model_catalog last_seen_seq)
[ "$CSEQ" = "true/0" ] || {
  echo "❌ channels.catalog_sync_seq = ${CSEQ:-ABSENT}，期望 NOT NULL DEFAULT 0"; exit 1; }
[ "$LSEQ" = "true/0" ] || {
  echo "❌ channel_model_catalog.last_seen_seq = ${LSEQ:-ABSENT}，期望 NOT NULL DEFAULT 0"; exit 1; }
SEQ_CHECKS=$(q "select count(*) from pg_constraint
                where conname in ('channels_catalog_sync_seq_nonnegative',
                                  'channel_model_catalog_last_seen_seq_nonnegative')
                  and pg_get_constraintdef(oid) like '%>= 0%'")
[ "$SEQ_CHECKS" = "2" ] || {
  echo "❌ 020 的非负 CHECK = $SEQ_CHECKS，期望 2"; exit 1; }
echo "   ✅ P1 三表 + upstream_keys 六列 + 020 目录轮次两列就位"

CRED_LOCK=$(q "select count(*) from pg_constraint
               where conname='collector_credentials_refresh_lock_key_check'")
[ "$CRED_LOCK" = "1" ] || {
  echo "❌ 缺 Sub2API refresh_lock_key 约束（021）"; exit 1; }
echo "   ✅ Sub2API 凭证 refresh_lock_key 约束就位"

LEGACY_CRED_COLS=$(q "select count(*) from information_schema.columns
                       where table_name='collector_credentials'
                         and column_name in ('username','password')")
[ "$LEGACY_CRED_COLS" = "0" ] || {
  echo "❌ collector_credentials 仍有未实现的账密列"; exit 1; }
echo "   ✅ 未实现的账密列已移除"

CRED_ACCOUNT=$(q "select count(*) from information_schema.columns
                  where table_name='collector_credentials'
                    and column_name='account_id' and is_nullable='NO'")
[ "$CRED_ACCOUNT" = "1" ] || {
  echo "❌ collector_credentials.account_id 缺失或可为空"; exit 1; }
IDX_ACCOUNT=$(q "select count(*) from pg_indexes
                 where indexname='idx_cred_account_unique'")
[ "$IDX_ACCOUNT" = "1" ] || {
  echo "❌ 缺账号级凭证唯一索引 idx_cred_account_unique"; exit 1; }
echo "   ✅ 凭证按账号归属且唯一"

echo "── 6/8 CHECK 取值与站型注册表一致 ──"
# 017 收窄了 site_family 与 cred_type 的取值。这里断言"库里的取值范围 == 从注册表
# 推导出来的"，两个方向都会红：
#   · 加了一族却没配迁移放宽 → 该族的渠道**建不进来**，而约束冲突报在**写入时**，
#     不是启动时 —— 也就是新站型接好了、界面上选得到，一按创建才炸。
#   · 迁移放宽了却没人注册那一族 → 库比代码宽，那个值写进去之后 Lookup 拿不到注册，
#     采集侧报"无对应适配器"，停在只能靠人去 UPDATE 才能救的状态。
# 取值从 cmd/registry-dump 取而**不在这里写死**：写死的那份就是第二份家族清单，
# 而"漏加一族"正是本断言要查的事（同 1/7 不写 `≥45` 下界的理由）。
REG=$(go run ./cmd/registry-dump)
[ -n "$REG" ] || { echo "❌ registry-dump 无输出 —— 注册表是空的？"; exit 1; }

# site_family = 已注册家族 + unknown 哨兵（探测未命中，04 §7；它不该有注册，
# 但**必须是合法取值**，否则 Detect 没认出来的站连渠道都建不了）
WANT_FAM=$({ echo "$REG" | cut -f1; echo unknown; } | sort -u)

# cred_type 只允许当前已注册家族真正使用的类型。
WANT_CRED=$(echo "$REG" | cut -f2 | sort -u)

enum_of() { # enum_of <约束名> —— 从 CHECK 定义里取字面量取值
  q "select pg_get_constraintdef(oid) from pg_constraint where conname='$1'" \
    | grep -oE "'[a-z0-9_]+'" | tr -d "'" | sort -u
}

for c in channels_site_family_check collector_credentials_site_family_check; do
  GOT_FAM=$(enum_of "$c")
  [ -n "$GOT_FAM" ] || { echo "❌ 找不到约束 $c（017 没跑？约束改名了？）"; exit 1; }
  if [ "$GOT_FAM" != "$WANT_FAM" ]; then
    echo "❌ $c 的取值与注册表不一致"
    diff <(echo "$WANT_FAM") <(echo "$GOT_FAM") | sed 's/^/   /' || true
    echo "   （< 注册表推导的  > 库里实际的）加一族要配一条迁移放宽它"
    exit 1
  fi
done
echo "   ✅ site_family 两处约束 = $(echo "$WANT_FAM" | tr '\n' ' ')"

GOT_CRED=$(enum_of collector_credentials_cred_type_check)
if [ "$GOT_CRED" != "$WANT_CRED" ]; then
  echo "❌ collector_credentials_cred_type_check 的取值与注册表不一致"
  diff <(echo "$WANT_CRED") <(echo "$GOT_CRED") | sed 's/^/   /' || true
  echo "   （< 注册表推导的  > 库里实际的）"
  exit 1
fi
echo "   ✅ cred_type 约束 = $(echo "$WANT_CRED" | tr '\n' ' ')"

echo "── 7/8 两处 billing_unit 的列定义必须一致（018）──"
# 为什么断言"两处一致"而不是"权威表可空且有 CHECK"：
#
# 缺陷不是某一张表定错了，是**两张表对同一个列名给了相反的处置** —— 015 给目录
# 补「可空 + CHECK」并定下"无价则口径留 NULL、不补默认值"，而 006 早已把
# price_versions 定成 NOT NULL DEFAULT 'per_1m_token'，于是同一份写入代码
# （sink.go）在目录上遵守规则、在**成本公式真正读的那张表上**违反它。
# 断言"相等"能同时守住三件事，且日后加口径（per_1k_char）时会指着漏改的那一处红：
#   · 权威表不许再有默认值（默认值 = 列替上游声明了口径）
#   · 权威表必须有 CHECK（拼错的口径字符串不许入库，unit_tokens 查不到它）
#   · 两张表的取值集合不许分叉（宽的那张会先收下窄的那张拒绝的值）
DEF_CNT=$(q "select count(*) from information_schema.columns
             where table_name in ('price_versions','channel_model_catalog')
               and column_name='billing_unit' and column_default is not null")
[ "$DEF_CNT" = "0" ] || {
  echo "❌ billing_unit 仍有列默认值（$DEF_CNT 处）—— 默认值会把「上游未声明」伪装成"
  echo "   「已知按 token 计价」，而成本公式按该口径要除以 1,000,000（02 §1.3bis / 018）"
  q "select '   '||table_name||': default '||column_default from information_schema.columns
     where table_name in ('price_versions','channel_model_catalog')
       and column_name='billing_unit' and column_default is not null"
  exit 1; }

NULLABLE=$(q "select string_agg(table_name||'='||is_nullable, ' ' order by table_name)
              from information_schema.columns
              where table_name in ('price_versions','channel_model_catalog')
                and column_name='billing_unit'")
[ "$NULLABLE" = "channel_model_catalog=YES price_versions=YES" ] || {
  echo "❌ 两处 billing_unit 的可空性不一致或不为 YES：$NULLABLE"
  echo "   —— 一张用 NULL 表达「未声明」、另一张用「写不进来」表达，读 schema 的人"
  echo "      就得先判断这一列在哪张表上语义是什么。那正是本缺陷的成因（018 头部）"
  exit 1; }

# ⚠️ `|| true` 不可省。enum_of 里 `q | grep -oE …`，约束不存在时 grep 无匹配返回 1，
#    而 `set -o pipefail` 让整条管道返回 1 —— 简单赋值的退出码就是命令替换的退出码，
#    于是 `set -e` **在这一行就把脚本杀掉**，下面那句解释"没有 CHECK 会怎样"的
#    诊断根本执行不到。反向自验 C（删掉 ADD CONSTRAINT）实测到的就是这个：
#    脚本确实红了，但一个字都没打，红的原因要靠人去猜。
CAT_UNITS=$(enum_of channel_model_catalog_billing_unit_check || true)
PV_UNITS=$(enum_of price_versions_billing_unit_check || true)
[ -n "$PV_UNITS" ] || {
  echo "❌ price_versions 上找不到 billing_unit 的 CHECK（018 没跑？约束改名了？）"
  echo "   没有它，拼错的口径（'per_1M_token'）照收，而 unit_tokens 查表查不到那个值"
  exit 1; }
if [ "$CAT_UNITS" != "$PV_UNITS" ]; then
  echo "❌ 两处 billing_unit 的取值集合不一致 —— 宽的那张会收下窄的那张拒绝的值"
  diff <(echo "$CAT_UNITS") <(echo "$PV_UNITS") | sed 's/^/   /' || true
  echo "   （< 目录表的  > 权威价格表的）加一种口径要同时改两处"
  exit 1
fi
echo "   ✅ 两处均为「可空 + 无默认 + CHECK」，取值 = $(echo "$PV_UNITS" | tr '\n' ' ')"

# 真跑两条：约束要在**写入时**起作用，而不是只存在于 pg_constraint 里。
# 少了这两条，一个建在别的列上、或 NOT VALID 的约束也能让上面全绿。
#
# ⚠️ 必须先造 channel/model 行：price_versions 的两条外键指向它们，空库里
#    （channels 0 行 / models 0 行，后者是 AC-39 的常态）**外键会先于 CHECK 触发**，
#    于是这条断言看到的错误与口径无关，却照样"红得对" —— 下面显式区分了这两种失败。
#    整段包在事务里回滚，不给后续步骤留测试行。
# ⚠️ 两条探针各用**自己的 model 行**（mid1/mid2）。共用一个 (cid,mid) 时探针②
#    的 `SELECT … WHERE channel_id=cid AND model_id=mid` 会取到两行 ——
#    标量子查询报 cardinality_violation，而那不在下面的 EXCEPTION 名单里，
#    于是 DO 块整体中止、psql 非零退出、`set -e` 在赋值这一行就杀掉脚本。
#    反向自验 E 实测过：脚本红了，但红的原因是这个，**不是** BAD_ACCEPTED 那条
#    断言在起作用 —— 一个为错误原因而红的破坏测试等于没验。
# ⚠️ `|| true` 同理：探针出任何意外时要让下面的 case 打出 $PROBE 供人读，
#    而不是静默死在赋值上。
PROBE=$(docker exec "$CT" psql -U postgres -d sla -tAc "
begin;
insert into channels (name, site_family, base_url)
  values ('billing-unit-probe','unknown','http://127.0.0.1:1');
insert into models (canonical_name) values ('billing-unit-probe-1');
insert into models (canonical_name) values ('billing-unit-probe-2');
do \$\$
DECLARE cid bigint; mid1 bigint; mid2 bigint; got text;
BEGIN
  SELECT id INTO cid  FROM channels WHERE name='billing-unit-probe';
  SELECT id INTO mid1 FROM models   WHERE canonical_name='billing-unit-probe-1';
  SELECT id INTO mid2 FROM models   WHERE canonical_name='billing-unit-probe-2';
  -- ① 坏口径必须被拦
  BEGIN
    INSERT INTO price_versions (id, channel_id, model_id, input_price, output_price,
                                billing_unit, data_source, queried_at, effective_at)
    VALUES (gen_random_uuid(), cid, mid1, 1, 1, 'per_1M_token', 'manual', now(), now());
    RAISE NOTICE 'BAD_ACCEPTED';
  EXCEPTION
    WHEN check_violation      THEN RAISE NOTICE 'BAD_REJECTED';
    WHEN foreign_key_violation THEN RAISE NOTICE 'FK_FIRST';
  END;
  -- ② 不给 billing_unit 必须落 NULL（而不是被默认值补成 per_1m_token）
  BEGIN
    INSERT INTO price_versions (id, channel_id, model_id, input_price, output_price,
                                data_source, queried_at, effective_at)
    VALUES (gen_random_uuid(), cid, mid2, 1, 1, 'manual', now(), now());
    SELECT billing_unit INTO got FROM price_versions
      WHERE channel_id=cid AND model_id=mid2;
    IF got IS NULL THEN
      RAISE NOTICE 'OMIT_IS_NULL';
    ELSE
      RAISE NOTICE 'OMIT_DEFAULTED_TO_%', got;
    END IF;
  EXCEPTION
    WHEN not_null_violation THEN RAISE NOTICE 'OMIT_NOT_NULL';
  END;
END \$\$;
rollback;" 2>&1 || true)
case "$PROBE" in
  *FK_FIRST*)
    echo "❌ 外键先于 CHECK 触发 —— 这条断言没验到口径约束（造的依赖行没进去？）"
    echo "$PROBE" | sed 's/^/   /'; exit 1;;
  *BAD_ACCEPTED*)
    echo "❌ 坏口径 'per_1M_token' 被收下了 —— CHECK 没生效"
    echo "   unit_tokens 查表查不到这个值，成本要么 panic 要么落到某个默认分支"
    exit 1;;
esac
case "$PROBE" in
  *BAD_REJECTED*) ;;
  *) echo "❌ 探针没跑到坏口径那一步，输出如下："; echo "$PROBE" | sed 's/^/   /'; exit 1;;
esac
case "$PROBE" in
  *OMIT_IS_NULL*) ;;
  *OMIT_DEFAULTED_TO_*)
    echo "❌ 省略 billing_unit 时被列默认值补上了：$(echo "$PROBE" |
      grep -o 'OMIT_DEFAULTED_TO_[a-z0-9_]*')"
    echo "   —— 那正是 018 要去掉的东西：默认值替上游声明了口径"
    exit 1;;
  *OMIT_NOT_NULL*)
    echo "❌ 省略 billing_unit 时报 NOT NULL —— 018 的 DROP NOT NULL 没生效"
    exit 1;;
  *) echo "❌ 探针没跑到省略那一步，输出如下："; echo "$PROBE" | sed 's/^/   /'; exit 1;;
esac
echo "   ✅ 坏口径写入时被拦；省略口径落 NULL 而非被默认值补上（探针已回滚）"

echo "── 8/8 渠道与导入写路径的真库 Go 测试 ──"
# 为什么挂在这里：这几条要**可写的真 PG**，而本脚本已经有一个。
# internal/admin 的其余测试不碰库，SLA_TEST_DSN 不设时它们自己 t.Skip。
#
# 覆盖的缺口，两轮各一批：
#  · 2026-08-29 二次评审：POST /admin/import/all-api-hub 的**写路径**零自动覆盖
#    —— 58 项浏览器验收只跑 dry_run=true（真导入会写上百个渠道，那是运维的决定），
#    于是"四次独立写入中途失败留下半成品"没有任何断言看得见。
#  · 2026-09-01 自审 + Codex 三次评审：**PATCH /admin/channels/{id} 的 base_url
#    一处校验都没有**（POST 拒 file:// 而 PATCH 落库），建渠道与探测快照不原子，
#    补齐不补探测快照且覆盖 warning，渠道身份无库级唯一性。
#    107 项验收跑过 PATCH 的改名/停用/启用，从没 PATCH 过 base_url。
# ⚠️ 测试名**逐个列出并锚定**（`^(…)$`），不用 `TestImport|TestChannel|…` 那种前缀正则。
# Codex 2026-09-01 三次评审 [low]：前缀正则会让**任何**日后新增的、名字碰巧以
# TestImport/TestChannel/TestPatchChannel/TestCreateChannel 开头的合法测试把这一步
# 弄红 —— 而它跟迁移毫无关系。锚定列表则相反：新增测试不影响本步，改名会红在这里。
#
# 但 `EXPECT` 那个固定数**留着**（Codex 建议删）：`-run` 里打错一个字的那条只是
# 匹配不上，`go test` 照样 exit 0 并打 "no tests to run" —— 那正是最容易悄悄发生的
# 假绿，而它恰恰**不会**被退出码抓到。列表 + 计数各管一头：列表管"别牵连无关测试"，
# 计数管"列表本身别写错"。
TESTS=(
  TestImportRollsBackOnCredentialFailure
  TestImportRepairsIncompleteChannel
  TestImportRepairAddsSnapshotAndKeepsWarning
  TestImportRepairUpdatesCredentialFamily
  TestImportRejectsBadBaseURL
  TestImportValidatesBeforeProbing
  TestPatchChannelRejectsBadBaseURL
  TestChannelBaseURLNormalizedAndUnique
  TestCreateChannelRollsBackWhenSaveDetectedFails
  TestCreateKeyRejectsUnknownGroupRef
  TestListKeysIncludesGroupAndMultiplier
  TestPatchAccountCanClearEditableFields
  TestPatchKeyCanClearGroup
)
PAT="^($(IFS='|'; echo "${TESTS[*]}"))\$"
EXPECT=${#TESTS[@]}
if ! SLA_TEST_DSN="$DSN" go test ./internal/admin/ -run "$PAT" -count=1 -v \
     >/tmp/import-tx.log 2>&1; then
  echo "❌ 渠道/导入写路径测试失败"
  sed 's/^/   /' /tmp/import-tx.log | tail -40
  exit 1
fi
RAN=$(grep -c '^--- PASS: Test' /tmp/import-tx.log || true)
SKIPPED=$(grep -c '^--- SKIP: Test' /tmp/import-tx.log || true)
if [ "${SKIPPED:-0}" -gt 0 ]; then
  echo "❌ 有 ${SKIPPED} 条被跳过 —— SLA_TEST_DSN 没传进去，这几条等于没跑"
  grep '^--- SKIP' /tmp/import-tx.log | sed 's/^/   /'
  exit 1
fi
# 计数与上面的 TESTS 列表联动（不再写死 8）：列表少一个名字就红在这里。
if [ "${RAN:-0}" -ne "$EXPECT" ]; then
  echo "❌ 只跑了 ${RAN} 条，期望 ${EXPECT} 条（TESTS 列表里有名字拼错或已改名？）"
  grep -E '^--- (PASS|FAIL|SKIP)' /tmp/import-tx.log | sed 's/^/   /'
  exit 1
fi
echo "   ✅ 导入：凭证失败整体回滚 / 半成品能补齐 / 补齐补上探测快照且不覆盖 warning / 凭证站型同步更新 / 坏 base_url 被拒"
echo "   ✅ 渠道：PATCH 与 POST 共用地址校验 / 尾斜杠规范化 + 019 唯一约束 / 建渠道与探测快照同生共死"
echo "   ✅ 编辑：账号字段可清空 / Key 可解除分组"

STORE_TESTS=(
  TestHostRequestLimiterSerializesIndependentInstances
  TestHostRequestLimiterDoesNotBlockDifferentHosts
  TestHostRequestLimiterHonorsCancellation
  TestPoolMaxConnsFitsConcurrentCollectionWorkers
  TestSub2APIRefreshIsSerializedAcrossInstancesByRefreshLockKey
  TestSaveTxPreservesExistingRefreshLockKeyOnPartialUpdate
  TestCredentialRefreshLockMigrationMatchesRuntimeURLNormalization
  TestCatalogStaleAfterReliableMissingRounds
  TestUnregisteredKeySnapshotVisibleInInventory
  TestSaveKeyPersistsZeroQuota
  TestSavePricingUpdatesExistingCatalogPriceOnly
  TestSaveGroupsMissingModelFieldPreservesPreviousModels
  TestSaveGroupsRollsBackBusinessRowsWhenSnapshotFails
  TestSaveKeyRollsBackUsageWhenSnapshotFails
  TestSavePricingStoresSnapshotBeforeCountingRow
  TestCredentialsAreUniquePerAccount
  TestSameExternalKeyRefUpdatesItsOwnAccount
)
PAT="^($(IFS='|'; echo "${STORE_TESTS[*]}"))\$"
EXPECT=${#STORE_TESTS[@]}
if ! SLA_TEST_DSN="$DSN" go test ./internal/store/ -run "$PAT" -count=1 -v \
     >/tmp/catalog-round.log 2>&1; then
  echo "❌ 资产采集持久化测试失败"
  sed 's/^/   /' /tmp/catalog-round.log | tail -40
  exit 1
fi
RAN=$(grep -c '^--- PASS: Test' /tmp/catalog-round.log || true)
SKIPPED=$(grep -c '^--- SKIP: Test' /tmp/catalog-round.log || true)
if [ "${SKIPPED:-0}" -gt 0 ] || [ "${RAN:-0}" -ne "$EXPECT" ]; then
  echo "❌ 资产采集持久化测试未真实跑完（pass=${RAN:-0}/${EXPECT} skip=${SKIPPED:-0}）"
  grep -E '^--- (PASS|FAIL|SKIP): Test' /tmp/catalog-round.log | sed 's/^/   /'
  exit 1
fi
echo "   ✅ 目录轮次：连续 3 个可靠轮次缺席后 stale；再次出现后清除 stale"
echo "   ✅ 异常：未登记 Key 可见，补登记后消失"
echo "   ✅ Key 用量：明确采到 0 时覆盖旧额度，不把 0 当成缺字段"
echo "   ✅ 价格：独立价格周期刷新现有目录且不推进目录轮次"
echo "   ✅ 分组：降级响应缺少模型字段时保留旧模型清单"
echo "   ✅ 凭证：跨实例共享 refresh_lock_key 且只刷新一次"
echo "   ✅ host 限速：跨进程串行 / 不同 host 互不阻塞 / 取消即退出"
echo "   ✅ 连接池：4 条时并发采集会互等到超时，MinPoolConns 条够用"

echo
echo "✅ 迁移集成测试全部通过"
