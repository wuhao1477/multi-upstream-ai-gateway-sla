#!/usr/bin/env python3
"""PM 文档一致性门禁（机器可判定部分）。

覆盖六类在本项目反复出现的缺陷：
  1 坏链
  2 ```sql 块内混入 Markdown（DDL 抽取会失败）
  3 DDL 顺序（前向外键 / 索引早于表）
  4 引用不存在的列
  5 **FR/AC 的一期二期标注互斥**  ← 传播遗漏的高发区
  6 一期/二期 AC 计数与里程碑表自洽
  7 **DDL 定义了但没有任何规则读写的列**  ← 与第 4 类方向相反

第 5 类是本项目最高频的缺陷模式：改了主表、漏了引用它的其它文档。

第 7 类是前 39 轮全部漏掉的盲区：一直只查「SQL 引用的列必须存在」，
从没查过反方向「建了的列必须有人用」。DDL 跑在规则前面 —— 列建好了、
索引建好了、保留策略也写了，但没有任何事务、worker 或端点碰它。
悬空读（读一张没人写的表）比列不存在更危险：它不报错，只是永远读到空。
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
# ⚠️ 列不一定在 CREATE TABLE 里 —— 本文档有几处走 `ALTER TABLE t ADD COLUMN c`
#    （如 price_versions.confirmed）。只看建表体会把真实存在的列误报成不存在，
#    进而诱使人去"修"一份本来正确的文档（第 41 轮踩到）。
altered = {}
for m in re.finditer(r"ALTER TABLE (\w+) ADD COLUMN (?:IF NOT EXISTS )?(\w+)", alltext):
    altered.setdefault(m.group(1), set()).add(m.group(2))

miss = []
for t, c in set(re.findall(r"`(\w+)\.(\w+)`", alltext)):
    if t in tables:
        if c in altered.get(t, ()):
            continue
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


# ── 5bis config_params 键清单规范 ───────────────────────
# 迁移种子与 /admin/config 白名单都按 09 §4bis 逐行生成，
# 合并行（`a / b`）或缩写键（`.x`）会直接漏键 → 运行时静默用默认零值。
cfg = open("docs/dev/09-admin-api.md").read()
seg = cfg[cfg.index("## 4bis."): cfg.index("## 5.")]
keybad = []
for line in seg.split("\n"):
    m = re.match(r"^\| `([^`]+)` \|", line)
    if not m:
        continue
    k = m.group(1)
    if "/" in k or " " in k:
        keybad.append(f"合并行: {k}")
    elif "." in k:
        keybad.append(f"缩写键(含点): {k}")
    elif not re.fullmatch(r"[a-z0-9_]+", k):
        keybad.append(f"非法字符: {k}")
report("config_params 键清单规范", keybad)
KEYS = {re.match(r"^\| `([^`]+)` \|", l).group(1)
        for l in seg.split("\n") if re.match(r"^\| `([^`]+)` \|", l)}
print(f"   （清单共 {len(KEYS)} 个键）")


# ── 5ter 跨文档引用：配置键 / 管理端点 ──────────────────
# 第 33 轮：12 引用 debug.capture.*、02 引用 retention.months、
# 14 引用 /admin/ledger/requests/{id} —— 三者都不在权威清单里。
# 这类"引用了不存在的东西"人工审查反复漏，做成检查。
ALLDEV = {f: open(f).read() for f in DEV}

ref_bad = []
for f, body in ALLDEV.items():
    # 07 记录的是 AxonHub 自身的管理端点（历史实测），不是我方 API
    if f.endswith("09-admin-api.md") or f.endswith("07-axonhub-runtime-probes.md"):
        continue
    # 只认明确的键引用形式：config_params['k'] / param_key='k'
    for m in re.finditer(r"config_params\['([a-z][a-z0-9_.]+)'\]|param_key='([a-z][a-z0-9_.]+)'", body):
        k = m.group(1) or m.group(2)
        if k not in KEYS:
            ref_bad.append(f"{os.path.basename(f)}: config_params 键 `{k}` 不在 09 §4bis 清单")
    for m in re.finditer(r"`(/admin/[a-z0-9/{}_-]+)`", body):
        ep = re.sub(r"\{[^}]+\}", "{}", m.group(1)).rstrip("/")
        known = {re.sub(r"\{[^}]+\}", "{}", e).rstrip("/")
                 for e in re.findall(r"`?(/admin/[a-z0-9/{}_-]+)", ALLDEV["docs/dev/09-admin-api.md"])}
        if ep not in known:
            ref_bad.append(f"{os.path.basename(f)}: 端点 {ep} 未登记在 09")
report("跨文档引用（配置键/端点）", sorted(set(ref_bad)))


# ── 5quater 事务骨架的两类反复出现的残留 ────────────────
# ① 块内 COMMIT：本项目所有多语句事务的提交与否都由应用层按返回值决定，
#    块内写死 COMMIT 会让"任一为 0 则 ROLLBACK"的规则失效（已出现 3 次）。
# ② 旧三值：dispatch 早期返回三值，改四值后各处措辞未同步（已出现 2 次）。
tx_bad = []
for f, body in ALLDEV.items():
    for m in re.finditer(r"```sql\n(.*?)```", body, re.S):
        blk = m.group(1)
        # 只报**自相矛盾**的：块内写死 COMMIT，同一块里却又写「应用层断言/否则 ROLLBACK」。
        # 正当的单一事务（无条件回滚语义）照常 COMMIT，不报。
        if "COMMIT;" in blk and re.search(r"(应用层断言|否则\s*ROLLBACK|一律\s*ROLLBACK)", blk):
            ln = body[:m.start()].count("\n") + 1
            tx_bad.append(f"{os.path.basename(f)}:{ln} 块内 COMMIT 与同块的「应用层断言/否则 ROLLBACK」矛盾")
    # dispatch 返回值早期是三值，改四值后各处措辞与旧字段名反复残留
    for pat, why in [(r"canary_ok", "旧字段名 canary_ok（应为 exp_ok）"),
                     (r"三值分支", "仍称「三值分支」（已改四值）"),
                     (r"(quota_ok|capacity_ok|exp_ok)[^\n]{0,80}三值", "返回值仍称三值")]:
        for m in re.finditer(pat, body):
            ln = body[:m.start()].count("\n") + 1
            tx_bad.append(f"{os.path.basename(f)}:{ln} {why}")
report("事务骨架残留（块内 COMMIT / 旧三值）", sorted(set(tx_bad)))


# ── 5quinquies 事务 SQL 引用的列是否存在于 DDL ───────────
# 第 36 轮：05 的告警写入 SQL 引用 alert_events.last_seen_at，而 DDL 没这列；
# 同一段还省略了无 DEFAULT 的 id。第 4 类只查 `表.列` 反引号写法，
# 覆盖不到 SQL 语句里的裸列名 → 补这一类。
DDL_COLS = {}
for tm in re.finditer(r"CREATE TABLE (\w+) \((.*?)\n\)", sql, re.S):
    cols = set(re.findall(r"^\s{2}(\w+)\s+[A-Za-z]", tm.group(2), re.M))
    DDL_COLS[tm.group(1)] = cols

col_bad = []
for f, body in ALLDEV.items():
    for m in re.finditer(r"```sql\n(.*?)```", body, re.S):
        blk = m.group(1)
        ln0 = body[:m.start()].count("\n") + 1
        # INSERT INTO t (a, b, c)
        for im in re.finditer(r"INSERT INTO (\w+)\s*\(([^)]*)\)", blk):
            t = im.group(1)
            if t not in DDL_COLS:
                continue
            for c in re.findall(r"\b([a-z_][a-z0-9_]*)\b", im.group(2)):
                if c not in DDL_COLS[t]:
                    col_bad.append(f"{os.path.basename(f)}:{ln0} INSERT {t}({c}) —— 该列不在 DDL")
        # UPDATE t ... SET a = ...
        for um in re.finditer(r"UPDATE (\w+)[^\n]*\n?\s*SET\s+(.*?)(?:\n\s*(?:FROM|WHERE|RETURNING)|$)",
                              blk, re.S):
            t = um.group(1)
            if t not in DDL_COLS:
                continue
            for c in re.findall(r"(?:^|,)\s*([a-z_][a-z0-9_]*)\s*=", um.group(2)):
                if c not in DDL_COLS[t]:
                    col_bad.append(f"{os.path.basename(f)}:{ln0} UPDATE {t} SET {c} —— 该列不在 DDL")
        # ON CONFLICT ... DO UPDATE SET —— 表名取自其所属的 INSERT INTO
        for dm in re.finditer(r"INSERT INTO (\w+).*?DO UPDATE SET\s+(.*?)(?:\n\s*(?:WHERE|RETURNING)|;)",
                              blk, re.S):
            t = dm.group(1)
            if t not in DDL_COLS:
                continue
            for c in re.findall(r"(?:^|,)\s*([a-z_][a-z0-9_]*)\s*=", dm.group(2)):
                if c not in DDL_COLS[t]:
                    col_bad.append(f"{os.path.basename(f)}:{ln0} {t} DO UPDATE SET {c} —— 该列不在 DDL")
# 同类：引用了没有 CREATE FUNCTION 的自定义函数；以及 PL/pgSQL 专有语法
PG_BUILTIN = {
    "count", "coalesce", "greatest", "least", "now", "date_trunc", "floor", "ceil",
    "max", "min", "sum", "avg", "abs", "nullif", "percentile_disc", "length",
    "to_char", "extract", "gen_random_uuid", "concat", "round", "sqrt",
}
DEFINED_FN = set(re.findall(r"CREATE (?:OR REPLACE )?FUNCTION (\w+)", sql))
for f, body in ALLDEV.items():
    for m in re.finditer(r"```sql\n(.*?)```", body, re.S):
        blk, ln0 = m.group(1), body[:m.start()].count("\n") + 1
        # ⚠️ 只在**表达式位置**认函数调用（= 前 / SET 后 / WHERE 中 / SELECT 列表），
        #    否则 `INSERT INTO t(col…)`、`REFERENCES t(id)`、`CREATE TABLE t (…)`
        #    都会被当成函数调用 —— 第一版就是这么报了 76 条全误报。
        KNOWN_TABLES = set(DDL_COLS) | {"pg_try_advisory_lock"}
        for fm in re.finditer(r"(?:=|,|\(|\bSET\b|\bWHEN\b|\bTHEN\b|\bAND\b|\bOR\b)\s*"
                              r"([a-z_][a-z0-9_]{2,})\s*\(", blk):
            fn = fm.group(1)
            if fn in PG_BUILTIN or fn in DEFINED_FN or fn in KNOWN_TABLES:
                continue
            if fn in {"select", "values", "case", "interval", "cast", "exists", "partition"}:
                continue
            col_bad.append(f"{os.path.basename(f)}:{ln0} 调用了未定义的函数 {fn}()")
        # 去掉 -- 注释后再判，否则「说明为什么不能用 SELECT INTO」的注释本身会命中
        code_only = "\n".join(re.sub(r"--.*$", "", l) for l in blk.split("\n"))
        if re.search(r"SELECT[^;]*?\bINTO\s+:", code_only, re.S):
            col_bad.append(f"{os.path.basename(f)}:{ln0} 用了 PL/pgSQL 专有的 `SELECT … INTO :var`，普通连接跑不了")
# 同类：一条语句里出现两个 WITH（第 38 轮：adjust 改造时留下的，SQL 直接不可执行）
for f, body in ALLDEV.items():
    for m in re.finditer(r"```sql\n(.*?)```", body, re.S):
        blk, ln0 = m.group(1), body[:m.start()].count("\n") + 1
        code = "\n".join(re.sub(r"--.*$", "", l) for l in blk.split("\n"))
        for stmt in re.split(r";\s*\n", code):
            if len(re.findall(r"^\s*WITH\b", stmt, re.M)) > 1:
                col_bad.append(f"{os.path.basename(f)}:{ln0} 同一条语句里出现两个 WITH —— 不可执行")

# 同类：INSERT INTO attempts 必须带 single_hop_est_usd（恢复结算依赖它，
# 漏了不会报错，只会在崩溃恢复时把该跳费用算成 0）
for f, body in ALLDEV.items():
    for m in re.finditer(r"INSERT INTO attempts\s*\(([^)]*)\)", body, re.S):
        if "single_hop_est_usd" not in m.group(1):
            ln0 = body[:m.start()].count("\n") + 1
            col_bad.append(f"{os.path.basename(f)}:{ln0} INSERT attempts 漏 single_hop_est_usd")

report("事务 SQL 引用的列/函数存在于 DDL", sorted(set(col_bad)))


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


# ── 7 DDL 建了但没有任何规则读写的列 ──────────────────
# 与第 5 类方向相反：那类查「引用的列存在吗」，这类查「存在的列被引用吗」。
# 悬空列 = 规则没写完；悬空读（读一张没人写的表）= 永远读到空且不报错。
m02 = open("docs/dev/02-data-model.md").read()
ddl_sql = "\n".join(re.findall(r"```sql\n(.*?)```", m02, re.S))

# 二期整表推迟（[PRD §2.1](../PRD.md)），其列未被引用属预期
PHASE2_TABLES = {"subscription_plans", "user_subscriptions", "subscription_waste_forecast",
                 "subscription_quota_windows"}
# 列名与 SQL 关键字/通用词同形，逐一核对成本高于收益，单独豁免并在此写明理由
NOISY = {"currency", "region", "enabled", "created_at", "updated_at", "id"}

tbl_cols, defining_blocks = {}, []
for m in re.finditer(r"CREATE TABLE (?:IF NOT EXISTS )?(\w+)\s*\((.*?)\n\)", ddl_sql, re.S):
    tbl, body = m.group(1), m.group(2)
    defining_blocks.append(body)
    for line in body.split("\n"):
        line = re.sub(r"--.*$", "", line).strip()
        c = re.match(r"^([a-z_][a-z0-9_]*)\s+[A-Za-z]", line)
        if c and c.group(1).upper() not in (
            "PRIMARY", "UNIQUE", "CHECK", "FOREIGN", "CONSTRAINT", "EXCLUDE", "PARTITION"
        ):
            tbl_cols.setdefault(c.group(1), set()).add(tbl)

alldocs = "".join(open(f).read() for f in glob.glob("docs/**/*.md", recursive=True))
defined_only = "\n".join(defining_blocks)

dead = []
for col, tbls in sorted(tbl_cols.items()):
    if col in NOISY or tbls <= PHASE2_TABLES:
        continue
    # 出现总数 - 建表体内出现数 = 规则/事务/端点里的真实引用数
    total = len(re.findall(r"\b" + re.escape(col) + r"\b", alldocs))
    in_def = len(re.findall(r"\b" + re.escape(col) + r"\b", defined_only))
    if total - in_def <= 0:
        dead.append(f"{'/'.join(sorted(tbls))}.{col} —— 建表外零引用，无规则读写")

report("DDL 列均有规则承载（无悬空列）", dead)


# ── 8 悬空读：SQL 里被读、却没有任何 SQL 写的表 ──────────────
# 第 40/42 轮各栽一次：§1.3 读 session_prefix_ledger 而全库无写入；
# 段二读 health_metric_windows 的「总体行」而段一的 GROUP BY 决定它永不产生。
# 这类缺陷不报错，只是永远读到空——比语法错难发现得多。
ALL_TABLES = set(re.findall(r"CREATE TABLE (?:IF NOT EXISTS )?(\w+)", ddl_sql))
sql_blocks = "\n".join(
    m.group(1) for f in DEV for m in re.finditer(r"```sql\n(.*?)```", open(f).read(), re.S))
# 建表/建索引本身不算「写」
body = re.sub(r"CREATE TABLE (?:IF NOT EXISTS )?\w+\s*\(.*?\n\)[^;]*;", "", sql_blocks, flags=re.S)

written = set(re.findall(r"(?:INSERT INTO|UPDATE)\s+(\w+)", body))
read = set(re.findall(r"(?:FROM|JOIN)\s+(\w+)", body))
# 不是所有写入都以 SQL 骨架形式出现，另有两条**已登记**的写入路径：
#   ① 采集器：04 的「写入映射」表逐行列出它写哪张表
#   ② 管理端点：09 的端点表里点名"落 `xxx`"的表
# 这两条也算有写入者；除此之外的只读表就是悬空读。
collector_written = set(re.findall(r"^\| `?(\w+)", open("docs/dev/04-collector-adapter.md").read(), re.M))
admin_written = set(re.findall(r"`(\w+)`", open("docs/dev/09-admin-api.md").read()))

dangle = []
for t in sorted(read & ALL_TABLES):
    if t in written or t in collector_written or t in admin_written or t in PHASE2_TABLES:
        continue
    dangle.append(f"{t} —— 被 SQL 读取，但全库没有任何 INSERT/UPDATE 写它")
report("无悬空读（读到的表都有人写）", dangle)


print()
if FAIL:
    print(f"共 {FAIL} 处问题")
    sys.exit(1)
print("全部通过")
