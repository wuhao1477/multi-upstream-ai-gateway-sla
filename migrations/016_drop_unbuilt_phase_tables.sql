-- 放弃 M0 的「一次建全」：只保留当前阶段真的要用的表。
--
-- 决定（2026-08-30）：项目未上线，库可以随意改，于是"先把 53 张表全建出来、
-- P2/P3/P4 慢慢填"这个做法不再值得 —— 它的代价是**每次读 schema 的人都要先
-- 判断哪张表是活的**，而 28 张里一行都没有、一处代码都不碰。
--
-- ⚠️ 为什么是新增 016 DROP，而不是回去改 001~012：
--    migrate.go 的 checksum 契约 —— 已应用的迁移文件 checksum 不一致时直接
--    return error 且**不执行任何迁移**。真库（SLA_DB @ <internal-db-host>）已应用 15 个
--    文件，改基线会让那一栈起不来，而库里有 65 渠道 / 2782 目录行 / 65 条真凭证
--    （一次真实采集跑了 86 秒才采出来的数据集）。代价换来的只是新环境省几百毫秒
--    的"先 CREATE 再 DROP"。所以基线不可变契约保住，这里只往后加。
--
-- **设计没有被删**：02 里这 28 张表的 DDL、索引、事务骨架全部原样保留 ——
-- 它们是 P2/P3/P4 的设计，届时由各阶段自己的迁移 CREATE。
-- verify/check_migrations.py 的 DEFERRED 清单是这份"文档有、迁移无"的**穷举**声明，
-- 加表忘了加迁移、或给延期表加了迁移却没从清单删，两个方向都仍然会红。
--
-- 保留的 25 张里有 12 张 P1 代码同样不碰，是刻意留的（决策 B）：
--   · requests / attempts / attempt_usage / ledger_outbox / session_prefix_ledger
--     / probe_templates —— 它们是 FR-112「禁正文列」守卫与分区断言的**靶子**。
--     删了靶子，守卫就没得守，而 TestForbiddenColumnGuardActuallyWorks 专门反向
--     验过那道守卫不是空转。
--   · bindings / cache_scopes / model_aliases / multiplier_versions /
--     routing_policies / gateway_clients —— FK 闭包拉进来的。删它们要一并删掉
--     指向它们的外键，而那些外键长在保留表上（如 models ← model_aliases）。
--
-- CASCADE 是必须的：被删表之间有 26 条外键互指（如 binding_fault_domains →
-- fault_domains）。已核对**没有任何保留表指向被删表**，所以 CASCADE 不会
-- 悄悄删掉保留表上的约束 —— 那才是这条语句唯一的静默风险。

-- ── P2/P3 调度与健康域 ──
DROP TABLE IF EXISTS binding_fault_domains CASCADE;
DROP TABLE IF EXISTS fault_domains CASCADE;
DROP TABLE IF EXISTS routing_policy_revisions CASCADE;
DROP TABLE IF EXISTS sla_targets CASCADE;
DROP TABLE IF EXISTS channel_models CASCADE;
DROP TABLE IF EXISTS resource_health CASCADE;
DROP TABLE IF EXISTS health_metric_windows CASCADE;
DROP TABLE IF EXISTS quality_events CASCADE;
DROP TABLE IF EXISTS cache_hit_windows CASCADE;
DROP TABLE IF EXISTS capacity_claims CASCADE;
DROP TABLE IF EXISTS canary_claims CASCADE;
DROP TABLE IF EXISTS probe_budget_windows CASCADE;
DROP TABLE IF EXISTS probe_claims CASCADE;

-- ── P2 调用方配额与预留（gateway_clients 本身保留：FK 闭包）──
DROP TABLE IF EXISTS client_daily_spend CASCADE;
DROP TABLE IF EXISTS client_rate_window CASCADE;
DROP TABLE IF EXISTS client_reservations CASCADE;
DROP TABLE IF EXISTS reservation_adjustments CASCADE;
DROP TABLE IF EXISTS auth_rejections CASCADE;

-- ── P4 数据许可 ──
DROP TABLE IF EXISTS data_policies CASCADE;

-- ── P3 价格变更留痕（price_versions 本身保留：P1 采集就写它）──
DROP TABLE IF EXISTS price_change_log CASCADE;

-- ── P4 订阅台账域（订阅制整体推迟，15 §1.2）──
DROP TABLE IF EXISTS subscription_quota_windows CASCADE;
DROP TABLE IF EXISTS user_subscriptions CASCADE;
DROP TABLE IF EXISTS subscription_waste_forecast CASCADE;
DROP TABLE IF EXISTS subscription_plans CASCADE;

-- ── P3 告警 ──
DROP TABLE IF EXISTS alert_events CASCADE;

-- ── 账本分区子表 ──
--
-- 三张母表**保留**（见上方决策 B），只删这三个写死月份的子表。
-- 它们本就是 02 §9.1 的**示例**（"例：建 2026-08 分区"），而分区管理是运行期
-- 的事（后台定时任务或 pg_partman 提前建下月分区 + 滑动 DETACH+DROP）。
-- 留着一个 2026-08 的空分区不构成任何保护 —— 它 2026-09-01 就出窗口了。
--
-- ⚠️ **必须先 DETACH，不能直接 DROP ... CASCADE。** 实测（一次性 PG 16）：
--    PG 给"被引用方是分区表"的外键在**每个分区上各建一条子约束**，于是真库里
--    除了 attempt_usage_..._fkey → attempts 还有一条 ..._fkey1 → attempts_2026_08。
--    `DROP TABLE attempts_2026_08 CASCADE` 会顺着子约束把**母表间那条外键一起删掉**：
--        NOTICE: drop cascades to constraint attempt_usage_..._fkey on table attempt_usage
--    结果是 attempt_usage 上一条外键都不剩 —— P2 写账本时野 attempt_id 再没人挡，
--    而这事一声不响。CASCADE 的爆炸半径靠读 02 的 DDL 是看不出来的（那里写的是
--    `REFERENCES attempts`，子约束是 PG 自动派生的）。
--    DETACH 先把子约束摘掉，母表那条原名保留；这也正是 02 §9.1 说的退役方式。
--
-- ⚠️ 给 P2 的人：母表现在**没有任何分区**，第一次往 requests 插行会报
--    "no partition of relation found for row"。分区管理要在写账本之前落地，
--    这不是回归而是本来就欠的那一步（02 §9.1 的清理方式一栏依赖它）。
-- 先摘。放在 DO 里只为一件事：DETACH PARTITION **不支持 IF EXISTS**，
-- 而库若被手工摘过分区，硬跑会让整个迁移 return error（migrate.go 不吞异常）。
DO $$
DECLARE
  part text;
BEGIN
  FOREACH part IN ARRAY ARRAY['requests_2026_08', 'attempts_2026_08',
                              'attempt_usage_2026_08']
  LOOP
    IF EXISTS (SELECT 1 FROM pg_inherits i
               JOIN pg_class c ON c.oid = i.inhrelid
               WHERE c.relname = part) THEN
      EXECUTE format('ALTER TABLE %I DETACH PARTITION %I',
                     regexp_replace(part, '_2026_08$', ''), part);
    END IF;
  END LOOP;
END $$;

-- 再删。写成三条字面量而不是塞进上面的循环：verify/check_migrations.py 靠
-- `DROP TABLE IF EXISTS <名>` 的文本识别删表，EXECUTE format() 拼出来的它看不见，
-- 于是这三张会被当成"还在"，延期清单比对必红且原因难查。
DROP TABLE IF EXISTS requests_2026_08;
DROP TABLE IF EXISTS attempts_2026_08;
DROP TABLE IF EXISTS attempt_usage_2026_08;
