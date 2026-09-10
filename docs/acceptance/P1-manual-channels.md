# P1 采不到数据的上游渠道：已放弃纳管

| | |
| --- | --- |
| 用途 | [#12](https://github.com/wuhao1477/multi-upstream-ai-gateway-sla/issues/12) 完成标准第 4 项「采不到数据的渠道列出人工维护责任人」 |
| 数据来源 | [`coverage/p1-coverage-2026-08-30b.json`](coverage/p1-coverage-2026-08-30b.json)，2026-08-30 22:59 全量 sync（65 渠道 / 163.7 秒 / 并发 6） |
| 依据 | FR-011「采不到的转人工录入」；[04 §5.3](../dev/04-collector-adapter.md) 账密重登通路 |
| **处置** | **2026-08-31 决定：三类全部放弃纳管，只保留可用的。** 20 个渠道已 `status=disabled` + 停用原因；责任人一列**作废**（不再需要人去维护它们） |

> **本文件的定位随之变了。** 原先它是「待指派责任人的人工维护清单」，
> 现在是「**放弃了哪些站、为什么放弃、怎么还原**」的记录。
> 表体保留 20 行不删 —— 放弃 ≠ 删除，删掉这张表日后就说不清 65 个渠道里
> 那 20 个为什么是停用态，而重新导入 hub 文件时也会失去"这些站已确认死了"这个判断。

## 0. 这次做了什么（2026-08-31）

| | |
| --- | --- |
| 动作 | 逐个 `PATCH /admin/channels/{id}` → `status=disabled` + `disabled_reason` |
| 范围 | 三类共 **20** 个：需人工重登 11 / 凭证失效 6 / 形态不符 3 |
| 台账 | 渠道总数仍 **65**（52 newapi + 13 sub2api），其中 **enabled 45**（全为 newapi）、**disabled 20** |
| 未动 | `retryable` 那 2 个（渠道 1 夹具残留、Translate 的 CDN 522）—— 它们不属这三类 |
| 还原 | 停用前快照见提交说明；单个还原就是 `PATCH status=enabled`（服务端会一并清空原因与有效期） |

**为什么是停用而不是删除**：仓库里没有 `DELETE /admin/channels` 路由，删除要对真库
写裸 SQL，且会级联带走 `collector_credentials` / `channel_model_catalog` /
`channel_groups` —— 后两者是采集来的，导出文件里没有，删了不可复得。
而 `PATCH status=disabled` 走的是界面同一条代码路径（`3bc84d2` 补的入口），
可逆、有原因、留痕。

### 顺带修掉一个真缺陷：停用此前不影响采集

放弃之后立刻验了一次"停用是否真的生效"，结果**没生效**：对已停用的渠道 30
发采集，照样打了上游并返 502（`鉴权失败: 上游返回 401`）。`syncChannel` 从头到尾
不看 `status`。

那意味着这次放弃在报告上看不出任何效果 —— 每轮覆盖率仍会去打这 20 个死站，
20 次无谓的上游请求、20 条注定的 fatal、人工清单永远停在 20 条。已修，
两层都挡：端点侧停用即 **422**（`ErrPrecondition` 语义，不触达上游、不起算 60s 窗口），
覆盖率脚本侧只遍历在纳管的渠道并把跳过的逐行打出来。
详见 [P1-evidence §5.18](P1-evidence.md#518-放弃-20-个采不到的站并修掉停用不影响采集2026-08-31)。

## 1. 这张清单为什么是 20 个而不是 23 个

上一版报告把 **23** 个站列进 `need_manual`，那个数字是错的。分类函数的第一个分支是
`if "401" in reason or "鉴权" in reason`，而 `sync.go:110` 把**所有** `Authenticate`
失败都包成 `鉴权失败: %w` —— 于是这个分支吃掉一切，后面的"连接超时/不可达"分支
对 fan-out 失败**永远不可达**。被误记为"凭证失效"的有：

| 站 | 真实原因 | 为什么这是硬错误 |
| --- | --- | --- |
| redacted-channel-03 API（渠道 3） | `net/http: TLS handshake timeout` | 它的令牌与 all-api-hub 导出里那把**逐字节相同**（sha256 前 12 位 `[redacted fingerprint]` 两边一致），且同一天 `ui-stack.sh` 拿它跑完 58 项全绿。**派人去重登一把好令牌，是把人派去修一个不存在的问题。** |
| UI验收-397444（渠道 1） | `connection refused` | 夹具残留，base_url 指向已删除的 mock 端口（§3.4） |
| Translate（渠道 45） | `GET /api/user/self 返回 522` | Cloudflare 边缘：站点自己的源不可达，重登无用 |

修法与破坏性验证见 [P1-evidence §5.16](P1-evidence.md#516-覆盖率报告把-tls-超时记成凭证失效人工清单虚高了三个站2026-08-30)。
报告现在产出**两张**单子：`need_manual`（要人手）与 `retryable`（网络/边缘，下轮重试）。

**同一次修复顺带暴露了一个更严重的反向错误**：11 条 sub2api 现在报
`无 refresh_token 可用（需人工重登）`，这串既不含 `401` 也不含 `鉴权`，
于是第一版修好后它们**全部掉进兜底、被算成 retryable** —— 而它们恰恰是这批里最
确定需要人手的（库里没有 refresh_token，重试一万次也不会变好）。已单独成类。

## 2. 放弃清单（20 个）

<!-- ROSTER-START 表体由 verify/coverage_report.py 的 need_manual 生成；「当前状态」列每轮由真库现查 -->
| # | 渠道号 | 站名 | 站型 | 放弃原因 | 若要恢复需做什么 | 当前状态 |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | 37 | 100xlabs | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | `disabled` |
| 2 | 27 | Bwen | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | `disabled` |
| 3 | 38 | Dwaiai | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | `disabled` |
| 4 | 21 | Jlypx | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | `disabled` |
| 5 | 64 | redacted-channel-05 | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | `disabled` |
| 6 | 13 | Owlai | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | `disabled` |
| 7 | 14 | Qaq | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | `disabled` |
| 8 | 18 | redacted-channel-08 | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | `disabled` |
| 9 | 12 | Td | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | `disabled` |
| 10 | 63 | redacted-channel-10.Chat - redacted-channel-10，智在必达 | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | `disabled` |
| 11 | 58 | redacted-channel-11 | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | `disabled` |
| 12 | 25 | redacted-channel-12 | newapi | 凭证失效/鉴权失败 | 换一把有效令牌 | `disabled` |
| 13 | 33 | redacted-channel-13 | newapi | 凭证失效/鉴权失败 | 换一把有效令牌 | `disabled` |
| 14 | 8 | Z API | newapi | 凭证失效/鉴权失败 | 换一把有效令牌 | `disabled` |
| 15 | 42 | redacted-channel-15 | newapi | 凭证失效/鉴权失败 | 换一把有效令牌 | `disabled` |
| 16 | 31 | redacted-channel-16 | sub2api | 凭证失效/鉴权失败 | **先修凭证类型**，见 §3；再换有效令牌 | `disabled` |
| 17 | 30 | 龙虾 | sub2api | 凭证失效/鉴权失败 | **先修凭证类型**，见 §3；再换有效令牌 | `disabled` |
| 18 | 9 | redacted-channel-18 | newapi | 鉴权通过但响应形态不符（需确认站型） | 确认真实站型后重登记 | `disabled` |
| 19 | 57 | redacted-channel-19 | newapi | 鉴权通过但响应形态不符（需确认站型） | 确认真实站型后重登记 | `disabled` |
| 20 | 46 | redacted-channel-20 | newapi | 鉴权通过但响应形态不符（需确认站型） | 确认真实站型后重登记 | `disabled` |
<!-- ROSTER-END -->

小计：需人工重登 11、凭证失效 6、响应形态不符 3。

另有 2 个站本轮不可达但**不需人工**（网络/CDN 边缘，下一轮重试自愈）：
redacted-channel-03 API（TLS 握手超时）、Translate（Cloudflare 522）。夹具行 UI验收-397444 永不可达，
它的处置见 [§3.4](P1-evidence.md#34-真库里有一行夹具残留35-项里的-3-条-key-断言跑在它身上2026-08-30-查明)。

## 3. 重新导入解决不了任何一个

交叉核对过全部 20 个站：**库里的凭证与 all-api-hub 导出里的那把逐字节相同**
（按 base_url 与站名两路匹配，比 sha256 前 12 位与长度；20/20 相同，0 个"导出里有更新的"）。
也就是说这不是"导入漏了"或"库里的旧了"，**导出本身持有的就是这些已失效的凭证** ——
重新导入一次不会改变任何一行。恢复只能来自上游站点侧的新凭证。

**第 16、17 两行要先修凭证类型，重登也白搭。** 它们在库里登记为 `sub2api_jwt`，
但令牌只有 **32 字符**（真 sub2api JWT 实测 272~383 字符，且以 `eyJ` 开头）。
导出里这两站的 `authType` 是 `access_token`、`site_type` 分别是 `unknown` 与 `new-api` ——
**探测把它们判成 sub2api 并覆盖了导出的声明**（这个覆盖本身是对的，`ui-stack.sh`
那条「站型声明与探测不符的站点逐行标出」就是在验它，redacted-channel-16正是被标出的 2 个之一）。
但凭证还是那把 32 字符的 API key，用它走 sub2api 的 JWT 鉴权必然 401。
所以这两站要么补一把真的 sub2api 登录凭证（账密→JWT），要么确认它究竟是哪一族后重登记。

## 4. 责任人这一列为什么作废了

原先这一列是 20 个「待指派」，理由是：这些站是运营从 all-api-hub 导出的第三方中转站，
"谁维护哪个站"属账号归属与商务关系，仓库里没有字段承载它（`channels` 无 owner 列），
导出 JSON 里也没有（`tagIds` 全空、`notes` 未用），编一个名字比留空更坏
（[14 §3](../dev/14-acceptance-matrix.md) 规则 3「不可判定即不通过」）。

**2026-08-31 这个问题被另一种方式解决了：这三类站全部放弃纳管。** 不再需要有人
去重登、换令牌或确认站型 —— 于是"责任人"这一列失去对象。#12 第 4 项因此结清，
结清方式是**"采不到的渠道已不在纳管范围内"**，而不是"每个都指定了维护人"。

> ⚠️ 这与 P1-evidence §3.1 那次「第三族被移除而非填上」是同一种结清方式，
> 值得照同样的口径记下来：**放弃是一个决定，不是一次修复。** 那 20 个站的
> 真实可采集性至今未被恢复过，只是不再由本系统承担。日后若要接回其中任何一个,
> 按上表「若要恢复需做什么」那一列做，然后 `PATCH status=enabled`。

关于责任人落库的那条建议（**仍未实施**）：若日后有需要长期维护的站，责任人应落成
`channels` 的一列而不是文档里的表 —— 文档会与库漂移。现在没有需要维护的站，
这条也就不急。

## 5. 复现

```bash
make build
DATABASE_URL='postgres://…/SLA_DB?sslmode=disable' ADMIN_TOKEN=xxx SLA_ADDR=':18390' ./bin/sla-core &
ADMIN_TOKEN=xxx BASE=http://127.0.0.1:18390 OUT=/tmp/cov.json python3 verify/coverage_report.py
python3 verify/test_coverage_classify.py    # 分类判定顺序的自测
```

放弃之后跑出来的形态（2026-08-31 12:55 实测，归档
[`coverage/p1-coverage-2026-08-31.json`](coverage/p1-coverage-2026-08-31.json)）：
**45 个在纳管渠道 / 88.0 秒 / `need_manual` 0 / `skipped_disabled` 20**，
脚本开头会把跳过的 20 行连原因一起打出来。耗时比放弃前（163.7 秒）几乎腰斩 ——
不打那 20 个死站省下的正是失败路径最贵的那部分（七个用户 ID 头名逐个试探）。

要还原某一个站：

```bash
curl -X PATCH -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"status":"enabled"}' http://127.0.0.1:18390/admin/channels/30
# 服务端在 status=enabled 时会一并清空 disabled_reason 与 disabled_until
```

界面上也能做（渠道列表每行的启用/停用按钮，`3bc84d2` 补的入口）。
但先做上表「若要恢复需做什么」那一列 —— 直接启用只会让它下一轮继续失败。
