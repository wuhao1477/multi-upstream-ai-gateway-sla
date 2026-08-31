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
import re
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


# 致命错误的归类。**判定顺序是这个函数的全部要点**，改动前先读完这段。
#
# ⚠️ 原先第一个分支是 `if "401" in reason or "鉴权" in reason`，而 sync.go:110
# 把**所有** Authenticate 失败都包成 `鉴权失败: %w` —— 于是这个分支吃掉了一切，
# 下面的"连接超时/不可达"分支对 fan-out 失败**永远不可达**。后果不是数字难看，
# 而是**报告在撒谎**：2026-08-30 那轮把 `TLS handshake timeout`（渠道 3 redacted-channel-03）
# 和 `connection refused`（渠道 1 夹具）都记成"凭证失效"，并列进 need_manual
# 让人去重登。而渠道 3 的令牌与 all-api-hub 导出里那把**逐字节相同**（sha256
# 前 12 位 [redacted fingerprint] 两边一致），同一轮 ui-stack.sh 拿它跑 58 项全绿 ——
# 让运维去重登一把好令牌，是把人派去修一个不存在的问题。
#
# 所以传输层证据必须**先于**任何鉴权判定：底层连不上时，"鉴权失败"只是调用栈
# 最外层的包装词，不是失败原因。这与 §5.15 那个 ErrPrecondition 是同一类错误
# —— 都是把"没走到上游"误报成"上游/凭证有问题"。
_TRANSPORT = (
    "TLS handshake timeout", "connection refused", "no such host",
    "i/o timeout", "context deadline exceeded", "connection reset",
    "EOF", "server misbehaving", "network is unreachable",
)
# Cloudflare 边缘错误码：站点自己的源挂了或隧道断了，与我方凭证无关。
# 1033=隧道未找到、520/521/522/523/524=源不可达/超时。这些站重登也没用。
_EDGE_CODES = ("返回 520", "返回 521", "返回 522", "返回 523", "返回 524",
               "返回 530", "error-1033", "Error 1033")


def classify_fatal(reason: str) -> str:
    if "未登记采集凭证" in reason:
        return "未登记凭证"
    # 「需人工重登」必须在传输判定**之前**，且必须独立成类。
    #
    # ⚠️ 这一类是 2026-08-30 修 RefreshLead 自锁（P1-evidence §5.15）之后才出现的
    # 形态：13 条 sub2api 里 11 条现在报 `ErrPrecondition: 无 refresh_token 可用
    # （需人工重登：ErrNeedsRelogin）`。它既不含 "401" 也不含 "鉴权" —— 第一版
    # 分类改好之后，这 11 条**全部掉进兜底、进而被算成 retryable**，而它们恰恰是
    # 这批里最确定需要人手的：库里没有 refresh_token，重试一万次也不会变好。
    # 把它们记成"下一轮重试"，等于让这 11 个站永久停在坏状态而无人过问。
    #
    # 放在传输判定之前的理由：ErrPrecondition 的语义就是**没走到上游**
    # （collector.go:74-81），所以此时不存在传输层证据可言，先判它不会掩盖网络问题。
    if "需人工重登" in reason or "需要重新登录" in reason:
        return "需人工重登（无 refresh_token）"
    if any(k in reason for k in _TRANSPORT):
        return "网络/传输失败（与凭证无关）"
    if any(k in reason for k in _EDGE_CODES):
        return "上游站点自身不可达（CDN 边缘 5xx）"
    if "无预期字段" in reason:
        return "鉴权通过但响应形态不符（需确认站型）"
    if "401" in reason or "鉴权" in reason:
        return "凭证失效/鉴权失败"
    # 兜底键**必须去掉渠道号等每站不同的片段**，否则"分布"会碎成一堆计数 1。
    # 实测过一次：11 条同因失败因为串里带「（sub2api/渠道 12）」而排成 11 行，
    # 一个本该一眼看出的共性变成需要人眼归并的噪声。
    return "其它: " + re.sub(r"（[^）]*渠道\s*\d+[^）]*）", "（…）", reason)[:60]


# 只有这两类才是 FR-011 说的"转人工录入"。网络与边缘 5xx 是**重试**对象，
# 混进同一张单子会让人工清单虚高，而虚高的清单没人会逐条看完。
_MANUAL_CLASSES = ("凭证失效/鉴权失败", "未登记凭证",
                   "需人工重登（无 refresh_token）",
                   "鉴权通过但响应形态不符（需确认站型）")


def main():
    if not TOKEN:
        sys.exit("需要 ADMIN_TOKEN")

    code, data = api("/admin/channels")
    if code != 200:
        sys.exit(f"列渠道失败：{code} {data}")
    allch = data.get("items") or []
    if not allch:
        sys.exit("库中没有渠道")

    # 只打**在纳管**的渠道。
    #
    # 2026-08-31 放弃了 20 个采不到的站（改 status=disabled）。若这里仍遍历全表，
    # 那 20 个站每轮都会被打一次 —— 20 次无谓的上游请求，20 条注定的 fatal，
    # 人工清单永远停在 20 条，而"放弃"这个动作在报告上看不出任何效果。
    #
    # 端点侧也挡了（停用即 422、不触达上游），两层都有是刻意的：这里不打是
    # 为了不浪费一轮 60 秒的上游往返，端点那层是为了让"已停用"成为可依赖的事实。
    channels = [c for c in allch if c.get("status") != "disabled"]
    skipped = [c for c in allch if c.get("status") == "disabled"]
    if not channels:
        sys.exit(f"{len(allch)} 个渠道全部处于停用态，没有可采集的渠道")

    print(f"对 {len(channels)} 个在纳管渠道跑全量 sync，并发 {CONCURRENCY}…")
    if skipped:
        # 跳过必须**吵**：静静少打 20 个站会让"覆盖率 100%"读成"全都采到了"。
        print(f"⚠️ 另有 {len(skipped)} 个渠道已停用、不在本轮采集范围内：")
        for c in skipped:
            print(f"     #{c['id']:<4} {c['name'][:24]:<26} {c.get('disabled_reason','')[:44]}")
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
            r["fatal_class"] = klass = classify_fatal(r["fatal"])
            fatal_reasons[klass] += 1
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

    # 按 FR-011：采不到的必须列出来，运维据此决定人工录入。
    # **但只列真需要人手的那几类**（见 _MANUAL_CLASSES）：网络与边缘 5xx 归
    # retryable，它们下一轮可能自己好，派人去重登纯属白跑。两张单子都产出，
    # 于是"这轮到底几个站要人管"与"几个站只是当时网络不好"分得开。
    def entry(r):
        return {"name": r["name"], "base_url": r["base_url"],
                "family": r["site_family"], "klass": r["fatal_class"],
                "reason": r["fatal"][:160]}

    need_manual = [entry(r) for r in results
                   if r["fatal"] and r["fatal_class"] in _MANUAL_CLASSES]
    retryable = [entry(r) for r in results
                 if r["fatal"] and r["fatal_class"] not in _MANUAL_CLASSES]
    if need_manual:
        print(f"\n【需人工录入或修凭证的站点】共 {len(need_manual)} 个，前 10：")
        for m in need_manual[:10]:
            print(f"  {m['name'][:22]:24} {m['reason'][:70]}")
    if retryable:
        print(f"\n【本轮不可达但无需人工的站点】共 {len(retryable)} 个"
              f"（网络/CDN 边缘，下一轮重试）：")
        for m in retryable:
            print(f"  {m['name'][:22]:24} {m['klass']:24} {m['reason'][:50]}")

    with open(OUT, "w") as f:
        json.dump({
            "generated_at": time.strftime("%Y-%m-%d %H:%M:%S"),
            "channels": len(results),
            # 归档里必须留下"这轮少打了谁"，否则日后读到 channels:45 会以为
            # 真库只有 45 个渠道，而实际是 65 个里有 20 个被放弃纳管。
            "skipped_disabled": [
                {"id": c["id"], "name": c["name"],
                 "disabled_reason": c.get("disabled_reason", "")}
                for c in skipped
            ],
            "elapsed_seconds": round(elapsed, 1),
            "matrix": {f"{f}|{c}": dict(v) for (f, c), v in matrix.items()},
            "per_channel": per_channel_ok,
            "fatal_reasons": dict(fatal_reasons),
            "need_manual": need_manual,
            "retryable": retryable,
            "details": results,
        }, f, ensure_ascii=False, indent=2)
    print(f"\n明细已写 {OUT}（不含任何凭证）")


if __name__ == "__main__":
    main()
