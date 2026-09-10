#!/usr/bin/env python3
"""classify_fatal 的判定顺序自测。

为什么值得有这个文件：被测的**只是分支顺序**，而分支顺序错了不会抛异常、
不会让报告跑不出来 —— 它让报告安静地把 `TLS handshake timeout` 记成
"凭证失效"，再把那个站列进人工清单。这种错只能靠断言发现。

⚠️ 断言用的失败字符串**全部取自归档的真实报告**
（`docs/acceptance/coverage/p1-coverage-2026-08-{29,30}.json` 的 details[].fatal），
不是照 sync.go 的格式化串想象出来的。CLAUDE.md §1 的形态举证责任在这里同样成立：
夹具照实现者的理解写，就只能印证那个理解。
"""
import json
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).parent))
from coverage_report import _MANUAL_CLASSES, classify_fatal  # noqa: E402

ROOT = pathlib.Path(__file__).resolve().parent.parent
ARCHIVE = ROOT / "docs/acceptance/coverage"

# 取自真实报告的原串（截断处用 … 标出的地方是原样保留的前缀）。
REAL = [
    # 渠道 3 redacted-channel-03：令牌与 all-api-hub 导出逐字节相同，同轮 58 项验收全绿
    ('鉴权失败: 七个用户 ID 头名全部试探失败（04 §3.1 fan-out）: '
     'GET /api/user/self: Get "https://upstream-a.invalid/api/user/self": '
     'net/http: TLS handshake timeout',
     "网络/传输失败（与凭证无关）"),
    # 渠道 1 夹具：base_url 指向已删除的 mock 端口
    ('鉴权失败: 七个用户 ID 头名全部试探失败（04 §3.1 fan-out）: '
     'GET /api/user/self: Get "http://127.0.0.1:18099/api/user/self": '
     'dial tcp 127.0.0.1:18099: connect: connection refused',
     "网络/传输失败（与凭证无关）"),
    # sub2api 真 401（令牌过期，且库里无 refresh_token —— P1-evidence §5.15）
    ('鉴权失败: collector: 上游返回 401: GET /api/v1/auth/me',
     "凭证失效/鉴权失败"),
    # 修 RefreshLead 自锁之后才出现的形态：续期在触达上游之前就失败了。
    # 这是这批里最确定需要人手的一类，**必须不落进 retryable** ——
    # 库里没有 refresh_token，重试一万次也不会变好。
    ('凭证续期失败: 续期失败（sub2api/渠道 12）: collector: 采集前置条件不满足'
     '（未触达上游）: 无 refresh_token 可用（需人工重登：collector: 需要重新登录）',
     "需人工重登（无 refresh_token）"),
    # newapi 真 401
    ('鉴权失败: 七个用户 ID 头名全部试探失败（04 §3.1 fan-out）: '
     'collector: 上游返回 401: GET /api/user/self',
     "凭证失效/鉴权失败"),
    # 头名试出来了但响应形态不对 —— 站型可能不是声明的那个
    ('鉴权失败: 七个用户 ID 头名全部试探失败（04 §3.1 fan-out）: '
     '头名 neo-api-user 通过但响应无预期字段',
     "鉴权通过但响应形态不符（需确认站型）"),
    # Cloudflare 边缘：站点自己的源挂了，重登无用
    ('鉴权失败: 七个用户 ID 头名全部试探失败（04 §3.1 fan-out）: '
     'GET /api/user/self 返回 522: ',
     "上游站点自身不可达（CDN 边缘 5xx）"),
    ('鉴权失败: 七个用户 ID 头名全部试探失败（04 §3.1 fan-out）: '
     'GET /api/user/self 返回 530: {"type":"https://developers.cloudflare.com'
     '/support/troubleshooting/http-status-codes/cloudflare-1xxx-errors/'
     'error-1033/","title":"Error 1033: Cloudflare',
     "上游站点自身不可达（CDN 边缘 5xx）"),
]

fails = []


def check(name, ok, detail=""):
    print(f"{'✅' if ok else '❌'} {name}" + (f" — {detail}" if detail else ""))
    if not ok:
        fails.append(name)


for reason, want in REAL:
    got = classify_fatal(reason)
    check(f"{want[:18]:20} ← {reason[:52]}", got == want,
          "" if got == want else f"实际归到「{got}」")

# 双向哨兵。少了这两组的话，把 _MANUAL_CLASSES 写成"全都算人工"（或"全都不算"）
# 仍然能让上面那批归类断言全绿 —— 归类对了而分桶反了，报告照样在骗人。
for reason, want in REAL:
    if want in ("网络/传输失败（与凭证无关）",
                "上游站点自身不可达（CDN 边缘 5xx）"):
        check(f"不进人工清单: {want}", want not in _MANUAL_CLASSES)
    if want in ("需人工重登（无 refresh_token）", "凭证失效/鉴权失败",
                "鉴权通过但响应形态不符（需确认站型）"):
        check(f"必须进人工清单: {want}", want in _MANUAL_CLASSES)

# 兜底键必须已归并掉渠道号：否则 11 条同因失败会排成 11 行计数 1 的"分布"。
frag = classify_fatal("凭证续期失败: 续期失败（sub2api/渠道 12）: 某种未知形态")
frag2 = classify_fatal("凭证续期失败: 续期失败（sub2api/渠道 99）: 某种未知形态")
check("兜底键已归并渠道号（同因失败不碎成多行）", frag == frag2,
      f"{frag!r} vs {frag2!r}")

# 归档报告里的每一条 fatal 都要能归类，且不得落进兜底的"其它"。
# 兜底本身是对的（未知形态要暴露），但归档里已知的这些若落进兜底，
# 说明判定串与真实串对不上 —— 那正是本文件要拦的事。
for f in sorted(ARCHIVE.glob("p1-coverage-*.json")):
    d = json.loads(f.read_text(encoding="utf-8"))
    fatals = [x["fatal"] for x in d["details"] if x.get("fatal")]
    other = [x for x in fatals if classify_fatal(x).startswith("其它")]
    check(f"{f.name}：{len(fatals)} 条 fatal 全部可归类",
          not other, "" if not other else f"{len(other)} 条落进兜底：{other[0][:80]}")

print()
if fails:
    print(f"❌ {len(fails)} 条断言未通过")
    sys.exit(1)
print("✅ classify_fatal 判定顺序自测全部通过")
