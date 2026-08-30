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

cleanup() { docker rm -f "$CT" >/dev/null 2>&1 || true; }
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

echo "── 1/6 首次迁移 ──"
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

echo "── 2/6 种子：可种子化的键全部灌入 ──"
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

echo "── 3/6 幂等：重复迁移 ──"
go run ./cmd/migrate -dsn "$DSN" >/dev/null
KEYS2=$(q "select count(*) from config_params where scope_type='global'")
[ "$KEYS2" = "$WANT" ] || { echo "❌ 重复迁移后键数变成 $KEYS2，种子不幂等"; exit 1; }
WANT_MIG=$(ls migrations/*.sql | wc -l | tr -d ' ')
APPLIED=$(q "select count(*) from schema_migrations")
[ "$APPLIED" = "$WANT_MIG" ] || { echo "❌ schema_migrations = $APPLIED，期望 $WANT_MIG"; exit 1; }
echo "   ✅ 重复执行不重复插入（键 $WANT、迁移记录 $WANT_MIG）"

echo "── 4/6 运维改过的值不被重启抹回 ──"
q "update config_params set param_value='99'::jsonb
    where param_key='catalog_missing_rounds'" >/dev/null
go run ./cmd/migrate -dsn "$DSN" >/dev/null
KEPT=$(q "select param_value#>>'{}' from config_params where param_key='catalog_missing_rounds'")
[ "$KEPT" = "99" ] || { echo "❌ 改过的值被抹回 $KEPT —— 那会让改配置只在重启前有效"; exit 1; }
echo "   ✅ 已有值不被覆盖"
q "update config_params set param_value='3'::jsonb
    where param_key='catalog_missing_rounds'" >/dev/null

echo "── 5/6 账本母表是分区表（ddl-check 不覆盖这项）──"
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

echo "── 6/6 CHECK 取值与站型注册表一致 ──"
# 017 收窄了 site_family 与 cred_type 的取值。这里断言"库里的取值范围 == 从注册表
# 推导出来的"，两个方向都会红：
#   · 加了一族却没配迁移放宽 → 该族的渠道**建不进来**，而约束冲突报在**写入时**，
#     不是启动时 —— 也就是新站型接好了、界面上选得到，一按创建才炸。
#   · 迁移放宽了却没人注册那一族 → 库比代码宽，那个值写进去之后 Lookup 拿不到注册，
#     采集侧报"无对应适配器"，停在只能靠人去 UPDATE 才能救的状态。
# 取值从 cmd/registry-dump 取而**不在这里写死**：写死的那份就是第二份家族清单，
# 而"漏加一族"正是本断言要查的事（同 1/6 不写 `≥45` 下界的理由）。
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

echo
echo "✅ 迁移集成测试全部通过"
