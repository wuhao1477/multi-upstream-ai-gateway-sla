#!/usr/bin/env python3
"""对 all-api-hub 导出的真实站点跑一次 Detect 探测，统计覆盖率。

这是 P1 退出标准里"全渠道覆盖率报告"的第一步（00 §3、15 T6）：
只打**公开端点**（/api/status 等），无鉴权、零成本、不动账号状态。

⚠️ 凭证纪律：本脚本**只读 site_url 与 site_type**，绝不读 access_token，
   输出里也不含任何凭证 —— 报告是要入库的（docs/acceptance/）。

用法：python3 verify/probe_real_sites.py <backup.json> [并发数]
"""
import concurrent.futures as cf
import json
import ssl
import sys
import time
import urllib.error
import urllib.request
from collections import Counter

# 探测顺序与 04 §2 一致：命中即停
PROBES = [
    ("newapi", "/api/status", ("quota_per_unit", "turnstile_check", "checkin_enabled")),
    ("sub2api", "/api/v1/settings/public", ("site_name", "turnstile_enabled")),
    ("asxs", "/api/public/site-config", ()),  # 判据是 ampmanager 指纹
]

TIMEOUT = 8
UA = "sla-gateway-probe/1.0 (P1 coverage report)"
# 内网/自签证书的站点不少，探测阶段不校验证书 —— 我们只判站型，不传凭证
CTX = ssl.create_default_context()
CTX.check_hostname = False
CTX.verify_mode = ssl.CERT_NONE


def fetch(url):
    req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=TIMEOUT, context=CTX) as r:
        return r.status, r.read(1 << 20)


def detect(site):
    """返回 (family, detail)。family 为 None 表示不可达或未识别。"""
    base = site["site_url"].rstrip("/")
    last_err = None
    for fam, path, markers in PROBES:
        try:
            status, raw = fetch(base + path)
        except urllib.error.HTTPError as e:
            last_err = f"HTTP {e.code}"
            continue
        except Exception as e:  # 超时/DNS/TLS/连接拒绝
            last_err = type(e).__name__
            continue
        if status != 200:
            last_err = f"HTTP {status}"
            continue
        text = raw.decode("utf-8", "replace")
        if fam == "asxs":
            if "ampmanager" in text:
                return "asxs", "ampmanager 指纹"
            continue
        try:
            obj = json.loads(text)
        except Exception:
            last_err = "非 JSON"
            continue
        data = obj.get("data") if isinstance(obj.get("data"), dict) else obj
        hit = [m for m in markers if m in data]
        if hit:
            extra = ""
            if fam == "newapi":
                qpu = data.get("quota_per_unit")
                shield = data.get("turnstile_check")
                extra = f"qpu={qpu} 开盾={bool(shield)}"
            return fam, (", ".join(hit[:2]) + (" | " + extra if extra else ""))
    return None, last_err or "全部端点未命中"


def main():
    path = sys.argv[1]
    workers = int(sys.argv[2]) if len(sys.argv) > 2 else 12

    d = json.load(open(path))
    accs = d["accounts"]["accounts"]
    # 只取探测所需字段 —— 凭证从一开始就不进入本脚本的数据流
    sites = [{"name": a.get("site_name", ""), "site_url": a.get("site_url", ""),
              "declared": a.get("site_type", ""), "health": a.get("health", {}).get("status")}
             for a in accs if a.get("site_url")]

    print(f"共 {len(sites)} 个站点，并发 {workers}，逐个探测公开端点…\n", file=sys.stderr)
    t0 = time.time()
    results = []
    with cf.ThreadPoolExecutor(max_workers=workers) as ex:
        futs = {ex.submit(detect, s): s for s in sites}
        for i, fut in enumerate(cf.as_completed(futs), 1):
            s = futs[fut]
            try:
                fam, detail = fut.result()
            except Exception as e:
                fam, detail = None, f"探测异常 {type(e).__name__}"
            results.append({**s, "detected": fam, "detail": detail})
            if i % 20 == 0:
                print(f"  …{i}/{len(sites)}", file=sys.stderr)

    elapsed = time.time() - t0
    ok = [r for r in results if r["detected"]]

    print(f"\n{'='*74}")
    print(f"探测完成：{len(ok)}/{len(results)} 可识别站型，耗时 {elapsed:.0f}s")
    print(f"{'='*74}\n")

    print("【探测出的站型分布】")
    for fam, n in Counter(r["detected"] for r in results).most_common():
        print(f"  {str(fam or '不可达/未识别'):20} {n:>4}")

    print("\n【探测结果 vs 导出声明】")
    # all-api-hub 的 site_type 命名与我们的家族名映射
    alias = {"new-api": "newapi", "sub2api": "sub2api", "anyrouter": "?",
             "Rix-Api": "newapi", "unknown": "?"}
    agree = disagree = unknown_declared = 0
    mismatches = []
    for r in results:
        if not r["detected"]:
            continue
        want = alias.get(r["declared"], "?")
        if want == "?":
            unknown_declared += 1
        elif want == r["detected"]:
            agree += 1
        else:
            disagree += 1
            mismatches.append(r)
    print(f"  一致            {agree:>4}")
    print(f"  不一致          {disagree:>4}")
    print(f"  导出侧未分类    {unknown_declared:>4}（anyrouter/unknown，我们仍能探出家族）")
    if mismatches:
        print("\n  不一致明细（我们的探测 vs 导出声明）：")
        for r in mismatches[:10]:
            print(f"    {r['name'][:18]:20} 探测={r['detected']:8} 声明={r['declared']}")

    print("\n【不可达/未识别的原因分布】")
    for reason, n in Counter(
            r["detail"] for r in results if not r["detected"]).most_common(10):
        print(f"  {str(reason)[:44]:46} {n:>4}")

    print("\n【NewAPI 系的 quota_per_unit 分布】（必须逐站读取，不可写死）")
    qpus = Counter()
    shielded = 0
    for r in results:
        if r["detected"] == "newapi" and "qpu=" in (r["detail"] or ""):
            seg = r["detail"].split("qpu=")[1]
            qpu = seg.split()[0]
            qpus[qpu] += 1
            if "开盾=True" in r["detail"]:
                shielded += 1
    for v, n in qpus.most_common():
        print(f"  quota_per_unit={v:<12} {n:>4} 站")
    print(f"  其中开启 turnstile 人机验证：{shielded} 站（服务端采集不可行，04 §6）")

    json.dump(results, open("/tmp/probe-results.json", "w"),
              ensure_ascii=False, indent=1)
    print("\n明细已写 /tmp/probe-results.json（不含任何凭证）")


if __name__ == "__main__":
    main()
