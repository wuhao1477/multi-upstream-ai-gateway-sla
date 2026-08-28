#!/usr/bin/env python3
"""把 02 的 DDL 切成按域分文件的初始迁移（**一次性引导脚本**）。

⚠️ 定位：只用于生成 `migrations/` 的**初始基线**。此后 `migrations/` 是
schema 的唯一真相源（02 §9.1bis 规则 3：「文档的 DDL 与 migrations 以后者
为准」），schema 变更一律**新增**迁移文件，不再重跑本脚本、不改既有文件
（已应用的迁移不可变——改了会让已部署环境与新环境 schema 不一致）。

漂移防护由 verify/check_migrations.py 承担（比对文档与迁移的对象集合）。
"""
import re
import pathlib
import subprocess

ROOT = pathlib.Path(__file__).resolve().parents[1]
OUT_DIR = ROOT / "migrations"

# 按域切分：值是该文件包含的**首个**对象名，遇到下一个域的首对象即切换。
# 顺序必须与文档一致（依赖链：被引用的表先建，02 §9.1bis）。
DOMAINS = [
    ("001_domains_and_registry", "usd_amount"),
    ("002_groups_and_catalog", "channel_groups"),
    ("003_policies_and_config", "routing_policies"),
    ("004_gateway_clients", "gateway_clients"),
    ("005_data_policies", "data_policies"),
    ("006_pricing", "price_versions"),
    ("007_ledger", "requests"),
    ("008_subscriptions", "subscription_plans"),
    ("009_health_and_claims", "resource_health"),
    ("010_collector_and_signals", "collector_credentials"),
    ("011_alerts", "alert_events"),
    ("012_partitions_and_outbox", "requests_2026_08"),
]

HEADERS = {
    "001_domains_and_registry": "金额域 + 资源注册域（02 §0.2/§1.1/§1.2）",
    "002_groups_and_catalog": "上游分组与模型目录（02 §1.3，交付阶段 P1）",
    "003_policies_and_config": "别名与策略域 + config_params（02 §2）",
    "004_gateway_clients": "网关调用方凭证与配额（02 §2bis，P2 才写入）",
    "005_data_policies": "数据许可域（02 §2ter，默认关闭）",
    "006_pricing": "价格版本域（02 §3）",
    "007_ledger": "请求与逐 Attempt 账本（02 §4，分区母表；P2 才写入）",
    "008_subscriptions": "订阅台账域（02 §5，P4；建表保留、本阶段不写入）",
    "009_health_and_claims": "健康/冷却/容量/canary/probe 占用（02 §6）",
    "010_collector_and_signals": "采集凭证/快照/余额信号（02 §7）",
    "011_alerts": "告警事件（02 §8）",
    "012_partitions_and_outbox": "月分区与账本 outbox（02 §9）",
}


def main() -> None:
    subprocess.run(["python3", str(ROOT / "verify/extract_ddl.py")], check=True,
                   capture_output=True)
    sql = (ROOT / "verify/ddl-extracted.sql").read_text()

    # ⚠️ 不能按 "\n\n" 切：语句体内本身含空行（如 attempts 的分段注释），
    #    那样会把一条建表切成好几段（首版就切出 127 条而 extract_ddl 是 107 条）。
    #    改用与 extract_ddl 相同的括号深度法，保证两侧语句数一致。
    stmts, buf, depth = [], [], 0
    for line in sql.split("\n"):
        code = re.sub(r"--.*$", "", line)
        if not buf and not code.strip():
            continue
        buf.append(line)
        depth += code.count("(") - code.count(")")
        if depth <= 0 and code.rstrip().endswith(";"):
            stmts.append("\n".join(buf).strip())
            buf, depth = [], 0
    if buf:
        raise SystemExit("❌ 抽取产物有未闭合语句，先修 02 的 sql 块")

    def first_object(stmt: str) -> str:
        m = re.search(
            r"CREATE (?:TABLE|VIEW|DOMAIN)(?: IF NOT EXISTS)? (\w+)|"
            r"ALTER TABLE (\w+)|CREATE (?:UNIQUE )?INDEX (\w+)", stmt)
        if not m:
            return ""
        return next(g for g in m.groups() if g)

    # 为每条语句定域
    marks = {name: obj for name, obj in DOMAINS}
    order = [name for name, _ in DOMAINS]
    buckets: dict[str, list[str]] = {n: [] for n in order}
    cur = order[0]
    for st in stmts:
        obj = first_object(st)
        for name in order:
            if obj == marks[name]:
                cur = name
                break
        buckets[cur].append(st)

    OUT_DIR.mkdir(exist_ok=True)
    total = 0
    for name in order:
        body = buckets[name]
        if not body:
            raise SystemExit(f"域 {name} 为空——切分标记 {marks[name]!r} 可能已改名")
        path = OUT_DIR / f"{name}.sql"
        header = (
            f"-- {HEADERS[name]}\n"
            f"-- 由 verify/split_migrations.py 从 docs/dev/02-data-model.md 生成初始基线。\n"
            f"-- ⚠️ 此后本文件**不可修改**（已应用的迁移不可变）；schema 变更请新增迁移文件。\n\n"
        )
        path.write_text(header + "\n\n".join(body) + "\n")
        total += len(body)
        print(f"  {path.name:34} {len(body):3} 条")
    print(f"共 {total} 条语句 → {len(order)} 个文件")


if __name__ == "__main__":
    main()
