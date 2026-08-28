#!/usr/bin/env python3
"""对库中全部渠道跑一次全量 sync，产出覆盖率报告（P1 退出标准 ②）。

为什么需要它：单元测试与 mock 只能证明"代码按我理解的协议工作"，
证明不了"真实的 ~20 个上游到底采不采得到"。15 T6 把这件事定为
开工前隐患之一，ISSUE-005 把它提前到 P1 —— 采不到的按 FR-011 转人工录入，
而"哪些采不到"必须有据可查，否则运维不知道该手填哪几个。

用法：
    ADMIN_TOKEN=... BASE=http://127.0.0.1:18090 python3 verify/coverage_report.py

输出：/tmp/p1-coverage.json（明细）+ stdout（汇总表）
**不含任何凭证**：只记站名、地址、逐项状态。
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request
from collections import Counter, defaultdict
from concurrent.futures import ThreadPoolExecutor

BASE = os.environ.get("BASE", "http://127.0.0.1:18090")
TOKEN = os.environ.get("ADMIN_TOKEN", "")
OUT = os.environ.get("OUT", "/tmp/p1-coverage.json")

# 并发度刻意保守：一次 sync 会打同一站点的 5 个端点，而这些是**真实站点**。
# 并发过高既可能被当成扫描（04 §6 的风控顾虑），也会让报告里混入
# 因我方过载导致的假失败 —— 那会误导运维去修一个不存在的问题。
CONCURRENCY = int(os.environ.get("CONCURRENCY", "6"))


def api(path, method="GET", timeout=360):
    req = urllib.request.Request(
        BASE + path, method=method,
        headers={"Authorization": "Bearer " + TOKEN})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.load(e)
        except Exception:
            return e.code, {"error": e.reason}
    except Exception as e:
        return 0, {"error": f"{type(e).__name__}: {e}"}


def main():
    if not TOKEN:
        sys.exit("需要 ADMIN_TOKEN")

    code, data = api("/admin/channels")
    if code != 200:
        sys.exit(f"列渠道失败：{code} {data}")
    channels = data.get("items") or []
    if not channels:
        sys.exit("库中没有渠道")

    print(f"对 {len(channels)} 个渠道跑全量 sync，并发 {CONCURRENCY}…")
    print("（每站会打 5 个端点，站内已按 collector_request_interval_ms 限速）\n")

    results = []
    done = [0]
    start = time.time()

    def sync_one(ch):
        code, res = api(f"/admin/channels/{ch['id']}/sync", method="POST")
        done[0] += 1
        if done[0] % 10 == 0:
            print(f"  …{done[0]}/{len(channels)}  ({time.time()-start:.0f}s)")
        return {
            "channel_id": ch["id"],
            "name": ch["name"],
            "base_url": ch["base_url"],
            "site_family": ch["site_family"],
            "http_code": code,
            "items": res.get("items") or [],
            # 整体失败（鉴权/连接）时 sync 返回的是错误而非逐项结果
            "fatal": res.get("error", "") if code != 200 else "",
        }

    with ThreadPoolExecutor(max_workers=CONCURRENCY) as pool:
        for r in pool.map(sync_one, channels):
            results.append(r)

    elapsed = time.time() - start

    # ── 汇总 ──
    # 按 (家族, 能力) 统计各状态，这是报告的核心：
    # "NewAPI 系的 pricing 有几成采得到" 才是可行动的信息，
    # 而"总体成功率 62%"没法指导任何操作。
    matrix = defaultdict(Counter)
    fatal_reasons = Counter()
    per_channel_ok = Counter()

    for r in results:
        fam = r["site_family"]
        if r["fatal"]:
            reason = r["fatal"]
            # 归类致命错误：鉴权失败 vs 连不上 vs 其它
            if "401" in reason or "鉴权" in reason:
                fatal_reasons["凭证失效/鉴权失败"] += 1
            elif "未登记采集凭证" in reason:
                fatal_reasons["未登记凭证"] += 1
            elif any(k in reason for k in ("timeout", "Timeout", "超时",
                                           "deadline", "connect")):
                fatal_reasons["连接超时/不可达"] += 1
            else:
                fatal_reasons["其它: " + reason[:60]] += 1
            per_channel_ok[fam + "|fatal"] += 1
            continue

        ok_items = 0
        for it in r["items"]:
            matrix[(fam, it["capability"])][it["status"]] += 1
            if it["status"] == "ok":
                ok_items += 1
        # 一个渠道算"可用"的判据：账号或价格至少一项采到
        # —— 这两项是资产台账的最小有用集
        caps = {it["capability"]: it["status"] for it in r["items"]}
        if caps.get("account") == "ok" or caps.get("pricing") == "ok":
            per_channel_ok[fam + "|usable"] += 1
        else:
            per_channel_ok[fam + "|unusable"] += 1

    print("\n" + "=" * 74)
    print(f"全量 sync 完成，{len(results)} 个渠道，耗时 {elapsed:.0f}s")
    print("=" * 74)

    print("\n【逐家族 × 能力的采集成功率】")
    caps_order = ["account", "groups", "keys", "pricing",
                  "model_catalog", "subscription_quotas"]
    fams = sorted({f for f, _ in matrix})
    for fam in fams:
        print(f"\n  {fam}:")
        for cap in caps_order:
            c = matrix.get((fam, cap))
            if not c:
                continue
            total = sum(c.values())
            ok = c.get("ok", 0)
            parts = [f"{k}={v}" for k, v in sorted(c.items())]
            rate = f"{ok/total*100:5.1f}%" if total else "   n/a"
            print(f"    {cap:22} {rate}  ({', '.join(parts)})")

    print("\n【渠道级可用性】（账号或价格至少一项采到即算可用）")
    for fam in sorted({k.split("|")[0] for k in per_channel_ok}):
        usable = per_channel_ok[fam + "|usable"]
        unusable = per_channel_ok[fam + "|unusable"]
        fatal = per_channel_ok[fam + "|fatal"]
        tot = usable + unusable + fatal
        print(f"  {fam:10} 可用 {usable:3}/{tot:3}  "
              f"（部分失败 {unusable}，整体失败 {fatal}）")

    if fatal_reasons:
        print("\n【整体失败的原因分布】（这些站需人工介入，FR-011）")
        for reason, n in fatal_reasons.most_common():
            print(f"  {n:3}  {reason}")

    # 按 FR-011：采不到的必须列出来，运维据此决定人工录入
    need_manual = [
        {"name": r["name"], "base_url": r["base_url"],
         "family": r["site_family"], "reason": r["fatal"][:120]}
        for r in results if r["fatal"]
    ]
    if need_manual:
        print(f"\n【需人工录入或修凭证的站点】共 {len(need_manual)} 个，前 10：")
        for m in need_manual[:10]:
            print(f"  {m['name'][:22]:24} {m['reason'][:70]}")

    with open(OUT, "w") as f:
        json.dump({
            "generated_at": time.strftime("%Y-%m-%d %H:%M:%S"),
            "channels": len(results),
            "elapsed_seconds": round(elapsed, 1),
            "matrix": {f"{f}|{c}": dict(v) for (f, c), v in matrix.items()},
            "per_channel": per_channel_ok,
            "fatal_reasons": dict(fatal_reasons),
            "need_manual": need_manual,
            "details": results,
        }, f, ensure_ascii=False, indent=2)
    print(f"\n明细已写 {OUT}（不含任何凭证）")


if __name__ == "__main__":
    main()
