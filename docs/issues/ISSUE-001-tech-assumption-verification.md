# ISSUE-001：用 Docker 实跑验证选型的 6 项技术假设

| 项目 | 内容 |
| --- | --- |
| 状态 | 源码级预验证已完成（2026-07-23）；运行时验证已在本机 Docker 实跑（2026-07-23，OrbStack / AxonHub `v1.0.0-beta5` / SQLite）；结果见文末"Docker 运行时验证结果" |
| 来源 | OPEN-ISSUES 第四部分；2026-07-22 确认执行方式 |
| 执行方式 | 两段式：① 源码级预验证（已做，固定提交读代码）；② Docker 实跑运行时验证（待做，不写业务代码） |
| 版本锁定 | AxonHub 固定提交 `ed6119a168483a205a85a2f38c7153f5cf1b61a6`（`unstable`，2026-07-20；最近发布 `v1.0.0-beta5`） |
| 预计耗时 | 运行时段 1～2 周 |
| 规模参考 | 验证按 100～1000 QPS 量级设计（2026-07-22 确认） |

> **环境说明**：两阶段执行。① **源码级预验证**在无 Docker/Go 的会话沙箱完成（逐项定位到具体文件与函数），把 6 项假设从"仅 README 声明"升级为"源码证实/证伪"。② **运行时验证**已于 2026-07-23 在装有 Docker（OrbStack v29.4.0）的本机对 AxonHub `v1.0.0-beta5`（SQLite）实跑完成，逐项结论见文末"Docker 运行时验证结果"。两阶段结论一致：其中假设 3 的风险由运行时铁证收口。

## 待验证的 6 项假设

每项写成可自动重复运行的测试，保留作为今后升级 AxonHub 的准入检查（AxonHub 近 30 天 75 次提交，每次升级都要重跑）。

| # | 假设 | 验证要点 | 不成立的后果 |
| --- | --- | --- | --- |
| 1 | 单渠道 Profile 隔离 | 把 AxonHub 限定为只许用某一个渠道，制造该渠道故障，确认不会偷偷回退到其他未授权渠道 | 单资源绑定失效，Key 级成本/缓存归因全部不可信 |
| 2 | 取消可对账 | 客户端断开、SLA 接管取消、上游取消三种情况，确认各方记录能关联到同一 Attempt | 重复费用和接管账目对不上 |
| 3 | 首字定义 | 确认 AxonHub 的"首字"是用户真正可见内容，而不是响应头、空 SSE 或 heartbeat | TTFT 预算和首字前接管全部建立在错误信号上 |
| 4 | 账本可关联 | 确认价格、用量、缓存 Token、费用和平台请求 ID 能与外部账本关联 | 财务级 Attempt 账本无法落地 |
| 5 | 权限过滤顺序 | 确认权限过滤发生在渠道排序之前 | 不合规渠道可能进入候选 |
| 6 | 备选绑定稳固性 | 对 ccLoad 等备选验证单资源拓扑不被会话粘性、内部 Key/URL 轮换、隐藏重试或元事件提前提交破坏 | 备选方案不可用，无退路 |

## 执行步骤

1. Docker Compose 拉起固定版本 AxonHub（及对比用的 ccLoad），配置 2～3 个模拟上游（可用 mock server 模拟正常/慢速/故障/流式响应）。
2. 每项假设编写独立测试脚本，输出 通过/不通过 + 证据（日志、响应记录）。
3. 结果回写本文档"验证结果"节；不成立的假设按预案处理：优先自研层补偿 → 补偿不了切 ccLoad 重新验证。
4. 测试脚本入库保存，作为升级准入检查。

## 源码级预验证结果（2026-07-23，固定提交 `ed6119a1`）

| # | 假设 | 源码结论 | 证据（文件:符号） | 运行时仍需收口 |
| --- | --- | --- | --- | --- |
| 1 | 单渠道 Profile 隔离 | ✅ **成立**。候选选择先按 `profile.ChannelIDs` 过滤，再交负载均衡；单渠道 Profile 的候选集只含该渠道，重试循环 `HasMoreChannels/NextChannel` 只在候选集内切换，无法切到未授权渠道 | `orchestrator/select_candidates.go`（Key/Project Profile → ChannelIDs 白名单）；`llm/pipeline/pipeline.go` 重试循环 | 实配单渠道 Profile 制造故障，确认无越权切换、无同渠道多 Key 隐性轮换 |
| 2 | 取消可对账 | ✅ **成立**。`request_execution.status` 枚举含 `canceled`；每次尝试落一行 execution，均以 `request_id` 关联同一 Request；`ctx.Err()` 取消即停止重试 | `ent/schema/request_execution.go`（status 枚举、request_id 边）；`pipeline.go`（`if ctx.Err()!=nil`） | 三种取消（客户端断开/接管/上游）都落 `canceled` 且能关联到同一 request；重复费用聚合由外部账本完成（符合设计） |
| 3 | 首字定义 | ⚠️ **风险证实**。记录的 `metrics_first_token_latency_ms` 在**首个非 nil 流事件**即打点（早于内容判断），非首个用户可见内容。其重试逻辑另用内容感知的 `hasResponseContent`，但该定义未用于对外 TTFT 指标 | `orchestrator/performance.go:156` `MarkFirstToken()` 先于内容检查；对比 `llm/pipeline/empty_response.go:hasResponseContent/hasMessageContent`（内容感知，排除 role-only/空 delta） | 外部 SLA 核心**不得采信** AxonHub 的 TTFT 字段，须自行按内容感知逻辑计算首字；运行时验证空 SSE/心跳/role-only 是否被误计为首字 |
| 4 | 账本可关联 | ✅ **成立**。`request_execution` 有 `external_id`（平台请求ID）、逐尝试 metrics；`usage_log` 有 request_id、channel_id、model_id、各类 token（含缓存 5m/1h）、`total_cost`、`cost_items`、`cost_price_reference_id`（关联价格版本） | `ent/schema/request_execution.go`、`ent/schema/usage_log.go` | 验证真实上游回传的 `external_id` 确为平台请求ID、缓存 token 与费用能对上真实账单 |
| 5 | 权限过滤顺序 | ✅ **成立**。`select_candidates` 顺序为 Project Profile → Key Profile → 工具/流策略 → Provider Quota → **最后** `WithLoadBalancedSelector`，即渠道/权限过滤在负载均衡排序之前 | `orchestrator/select_candidates.go`（选择器组装顺序） | 确认 `QuotaEnforcementModeDePrioritize` 下配额 `unknown` 是否仍进候选（axonhub.md 已记录此缺口），需运行时确认保守下限行为 |
| 6 | 备选 ccLoad 绑定 | ⏸️ **本轮未重跑**。ccLoad 为独立仓库，源码结论见 [ccLoad 评估](../tech-selection/research/ccload.md)：单 Key/URL Channel + 单 Channel Token 可 L0，但元事件可能提前提交 | `tech-selection/research/ccload.md` | 仅在启用 ccLoad 备选时，单独 Docker 验证单资源拓扑不被会话粘性/内部轮换/隐藏重试/元事件提前提交破坏 |

### 小结

6 项中 **3 项源码确认成立（1/2/4/5 共 4 项为成立，其中 5 附带一个已知配额缺口）**，**1 项风险证实（3 首字定义）**，**1 项待备选启用时验证（6）**。最关键结论：假设 3 证实了选型文档的担忧——**AxonHub 记录的首字时间不可直接作为用户可见 TTFT**，外部 SLA 核心必须自算。这不影响选型成立（自研核心本就承担 TTFT 判定），但必须写入自研核心的实现要求。

## Docker 运行时验证方案（harness 已就绪，已实跑）

已构建可一键运行的验证 harness：[`verify/`](../../verify/README.md)（compose + stdlib mock 上游 + 自动 setup + 6 假设探测 + 自清理）。**2026-07-23 已在本机 Docker 实跑**（OrbStack v29.4.0，镜像 `looplj/axonhub:v1.0.0-beta5`），实跑中据 beta5 的实际 GraphQL schema 对 setup/探测脚本做了适配（详见下方"beta5 schema 适配"），跑完自清理无残留。

harness 组成与用法见 [verify/README.md](../../verify/README.md)，要点：

1. `docker compose -f verify/docker-compose.verify.yml up -d` —— AxonHub(`v1.0.0-beta5`，SQLite，免 Postgres) + Python mock 上游（role-only 首帧 / 空 SSE / 慢首帧 / 心跳 / 500）。
2. `python3 verify/setup.py` —— 按 `ed6119a1` 的 GraphQL schema 自动初始化、建渠道A/B、建 Key、锁单渠道 profile。
3. `verify/run_tests.sh` —— 6 项假设的运行时探测（带时间戳 SSE + execution 记录查询），重点收口假设 3（首字打点）与假设 1（隔离）。
4. `verify/cleanup.sh` —— 停容器、删卷/网络/镜像、删状态文件。
5. 测试脚本入库，作为每次升级 AxonHub 的准入检查（近 30 天 75 次提交，升级须重跑）。

> 第一次运行若因版本差异报 GraphQL 字段名错误，把报错贴回即可据实修正；这不影响源码级结论。

## Docker 运行时验证结果（2026-07-23，AxonHub `v1.0.0-beta5` / SQLite）

运行时实跑结论与源码级预验证一致（假设 5 的配额缺口另由运行时决定性确认，见下）。汇总:

| # | 结果 | 证据（运行时） | 日期 |
| --- | --- | --- | --- |
| 1 | ✅ 证实 | mock-500（渠道A）失败后在**渠道A内重试 3 次**（execution 均 `channelID=Channel/1`、`responseStatusCode=500`），客户端收到错误 `mock upstream 500`，**从未切到渠道B**（active profile 仅含渠道A） | 2026-07-23 |
| 2 | ✅ 证实（三路） | 客户端断开=`canceled`、上游中途断开=`failed`、内部首事件超时+转移=attempt `failed`+`completed`；三者均逐 attempt 落 execution、关联同一 request，可对账。详见下 | 2026-07-23 |
| 3 | ⚠️ 风险证实 | `metricsFirstTokenLatencyMs` = 首个 JSON 流事件（含 role-only delta），**非用户可见内容 TTFT**。三条铁证见下 | 2026-07-23 |
| 4 | ✅ 证实 | usage_log 记录 token（prompt/completion/total/**cached**）+ `totalCost` + `costPriceReferenceID`（价格版本），挂在对应 Request 下；成本按配置单价精确算出，缓存 token 走折扣价 | 2026-07-23 |
| 5 | ⚠️ 部分收口 | 配额 enforcement 系统级、**默认关闭**；`DE_PRIORITIZE` 下配额 `unknown` 渠道**仍进候选且可被选中**（保守下限=保留而非排除）。详见下 | 2026-07-23 |
| 6 | ⏸ 本轮未覆盖 | ccLoad 独立仓库、需另起 compose | — |

### 假设 2 取消对账 —— 三路取消/失败全部可对账

三种中断来源都逐 attempt 落一行 execution、以 `channelID` 标注、关联同一 `request_id`：

| 取消/失败来源 | attempt status | errorMessage | 关联 |
| --- | --- | --- | --- |
| 客户端断开 | `canceled` | `...context canceled` | ✅ 同 request |
| 上游中途断开（mock-abort：发 role+部分内容后直接断 TCP，无 finish/[DONE]） | `failed` | `stream ended without terminal event or completed response` | ✅ 同 request |
| 内部首事件超时 + 故障转移 | attempt-1 `failed` / attempt-2 `completed` | `stream first event timeout` | ✅ 两 attempt 同 request |
| （参照）上游 500 | `failed` | `mock upstream 500` | ✅ 同 request |

**语义澄清（重要）**：`canceled` 在 beta5 里**专指客户端/上下文取消**；上游断开、内部超时都归 `failed`。→ **外部 SLA 账本若要统一"取消"口径，须按 `errorMessage` 归并，不能只看 status 枚举。**

**"SLA 接管"的定位**：选型架构里的"SLA 接管取消"是自研层在 AxonHub 之前决定取消 → 从 AxonHub 看等同客户端断开 → `canceled`（已覆盖）。本轮另验证了 AxonHub **自身** retry-policy 驱动的故障转移（独立机制）：内部首事件超时掐断 attempt-1（`failed`）+ 转移到 attempt-2（`completed`），同一 request 可对账。

**RetryPolicy 配置要点**（`retryPolicy` / `updateRetryPolicy`，系统级）：

- `streamFirstEventTimeoutSeconds` **默认 `0`=关闭** —— 开箱状态 AxonHub **不会**主动掐断慢首字，须显式开启才有内部接管。
- `maxChannelRetries`（默认 3，跨渠道转移）、`maxSingleChannelRetries`（默认 2，同渠道）、`loadBalancerStrategy`（默认 `adaptive`；`failover` 按 `orderingWeight` 升序定主备）。
- 坑：`updateRetryPolicy` 是**整体替换**，只传部分字段会把其余重置为默认/0。

### 假设 3 首字定义 —— 三条运行时铁证

外部 SLA 的首字指标**不得采信** AxonHub 的 `metricsFirstTokenLatencyMs`，必须自算（内容感知 TTFT）：

1. **mock-normal**：mock 在 role-only delta 后 sleep 0.5s 才发首内容；记录值 `10ms`（≈0）而非 ≈500ms → 打点在**首个流事件**，非首内容。
2. **mock-empty-sse**（全程零内容，只有 role delta + finish）：仍记录 `metricsLatencyMs=10ms` → 一个无任何可见内容的响应照样被赋予极小延迟，首字判定与内容无关。
3. **mock-heartbeat**（先 ~0.6s 的 SSE 注释心跳，再 role delta，再内容）：记录 `623ms`（≈心跳走完后的 role delta 时刻）→ **SSE 注释心跳不计入，role-only delta 计入**。

与源码级 `orchestrator/performance.go:MarkFirstToken()`（先于内容判断打点）分析完全吻合。**自研 SLA 核心必须实现内容感知 TTFT，不能复用 AxonHub 该字段。**

### 假设 4 账本可关联 —— token + cost 均收口

给渠道A 的 mock-normal / mock-heartbeat 配置每-1M-token 单价（prompt=1 / completion=2 / cached=0.5）后，请求的 usage_log：

| 模型 | prompt | completion | cached | totalCost | costPriceReferenceID | 成本验算 |
| --- | --- | --- | --- | --- | --- | --- |
| mock-normal | 6 | 4 | 2 | `1.3e-5` | `TEqwiMyL` | 非缓存 4×1 + 缓存 2×0.5 + 补全 4×2 = 13e-6 ✅ |
| mock-heartbeat | 9 | 1 | 0 | `1.1e-5` | `cGZZVgnV` | 9×1 + 1×2 = 11e-6 ✅ |

- **token**：prompt/completion/total 与上游 usage 一致；`promptCachedTokens` 正确记录缓存部分。
- **cost**：`totalCost` 按配置单价精确算出；**缓存 token 走折扣价**（验算吻合）。
- **关联**：`costPriceReferenceID` 指向具体价格版本；usage_log 挂在对应 Request 下，`externalID` 为上游响应 id（如 `chatcmpl-mock-mock-normal`）。
- 前提：AxonHub 侧需配置模型价格（`saveChannelModelPrices`，见下）+ 上游回传 usage（标准 OpenAI 流式末帧 `usage` 对象）。二者齐备即可财务级对账。

> 失败/取消的请求（mock-500、mock-slow-first）`usageLogs` 为空、`externalID` 为空，符合预期（无成功用量）。

### 假设 5 权限/配额过滤顺序 —— 部分收口（配额模型 + unknown 保守下限）

beta5 的 provider 配额模型：

- **enforcement 是系统全局开关**（`quotaEnforcementSettings{ enabled mode }`），**默认 `enabled=false`** —— 开箱即用时 provider 配额根本不参与候选过滤/排序，必须显式开启。
- 两种模式：`EXHAUSTED_ONLY`（仅耗尽才管）/ `DE_PRIORITIZE`（降优先级）。
- provider 配额**仅对订阅类渠道生效**（`opencode_go` / `claudecode` / `codex` / `github_copilot` 等）；通用 `openai`/`anthropic` 渠道 `providerQuotaStatus=null`，不参与 provider 配额。
- 配额状态 `available | warning | exhausted | unknown` 挂在 Channel 上（`Channel.providerQuotaStatus`），由 AxonHub 探测订阅面板得出；探测失败/未就绪即 `unknown`（`ready:false`）。

**决定性运行时结论（收口 [axonhub.md](../tech-selection/research/axonhub.md) 记录的缺口）**：显式开启 `enabled=true, mode=DE_PRIORITIZE`，把 Key profile 锁到一个配额 `unknown`（`opencode_go` 探测失败）的渠道，发 `mock-normal` → 请求 `completed`、execution 落在该 unknown 渠道；混合 profile（openai 无配额 + opencode_go unknown）连发 9 请求 → 全部正常路由。

→ **`unknown` 配额渠道仍进候选且可被选中，保守下限是"保留可用"（permissive），不是"按 exhausted 剔除"（conservative-exclude）。** 若外部 SLA 要求"配额状态未知时保守排除该渠道"，**AxonHub 默认不满足，须自研层补这层保守过滤**（与"自研核心承担关键判定"定位一致）。附带更强的默认风险：配额执行**默认完全关闭**。

源码级"过滤先于负载均衡"由 profile 白名单在运行时确实先于 LB 生效间接佐证（见假设 1）。**未隔离验证**（需真实订阅凭证，mock 无法伪造订阅面板协议）：`available`/`exhausted` 真实状态下的剔除与降级排序、`unknown` 是否排在 `available` 之后——需接真实 `opencode_go`/`claudecode` 渠道另做。

### beta5 schema 适配（供今后升级重跑参考）

运行时实跑用 `v1.0.0-beta5` 镜像，其 GraphQL schema 与固定评估提交 `ed6119a1` 有若干差异；harness 脚本（`verify/setup.py`、`verify/run_tests.sh`）已据实适配（工作区改动，未 commit）。**每次升级 AxonHub 重跑时若报字段/端点错误，按下表核对：**

| 位置 | beta5 实际 | 说明 |
| --- | --- | --- |
| GraphQL 端点 | `/admin/graphql`（非 `/graphql`） | 裸 `/graphql` 被前端 SPA catch-all 吃掉返回 HTML |
| 系统/鉴权路由 | `/admin/system/initialize`、`/admin/auth/signin` | 均带 `/admin` 前缀 |
| ID 格式 | relay guid（`gid://axonhub/Project/1`、`Channel/1`） | `createAPIKey.projectID` 需 gid；`channelIDs`（[Int]）需取 gid 末段数字 |
| 渠道默认状态 | 建后为 `disabled` | **必须** `updateChannelStatus(status:enabled)`，否则请求 `model not found` |
| 用量字段 | `usageLogs`（连接），非 `usage` | `totalTokens/promptTokens/completionTokens/promptCachedTokens/totalCost/costPriceReferenceID` |
| Request 字段 | `modelID`（非 `model`）；`executions` 为分页连接 | metrics 在 Request 与 RequestExecution 两层都有 |
| 模型定价 | `saveChannelModelPrices(channelId, input:[SaveChannelModelPriceInput!])` | item `pricing.mode="usage_per_unit"`，`usagePerUnit`=每 1M token 单价；itemCode：`prompt_tokens/completion_tokens/prompt_cached_tokens` |
| 路由就绪 | 渠道启用后异步同步，有秒级延迟 | setup.py 加 warmup 轮询，避免 run_tests 竞态到 `model not found` |
| 时间戳 | macOS `date` 无 `%N` | run_tests.sh 用 `gdate`/python 回退取毫秒 |
