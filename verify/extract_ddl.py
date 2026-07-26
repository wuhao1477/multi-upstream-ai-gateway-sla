#!/usr/bin/env python3
"""从 docs/dev/02-data-model.md 抽取全部 DDL 到 verify/ddl-extracted.sql。

切分用括号深度而非简单按分号 split —— 表体内的注释含分号会切错
（第一版就是这么只抽出 4 张表的）。
"""
import re

SRC = "docs/dev/02-data-model.md"
OUT = "verify/ddl-extracted.sql"

src = open(SRC).read()
fence = re.compile(r"```sql\n(.*?)```", re.S)
sql = "\n".join(m.group(1) for m in fence.finditer(src))

stmts, buf, depth = [], [], 0
for line in sql.split("\n"):
    code = re.sub(r"--.*$", "", line)          # 去行内注释，免得干扰计数
    if not buf and not code.strip():
        continue
    buf.append(line)
    depth += code.count("(") - code.count(")")
    if depth <= 0 and code.rstrip().endswith(";"):
        stmts.append("\n".join(buf))
        buf, depth = [], 0

# 只要 DDL；排除事务骨架里带 :参数 的伪 SQL
ddl = [
    s for s in stmts
    if re.match(r"^\s*(CREATE|ALTER)\s", s, re.I) and not re.search(r"^\s*:", s, re.M)
]

# 守卫：```sql 块里若残留未闭合内容（没有以分号收尾的片段），它会静默吞掉
# 后续语句 —— 第 28 轮就这么丢过一整张 data_policies 建表。宁可报错也不静默。
if buf:
    raise SystemExit(
        "❌ ```sql 块内有未闭合语句（缺分号或括号不平衡），它会吞掉后面的 DDL：\n"
        + "\n".join(buf[:6])
        + "\n\n若这是说明性片段而非完整语句，请改用 ```text 围栏。"
    )

open(OUT, "w").write("\n\n".join(ddl) + "\n")
print(f"抽出 {len(ddl)} 条 DDL → {OUT}")
