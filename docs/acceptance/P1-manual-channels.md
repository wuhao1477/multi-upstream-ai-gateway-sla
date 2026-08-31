# P1 需人工维护的上游渠道清单

| | |
| --- | --- |
| 用途 | [#12](https://github.com/wuhao1477/multi-upstream-ai-gateway-sla/issues/12) 完成标准第 4 项「采不到数据的渠道列出人工维护责任人」 |
| 数据来源 | [`coverage/p1-coverage-2026-08-30b.json`](coverage/p1-coverage-2026-08-30b.json)，2026-08-30 22:59 全量 sync（65 渠道 / 163.7 秒 / 并发 6） |
| 依据 | FR-011「采不到的转人工录入」；[04 §5.3](../dev/04-collector-adapter.md) 账密重登通路 |
| **责任人** | **全部待指派** —— 这一列只有运营/站主关系的持有者能填，见文末 |

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

## 2. 清单（20 个）

<!-- ROSTER-START 表体由 verify/coverage_report.py 的 need_manual 生成，责任人列人工维护 -->
| # | 站名 | 站型 | 类别 | 下一步动作 | 责任人 |
| --- | --- | --- | --- | --- | --- |
| 1 | 100xlabs | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | **待指派** |
| 2 | Bwen | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | **待指派** |
| 3 | Dwaiai | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | **待指派** |
| 4 | Jlypx | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | **待指派** |
| 5 | redacted-channel-05 | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | **待指派** |
| 6 | Owlai | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | **待指派** |
| 7 | Qaq | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | **待指派** |
| 8 | redacted-channel-08 | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | **待指派** |
| 9 | Td | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | **待指派** |
| 10 | redacted-channel-10.Chat - redacted-channel-10，智在必达 | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | **待指派** |
| 11 | redacted-channel-11 | sub2api | 需人工重登（无 refresh_token） | 账密重登（04 §5.3）或补 `refresh_token` | **待指派** |
| 12 | redacted-channel-12 | newapi | 凭证失效/鉴权失败 | 换一把有效 `access_token` | **待指派** |
| 13 | redacted-channel-13 | newapi | 凭证失效/鉴权失败 | 换一把有效 `access_token` | **待指派** |
| 14 | Z API | newapi | 凭证失效/鉴权失败 | 换一把有效 `access_token` | **待指派** |
| 15 | redacted-channel-15 | newapi | 凭证失效/鉴权失败 | 换一把有效 `access_token` | **待指派** |
| 16 | redacted-channel-16 | sub2api | 凭证失效/鉴权失败 | **先修凭证类型**，见 §3 | **待指派** |
| 17 | 龙虾 | sub2api | 凭证失效/鉴权失败 | **先修凭证类型**，见 §3 | **待指派** |
| 18 | redacted-channel-18 | newapi | 鉴权通过但响应形态不符（需确认站型） | 确认真实站型后重登记 | **待指派** |
| 19 | redacted-channel-19 | newapi | 鉴权通过但响应形态不符（需确认站型） | 确认真实站型后重登记 | **待指派** |
| 20 | redacted-channel-20 | newapi | 鉴权通过但响应形态不符（需确认站型） | 确认真实站型后重登记 | **待指派** |
<!-- ROSTER-END -->

小计：需人工重登 11、凭证失效 6、响应形态不符 3。

另有 2 个站本轮不可达但**不需人工**（网络/CDN 边缘，下一轮重试自愈）：
redacted-channel-03 API（TLS 握手超时）、Translate（Cloudflare 522）。夹具行 UI验收-397444 永不可达，
它的处置见 [§3.4](P1-evidence.md#34-真库里有一行夹具残留32-项里的-3-条-key-断言跑在它身上2026-08-30-查明)。

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

## 4. 责任人这一列为什么空着

**我填不了，也不该猜。** 这 65 个站是运营从 all-api-hub 导出的第三方中转站，
"谁维护哪个站"是账号归属与商务关系，仓库里没有任何字段承载它（`channels` 表无
owner 列），导出 JSON 里也没有（`tagIds` 全为空、`notes` 未使用）。
按 [14 §3](../dev/14-acceptance-matrix.md) 规则 3「不可判定即不通过」，
在这里编一个名字比留空更坏。

要结清 #12 第 4 项，需要你补两件事之一：

1. **逐行填人名**（20 行，或按类别整批指派）—— 填完这张表即可勾掉那一项；
2. **或者声明"这批站全部由 <某人/某组> 兜底"** —— 那就在表头加一行默认责任人，
   表体只保留例外。

补充一条与之相关的建议（**未实施，等你决定**）：责任人若要长期可查，
应该落成 `channels` 的一列而不是文档里的表 —— 文档会与库漂移，而这张表每轮
覆盖率报告都会变。但那是加迁移的事，不该顺手做在验收提交里。

## 5. 复现

```bash
make build
DATABASE_URL='postgres://…/SLA_DB?sslmode=disable' ADMIN_TOKEN=xxx SLA_ADDR=':18390' ./bin/sla-core &
ADMIN_TOKEN=xxx BASE=http://127.0.0.1:18390 OUT=/tmp/cov.json python3 verify/coverage_report.py
python3 verify/test_coverage_classify.py    # 分类判定顺序的自测
```
