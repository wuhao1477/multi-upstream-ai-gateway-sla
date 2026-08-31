#!/usr/bin/env bash
# 迁移与种子的集成测试：起临时 PG，跑真实 bootstrap，断言结果。
#
# 与 ddl-check.sh 的区别：那个只验「02 的 DDL 能否在 PG 上跑通」，
# 本脚本验的是**迁移系统本身** —— 幂等、选主、种子、分区、以及
# 02 §9.1bis 明确说 ddl-check 不覆盖的分区子表创建。
set -euo pipefail
cd "$(dirname "$0")/.."

CT=migtestpg
PORT=55433
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
# 期望值**从生成物算出**而不写死：72 键里 EnvSourced 的不落表
# （admin_token，09 §4bis 避免自己改自己），故库中应是 72-1=71。
# 写死数字会在下次新增 EnvSourced 项时又挂一次。
WANT=$(grep -c 'EnvSourced: false}' internal/config/params_gen.go)
KEYS=$(q "select count(*) from config_params where scope_type='global'")
[ "$KEYS" = "$WANT" ] || { echo "❌ 配置键数 = $KEYS，期望 $WANT（可种子化项）"; exit 1; }
echo "   ✅ $WANT 键已灌入（EnvSourced 项按设计不落表）"

# 同样派生：关键项里 EnvSourced 的不落表（admin_token 既关键又 env 来源）
WANT_CRIT=$(grep -c 'Critical: true, Group: "[^"]*", EnvSourced: false}' \
  internal/config/params_gen.go)
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
echo "   ✅ catalog_missing_rounds = $WANT_ROUNDS（取 0 会让每轮都判模型下架）"

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

echo "── 5/8 账本母表是分区表（ddl-check 不覆盖这项）──"
# 016 之前这里断言"3 张 2026-08 子表已挂载"。那三张子表是 02 §9.1 的**示例**
# DDL（写死月份），016 已删 —— 分区管理属运行期（定时任务或 pg_partman）。
# 于是这条改断更耐久的那一半：母表确实以 PARTITION BY 建出（relkind='p'）。
# 建成普通表会让 P2 上线后无法按月 DETACH+DROP，而那时表里已经有数据了。
PARTED=$(q "select count(*) from pg_class
            where relname in ('requests','attempts','attempt_usage') and relkind='p'")
[ "$PARTED" = "3" ] || { echo "❌ 分区母表 = $PARTED，期望 3（requests/attempts/attempt_usage）"; exit 1; }
echo "   ✅ 3 张账本母表均为分区表"

# 当前**没有任何分区**，这是 016 的既知后果，写成断言以免它被当成回归。
# P2 写账本之前必须先落地分区管理，否则第一次 INSERT 报
# "no partition of relation found for row"。
PARTS=$(q "select count(*) from pg_inherits i join pg_class p on p.oid=i.inhparent
           where p.relname in ('requests','attempts','attempt_usage')")
[ "$PARTS" = "0" ] || {
  echo "⚠️ 母表已有 $PARTS 个分区 —— 若已落地分区管理，请把 016 的告知与本断言一并更新"
  exit 1; }
echo "   ✅ 母表暂无分区（016 既知：P2 写账本前须先落地分区管理）"

# 删分区**没有顺带删掉母表间的外键** —— 016 的头号坑，实测过一次真的会发生。
# PG 给被引用分区表的每个分区各建一条子约束，`DROP ... CASCADE` 会顺着它把
# attempt_usage → attempts 那条一起带走，且只打一行 NOTICE。丢了它，P2 写账本时
# 野 attempt_id 无人拦。016 改用 DETACH+DROP 规避，这条断言盯住它别退回 CASCADE。
FK=$(q "select count(*) from pg_constraint
        where conrelid='attempt_usage'::regclass and contype='f'
          and confrelid='attempts'::regclass")
[ "$FK" = "1" ] || {
  echo "❌ attempt_usage → attempts 的外键 = $FK，期望 1"
  echo "   八成是删分区用了 DROP ... CASCADE（见 016 头部实测记录），改回 DETACH 再 DROP"
  q "select '   现存外键: '||conname||' → '||confrelid::regclass from pg_constraint
     where conrelid='attempt_usage'::regclass and contype='f'"
  exit 1; }
echo "   ✅ attempt_usage → attempts 外键仍在（删分区未顺带带走它）"

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
echo "   ✅ P1 三表 + upstream_keys 六列就位"

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

# cred_type = 各注册的 CredType 与 PasswdCredType，另加 account_password ——
# 它当前无人声明，是刻意留着的"无令牌端点站型只能账密重登"那条通路的库侧一端
# （017 头部与 04 §5.3）。写在这里而不是让它随注册表浮动，是因为它的存在理由
# 恰恰是"当前没有任何注册声明它"。
WANT_CRED=$({ echo "$REG" | cut -f2; echo "$REG" | cut -f3; echo account_password; } \
  | grep -v '^$' | sort -u)

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
PAT='TestImport|TestPatchChannel|TestChannel|TestCreateChannel'
EXPECT=8
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
# 数目写死是刻意的：加了测试却忘了改这里会红，而"少跑了几条"正是最容易
# 悄悄发生的假绿（-run 正则打错一个字就少匹配几条，日志里看不出来）。
if [ "${RAN:-0}" -ne "$EXPECT" ]; then
  echo "❌ 只跑了 ${RAN} 条，期望 ${EXPECT} 条"
  grep -E '^--- (PASS|FAIL|SKIP)' /tmp/import-tx.log | sed 's/^/   /'
  exit 1
fi
echo "   ✅ 导入：凭证失败整体回滚 / 半成品能补齐 / 补齐补上探测快照且不覆盖 warning / 坏 base_url 被拒"
echo "   ✅ 渠道：PATCH 与 POST 共用地址校验 / 尾斜杠规范化 + 019 唯一约束 / 建渠道与探测快照同生共死"

echo
echo "✅ 迁移集成测试全部通过"
