#!/usr/bin/env bash
# DDL 可执行性门禁（对应 02 §9.1bis）
#
# 从 docs/dev/02-data-model.md 抽取全部 sql 代码块，按括号深度切分语句，
# 筛出 CREATE/ALTER，在真实 postgres:16 上以 ON_ERROR_STOP=1 执行。
#
# 边界：只验证「建表能否跑通」。不验证分区子表创建、分区表上的外键行为、
#       以及事务骨架里带 :参数 的伪 SQL —— 后者须在 M0 用真实查询覆盖。
set -euo pipefail
cd "$(dirname "$0")/.."

python3 verify/extract_ddl.py

docker rm -f ddlpg >/dev/null 2>&1 || true
docker run -d --name ddlpg -e POSTGRES_PASSWORD=x -e POSTGRES_DB=sla postgres:16 >/dev/null
for _ in $(seq 1 60); do
  docker exec ddlpg pg_isready -U postgres >/dev/null 2>&1 && break
  sleep 1
done
sleep 3

docker exec -i ddlpg psql -U postgres -d sla -v ON_ERROR_STOP=1 < verify/ddl-extracted.sql >/dev/null
echo "✅ DDL 在 postgres:16 上执行通过"
echo -n "public schema 对象数: "
docker exec ddlpg psql -U postgres -d sla -tAc \
  "select count(*) from information_schema.tables where table_schema='public'"

# 回归：第18轮 critical —— 匿名 401（两列全 NULL）必须可写入，且同分钟只聚合一行
docker exec ddlpg psql -U postgres -d sla -v ON_ERROR_STOP=1 -q -c "
INSERT INTO auth_rejections(window_start, gateway_client_id, secret_prefix, reason, count)
VALUES (date_trunc('minute', now()), NULL, NULL, 'no_credential', 1);
INSERT INTO auth_rejections(window_start, gateway_client_id, secret_prefix, reason, count)
VALUES (date_trunc('minute', now()), NULL, NULL, 'no_credential', 1)
ON CONFLICT (window_start, gateway_client_id, secret_prefix, reason)
DO UPDATE SET count = auth_rejections.count + 1;"
RESULT=$(docker exec ddlpg psql -U postgres -d sla -tAc \
  "select count(*)||'/'||sum(count) from auth_rejections")
if [ "$RESULT" = "1/2" ]; then
  echo "✅ 匿名 401 聚合语义正确（1 行 / 计数 2）"
else
  echo "❌ 匿名 401 聚合异常: $RESULT（期望 1/2）"; docker rm -f ddlpg >/dev/null; exit 1
fi

docker rm -f ddlpg >/dev/null
