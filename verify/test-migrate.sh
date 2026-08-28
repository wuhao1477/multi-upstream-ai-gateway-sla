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

echo "── 1/5 首次迁移 ──"
go run ./cmd/migrate -dsn "$DSN"

TABLES=$(q "select count(*) from information_schema.tables where table_schema='public'")
echo "   public 表/视图数: $TABLES"
[ "$TABLES" -ge 45 ] || { echo "❌ 对象数偏少（期望 ≥45）"; exit 1; }

echo "── 2/5 种子：可种子化的键全部灌入 ──"
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

echo "── 3/5 幂等：重复迁移 ──"
go run ./cmd/migrate -dsn "$DSN" >/dev/null
KEYS2=$(q "select count(*) from config_params where scope_type='global'")
[ "$KEYS2" = "$WANT" ] || { echo "❌ 重复迁移后键数变成 $KEYS2，种子不幂等"; exit 1; }
WANT_MIG=$(ls migrations/*.sql | wc -l | tr -d ' ')
APPLIED=$(q "select count(*) from schema_migrations")
[ "$APPLIED" = "$WANT_MIG" ] || { echo "❌ schema_migrations = $APPLIED，期望 $WANT_MIG"; exit 1; }
echo "   ✅ 重复执行不重复插入（键 $WANT、迁移记录 $WANT_MIG）"

echo "── 4/5 运维改过的值不被重启抹回 ──"
q "update config_params set param_value='99'::jsonb
    where param_key='catalog_missing_rounds'" >/dev/null
go run ./cmd/migrate -dsn "$DSN" >/dev/null
KEPT=$(q "select param_value#>>'{}' from config_params where param_key='catalog_missing_rounds'")
[ "$KEPT" = "99" ] || { echo "❌ 改过的值被抹回 $KEPT —— 那会让改配置只在重启前有效"; exit 1; }
echo "   ✅ 已有值不被覆盖"
q "update config_params set param_value='3'::jsonb
    where param_key='catalog_missing_rounds'" >/dev/null

echo "── 5/5 分区子表真实存在（ddl-check 不覆盖这项）──"
PARTS=$(q "select count(*) from pg_class c join pg_inherits i on c.oid=i.inhrelid
           where c.relname like '%_2026_08'")
[ "$PARTS" = "3" ] || { echo "❌ 分区子表 = $PARTS，期望 3（requests/attempts/attempt_usage）"; exit 1; }
echo "   ✅ 3 张分区子表已挂载"

# P1 三张新表与 upstream_keys 六列
for t in channel_groups group_models channel_model_catalog; do
  n=$(q "select count(*) from information_schema.tables where table_name='$t'")
  [ "$n" = "1" ] || { echo "❌ 缺 P1 表 $t"; exit 1; }
done
COLS=$(q "select count(*) from information_schema.columns where table_name='upstream_keys'
          and column_name in ('channel_group_id','remain_quota_usd','used_quota_usd',
                              'rpm_limit','concurrency_limit','quota_synced_at')")
[ "$COLS" = "6" ] || { echo "❌ upstream_keys 的 P1 列 = $COLS，期望 6"; exit 1; }
echo "   ✅ P1 三表 + upstream_keys 六列就位"

echo
echo "✅ 迁移集成测试全部通过"
