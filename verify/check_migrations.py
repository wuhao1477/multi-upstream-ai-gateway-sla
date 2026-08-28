#!/usr/bin/env python3
"""校验 migrations/ 与 docs/dev/02-data-model.md 的对象集合一致。

**为什么需要这道检查**：02 §9.1bis 规则 3 定「以 migrations 为准」，于是两边
必然存在漂移风险 —— 改了文档忘记加迁移（新环境缺表），或加了迁移忘记回写文档
（文档不再可信，而它是 04/05/09 全部规则的引用对象）。

比对的是**对象集合与列集合**，不是逐字节相同：迁移可以有额外的
`CREATE TABLE IF NOT EXISTS`、注释差异、以及 schema_migrations 这类自身用表。

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


def objects(sql: str) -> tuple[set[str], dict[str, set[str]]]:
    """返回 (对象名集合, 表名→列名集合)。"""
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

    return objs, cols


def main() -> int:
    subprocess.run(["python3", str(ROOT / "verify/extract_ddl.py")],
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

    only_doc = doc_objs - mig_objs
    if only_doc:
        problems.append(
            "文档有、迁移无（新环境会缺这些对象）: " + ", ".join(sorted(only_doc)))
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
          f"{sum(len(v) for v in mig_cols.values())} 列）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
