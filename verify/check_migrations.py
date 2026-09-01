#!/usr/bin/env python3
"""校验 migrations/ 与 docs/dev/02-data-model.md 的对象集合一致。

**为什么需要这道检查**：02 §9.1bis 规则 3 定「以 migrations 为准」，于是两边
必然存在漂移风险 —— 改了文档忘记加迁移（新环境缺表），或加了迁移忘记回写文档
（文档不再可信，而它是 04/05/09 全部规则的引用对象）。

比对的是**对象集合与列集合**，不是逐字节相同：迁移可以有额外的
`CREATE TABLE IF NOT EXISTS`、注释差异、以及 schema_migrations 这类自身用表。

两边**不再要求逐张相等**（016 起）：02 是全阶段的设计，迁移只建当前阶段真的用
的表。差集必须**逐字等于** DEFERRED_TABLES —— 见该常量上方的说明。

退出码非 0 即门禁失败。
"""
import re
import pathlib
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
MIG_DIR = ROOT / "migrations"

# 迁移系统自身的表，文档不该有
RUNNER_TABLES = {"schema_migrations"}

# 「02 有设计、迁移刻意不建」的**穷举**清单（016 放弃 M0"一次建全"）。
#
# 用穷举清单而不是"允许迁移少于文档"（⊆）：⊆ 会让**漏加迁移**这件事静默 ——
# 而那正是本脚本存在的唯一理由（新环境缺表，且要等到运行时才炸）。
# 下面用 `!=` 比对，于是两个方向都会红：
#   · 往 02 加了表却忘了写迁移 → 差集多出它，清单里没有 → 红
#   · 实现了某个延期表（加了 CREATE）却没从本清单删 → 差集少了它 → 红
#
# 索引**不列**：延期表上的索引不是一个独立决定，由 doc 侧的
# `CREATE INDEX … ON <表>` 现场推导（indexes_on）。手抄 18 个索引名只会带来
# 一类新错误 —— 名字抄错时它同时从两边消失，比对照样绿。
#
# 分组与理由见 migrations/016_drop_unbuilt_phase_tables.sql，不在这里重复。
DEFERRED_TABLES = {
    # P2/P3 运行面（P1 只保留上游采集与管理）
    "requests", "attempts", "attempt_usage", "ledger_outbox",
    "session_prefix_ledger", "probe_templates", "bindings", "cache_scopes",
    "model_aliases", "multiplier_versions", "routing_policies", "gateway_clients",
    # P2/P3 调度与健康域
    "binding_fault_domains", "fault_domains", "routing_policy_revisions",
    "sla_targets", "channel_models", "resource_health", "health_metric_windows",
    "quality_events", "cache_hit_windows", "capacity_claims", "canary_claims",
    "probe_budget_windows", "probe_claims",
    # P2 调用方配额与预留
    "client_daily_spend", "client_rate_window", "client_reservations",
    "reservation_adjustments", "auth_rejections",
    # P4 数据许可
    "data_policies",
    # P3 价格变更留痕
    "price_change_log",
    # P4 订阅台账域（15 §1.2）
    "subscription_quota_windows", "user_subscriptions",
    "subscription_waste_forecast", "subscription_plans",
    # P3 告警
    "alert_events",
    # 账本分区子表（母表保留；分区管理是运行期的事，02 §9.1）
    "requests_2026_08", "attempts_2026_08", "attempt_usage_2026_08",
}


def relations(sql: str) -> set[str]:
    """净效果下的表与视图名（不含 DOMAIN/INDEX）—— 即 information_schema.tables 该有的。

    给 test-migrate.sh 的 `--tables` 用：让"真库里有哪些表"这件事由**迁移文本
    算出来**，而不是脚本里写一个 `≥45` 的下界。下界的问题是每次阶段增删都要手改，
    而改错的方向总是"调大调小让它绿"。
    """
    rels = {
        m.group(1)
        for m in re.finditer(r"CREATE (?:TABLE|VIEW)(?: IF NOT EXISTS)? (\w+)", sql)
    }
    rels -= {m.group(1) for m in re.finditer(r"DROP TABLE (?:IF EXISTS )?(\w+)", sql)}
    return rels


def indexes_on(sql: str, tables: set[str]) -> set[str]:
    """sql 里建在 tables 上的索引名。

    `DROP TABLE … CASCADE` 不点名索引就把它们带走了，所以两边都得靠这个推导，
    否则迁移侧少了 18 个索引、文档侧还有，比对必红。
    """
    return {
        m.group(1)
        for m in re.finditer(
            r"CREATE (?:UNIQUE )?INDEX (?:IF NOT EXISTS )?(\w+)\s+ON\s+(\w+)", sql)
        if m.group(2) in tables
    }


def objects(sql: str) -> tuple[set[str], dict[str, set[str]]]:
    """返回 (对象名集合, 表名→列名集合)，**净效果**：建完又删的不算存在。

    净效果而不是"建过就算"：016 是先建（001~012）后删，只看 CREATE 会得出
    28 张表仍然存在，DROP 迁移就成了对门禁不可见的一次改动。
    """
    objs: set[str] = set()
    cols: dict[str, set[str]] = {}

    for m in re.finditer(r"CREATE (?:TABLE|VIEW|DOMAIN)(?: IF NOT EXISTS)? (\w+)", sql):
        objs.add(m.group(1))
    for m in re.finditer(r"CREATE (?:UNIQUE )?INDEX (?:IF NOT EXISTS )?(\w+)", sql):
        objs.add(m.group(1))

    # 建表体内的列
    for m in re.finditer(r"CREATE TABLE (?:IF NOT EXISTS )?(\w+)\s*\((.*?)\n\)", sql, re.S):
        tbl, body = m.group(1), m.group(2)
        cs = set()
        for line in body.split("\n"):
            line = re.sub(r"--.*$", "", line).strip()
            c = re.match(r"^([a-z_][a-z0-9_]*)\s+[A-Za-z]", line)
            if c and c.group(1).upper() not in (
                "PRIMARY", "UNIQUE", "CHECK", "FOREIGN", "CONSTRAINT", "EXCLUDE", "PARTITION"
            ):
                cs.add(c.group(1))
        cols[tbl] = cs

    # ALTER 加的列（一条 ALTER 可加多列）
    for m in re.finditer(r"ALTER TABLE (\w+)([^;]*);", sql, re.S):
        tbl, body = m.group(1), m.group(2)
        for c in re.finditer(r"ADD COLUMN (?:IF NOT EXISTS )?(\w+)", body):
            cols.setdefault(tbl, set()).add(c.group(1))
        for c in re.finditer(r"DROP COLUMN (?:IF EXISTS )?(\w+)", body):
            cols.setdefault(tbl, set()).discard(c.group(1))

    # 删表：连带索引闭包与列。索引要推导，因为 CASCADE 不点名它们。
    dropped = {
        m.group(1)
        for m in re.finditer(r"DROP TABLE (?:IF EXISTS )?(\w+)", sql)
    }
    if dropped:
        objs -= dropped | indexes_on(sql, dropped)
        # 列也要清掉：延期表的 DDL 在 02 里仍会随 P2 设计演进，而 001~012 是
        # 不可变基线。留着比列 = 改设计就红，且没有合法改法。
        for t in dropped:
            cols.pop(t, None)

    return objs, cols


def main() -> int:
    subprocess.run([sys.executable, str(ROOT / "verify/extract_ddl.py")],
                   check=True, capture_output=True)
    doc_sql = (ROOT / "verify/ddl-extracted.sql").read_text()

    files = sorted(MIG_DIR.glob("*.sql"))
    if not files:
        print("❌ migrations/ 为空")
        return 1
    mig_sql = "\n".join(f.read_text() for f in files)
    doc_objs, doc_cols = objects(doc_sql)
    mig_objs, mig_cols = objects(mig_sql)
    mig_objs -= RUNNER_TABLES
    for t in RUNNER_TABLES:
        mig_cols.pop(t, None)

    problems = []

    # 延期对象 = 清单里的表 + 它们在 02 里的索引闭包
    deferred_idx = indexes_on(doc_sql, DEFERRED_TABLES)
    expect_only_doc = DEFERRED_TABLES | deferred_idx

    # 自检：清单里的表名必须真的在 02 里，且索引闭包不能为空。
    # 少了这两条，一个抄错的表名 / 一个失效的索引正则都会让下面的比对静默变绿。
    ghost = DEFERRED_TABLES - doc_objs
    if ghost:
        problems.append(
            "DEFERRED_TABLES 里这些表名在 02 中不存在（抄错了，或 02 已删掉它们）: "
            + ", ".join(sorted(ghost)))
    if not deferred_idx:
        problems.append(
            "延期表的索引闭包推导为空 —— indexes_on 的正则失效了？"
            "（这会让删表少带走索引的错误变成绿的）")

    only_doc = doc_objs - mig_objs
    if only_doc != expect_only_doc:
        missing = only_doc - expect_only_doc
        stale = expect_only_doc - only_doc
        if missing:
            problems.append(
                "文档有、迁移无，且不在延期清单里（新环境会缺这些对象，运行时才炸）: "
                + ", ".join(sorted(missing)))
        if stale:
            problems.append(
                "延期清单里的对象迁移其实建了 —— 实现了就该从 DEFERRED_TABLES 删掉，"
                "否则清单不再是「当前阶段建了什么」的可信声明: "
                + ", ".join(sorted(stale)))
    only_mig = mig_objs - doc_objs
    if only_mig:
        problems.append(
            "迁移有、文档无（文档不再可信，而 04/05/09 都引用它）: "
            + ", ".join(sorted(only_mig)))

    for tbl in sorted(doc_cols.keys() & mig_cols.keys()):
        miss = doc_cols[tbl] - mig_cols[tbl]
        extra = mig_cols[tbl] - doc_cols[tbl]
        if miss:
            problems.append(f"{tbl}: 文档有、迁移无的列 → {', '.join(sorted(miss))}")
        if extra:
            problems.append(f"{tbl}: 迁移有、文档无的列 → {', '.join(sorted(extra))}")

    if problems:
        print(f"❌ 迁移与文档不一致（{len(problems)} 处）")
        for p in problems:
            print("   " + p)
        print("\n修法：schema 变更须**同时**新增迁移文件与回写 02（02 §9.1bis 规则 3）。")
        return 1

    print(f"✅ 迁移与文档一致（{len(files)} 个文件 / {len(mig_objs)} 个对象 / "
          f"{sum(len(v) for v in mig_cols.values())} 列；"
          f"另有 {len(DEFERRED_TABLES)} 表 + {len(deferred_idx)} 索引按 016 延期）")
    return 0


if __name__ == "__main__":
    # `--relations`：打印迁移净效果下的表与视图名，一行一个。
    # test-migrate.sh 用它跟真库逐个比对，于是"真库该有哪些表"不再是脚本里的一个数字。
    if len(sys.argv) > 1 and sys.argv[1] == "--relations":
        mig = "\n".join(f.read_text() for f in sorted(MIG_DIR.glob("*.sql")))
        print("\n".join(sorted(relations(mig))))
        sys.exit(0)
    sys.exit(main())
