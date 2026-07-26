#!/usr/bin/env python3
"""PM 文档一致性门禁（机器可判定部分）。

覆盖六类在本项目反复出现的缺陷：
  1 坏链
  2 ```sql 块内混入 Markdown（DDL 抽取会失败）
  3 DDL 顺序（前向外键 / 索引早于表）
  4 引用不存在的列
  5 **FR/AC 的一期二期标注互斥**  ← 传播遗漏的高发区
  6 一期/二期 AC 计数与里程碑表自洽

第 5 类是本项目最高频的缺陷模式：改了主表、漏了引用它的其它文档。
"""
import glob
import os
import re
import sys

FAIL = 0


def report(name, items):
    global FAIL
    if items:
        FAIL += len(items)
        print(f"❌ {name}（{len(items)}）")
        for i in items[:12]:
            print(f"   {i}")
        if len(items) > 12:
            print(f"   …… 另有 {len(items)-12} 条")
    else:
        print(f"✅ {name}")


DOCS = sorted(glob.glob("docs/**/*.md", recursive=True))
DEV = sorted(glob.glob("docs/dev/*.md")) + ["docs/PRD.md"]


# ── 1 坏链 ──────────────────────────────────────────────
bad = []
for f in DOCS:
    d = os.path.dirname(f)
    for m in re.finditer(r"\]\((\.\.?/[^)#]+)(#[^)]*)?\)", open(f).read()):
        t = os.path.normpath(os.path.join(d, m.group(1)))
        if not os.path.exists(t):
            bad.append(f"{f} -> {m.group(1)}")
report("坏链", bad)


# ── 2 SQL 块内 Markdown ─────────────────────────────────
md = []
for f in DOCS:
    for m in re.finditer(r"```sql\n(.*?)```", open(f).read(), re.S):
        for l in m.group(1).split("\n"):
            if l.strip().startswith((">", "|", "**", "#")):
                md.append(f"{f}: {l.strip()[:60]}")
report("SQL 块内 Markdown", md)


# ── 3 DDL 顺序（只扫 sql 块）────────────────────────────
src = open("docs/dev/02-data-model.md").read()
sql = "".join(m.group(1) for m in re.finditer(r"```sql\n(.*?)```", src, re.S))
defined, order_bad = set(), []
for m in re.finditer(
    r"CREATE TABLE (\w+)|REFERENCES (\w+)|CREATE (?:UNIQUE )?INDEX \w+\s+ON (\w+)", sql
):
    if m.group(1):
        defined.add(m.group(1))
    else:
        n = m.group(2) or m.group(3)
        if n not in defined:
            order_bad.append(f"{'REF' if m.group(2) else 'INDEX'} -> {n}")
report("DDL 顺序", order_bad)


# ── 4 引用不存在的列 ────────────────────────────────────
alltext = "".join(open(f).read() for f in DEV)
tables = set(re.findall(r"CREATE TABLE (\w+)", alltext))
miss = []
for t, c in set(re.findall(r"`(\w+)\.(\w+)`", alltext)):
    if t in tables:
        blk = re.search(r"CREATE TABLE " + t + r"\b.*?\n\)", alltext, re.S)
        if blk and not re.search(r"\b" + c + r"\b", blk.group(0)):
            miss.append(f"{t}.{c}")
miss = [m for m in sorted(set(miss)) if m != "requests.format"]  # 07 记录的是 AxonHub 自身表
report("引用不存在的列", miss)


# ── 5 FR/AC 分期标注互斥 ────────────────────────────────
# 在同一行里同时出现「编号」与「一期/二期」标注时记录该编号的归属主张。
PHASE1 = re.compile(r"(一期|M0|M1|M2|M3|M4)")
PHASE2 = re.compile(r"二期")
# 历史/更正/前瞻性叙述不构成"归属主张"
NEG = re.compile(r"(不再|已废止|曾|此前|原文|旧句|更正|推翻|移入一期|拉入一期|由二期|二期加|二期开启|二期启用|二期兑现|二期直接|二期按需|二期若|二期随|二期时|二期扩|推二期|移二期|留二期|二期再|供二期|便于二期|为二期)")

claims = {}  # id -> {phase: [(file, lineno, snippet)]}
for f in DEV:
    for ln, line in enumerate(open(f).read().split("\n"), 1):
        if not re.search(r"\b(?:FR|AC)-\d+", line):
            continue
        if NEG.search(line):          # 历史/更正性叙述不计入主张
            continue
        # 两段式绑定（行级过宽会误报，纯子句级又会漏报）：
        #   ① 子句内同时有编号与期别 → 直接绑定（高置信）
        #   ② 子句只有期别没有编号 → 绑定到全行中**距离最近**的编号
        # 例：「### ⏭ 二期（缓存进阶，FR-056）」→ ② 命中，FR-056 判为二期（真冲突）
        #     「AC-17、AC-28（⏭ AC-20~24 维持二期）」→ ① 只绑 AC-20/24，前面几个不受影响
        segs, pos = [], 0
        for seg in re.split(r"([｜|；。]|（|）|\(|\))", line):
            segs.append((pos, seg))
            pos += len(seg)
        id_pos = [(m.start(), m.group(1))
                  for m in re.finditer(r"\b((?:FR|AC)-\d+)\b", line)]

        def claim(ident, phase):
            claims.setdefault(ident, {}).setdefault(phase, []).append(
                f"{os.path.basename(f)}:{ln}")

        for start, seg in segs:
            p1, p2 = bool(PHASE1.search(seg)), bool(PHASE2.search(seg))
            # 否定式："一期不实现 / 一期不做 / 一期不建模 / 一期不承诺" 语义是**二期**
            if p1 and re.search(r"一期\s*\**\s*不(实现|做|建模|承诺|支持|启用|覆盖)", seg):
                p1, p2 = False, True
            if p1 == p2:
                continue
            phase = "一期" if p1 else "二期"
            ids = re.findall(r"\b((?:FR|AC)-\d+)\b", seg)
            if ids:
                for i2 in set(ids):
                    claim(i2, phase)
            elif id_pos:
                mk = (PHASE1 if p1 else PHASE2).search(seg)
                if not mk:          # 否定式改判后原期别词已不在本段，按段首定位
                    mk = re.search(r"一期|二期", seg)
                if not mk:
                    continue
                at = start + mk.start()
                nearest = min(id_pos, key=lambda t: abs(t[0] - at))
                claim(nearest[1], phase)

conflict = []
for i, ph in sorted(claims.items(), key=lambda x: (x[0][:2], int(x[0][3:]))):
    if len(ph) <= 1:
        continue
    # 只报**跨文档**冲突：同一行/同一文件内并提两期通常是合法讨论，
    # 而不同文档给出不同归属才是真正会让开发做错的传播遗漏。
    files = {loc.split(":")[0] for locs in ph.values() for loc in locs}
    if len(files) < 2:
        continue
    where = "；".join(f"{k}={','.join(sorted(set(v))[:2])}" for k, v in ph.items())
    conflict.append(f"{i} → {where}")
# ⚠️ 本项**只警告不阻断**：它按文本启发式判定，无法完全区分
#    「FR-103 一期做最小 webhook、二期做完整渠道」这类**合法拆分**
#    与「改了主表漏了引用文档」这类**真冲突**。命中后须人工判读。
if conflict:
    print(f"⚠️  FR/AC 分期标注疑似互斥（{len(conflict)}，需人工判读，不阻断）")
    for c in conflict:
        print(f"   {c}")
else:
    print("✅ FR/AC 分期标注互斥")


# ── 6 AC 计数自洽 ───────────────────────────────────────
m14 = open("docs/dev/14-acceptance-matrix.md").read()
cnt = []
head = m14[m14.index("| **M0 骨架**"): m14.index("| 合计（一期）")]
p1set = set()
for line in head.strip().split("\n"):
    for m in re.finditer(r"AC-(\d+(?:/\d+)*)", line):
        for x in m.group(1).split("/"):
            p1set.add(f"AC-{int(x):02d}")
declared = re.search(r"\| 合计（一期） \| \| \*\*(\d+)\*\* \|", m14)
if declared and int(declared.group(1)) != len(p1set):
    cnt.append(f"14 声明一期 {declared.group(1)} 条，里程碑表实际 {len(p1set)} 条")
p2 = set()
for m in re.finditer(r"⏭ \*\*二期[^|]*\| ([^|]*) \| (\d+) \|", m14):
    for x in re.findall(r"AC-\d+", m.group(1)):
        p2.add(x)
if p1set & p2:
    cnt.append(f"同一 AC 同时在一期与二期表：{sorted(p1set & p2)}")
if len(p1set) + len(p2) != 36:
    cnt.append(f"一期 {len(p1set)} + 二期 {len(p2)} ≠ 36")
report("AC 计数自洽", cnt)


print()
if FAIL:
    print(f"共 {FAIL} 处问题")
    sys.exit(1)
print("全部通过")
