# Octopus 系列源码评估

| 项目 | `bestruirui/octopus` | `Hureru/octopus` |
| --- | --- | --- |
| 研究分支 | `master`，不是默认分支；无 `main` | `dev` |
| 固定提交 | `b7b053e7fd81911e2062359e93f9dcbd58114bb0` | `0e1a7ee062c8a24a9c8b95e7d1778f265182a304` |
| 提交/标签 | 2026-05-11 / `v0.9.28` | `v0.8.40` |
| 验证范围 | `master` 执行 `go test ./...` 通过；绝大多数包无测试 | 本轮未重复运行 |
| 总结 | 可构造受限 L0 拓扑绑定，但可靠性和观测不足 | 功能更多，但已是大型独立旁支 |

本轮按用户要求重新研究 `bestruirui/octopus` 的 `master`。GitHub 默认分支为 `dev`，且 `master` 比当前 `dev` 少两个提交；旧报告中指向 `dev` 的源码和协议实现不能作为本节证据。

## 1. 分叉关系

Hureru 是 GitHub 标记的 `bestruirui/octopus` fork，但已形成独立产品。此前按双方 `dev` 固定提交比较得到：公共祖先为 `bf07971d...`，上游独有 9 个提交、Hureru 独有 286 个提交，树差异涉及 424 个文件，约 `+95,349/-3,933`。该关系用于维护判断；`bestruirui` 的功能结论仍严格以本轮 `master` 为准。

## 2. `bestruirui/octopus` master 请求链

文本请求链为 API 认证 → `/v1/chat/completions|responses|messages|embeddings` → `relay.Handler` → 以请求 model 查 Group → Balancer 候选 → Channel → Channel 内 Key → 协议转换 → HTTP 请求 → 流/非流处理 → Metrics 和 RelayLog。图片请求使用独立 Relay，但选择模型相同。

已验证能力：

- Group 支持轮询、随机、优先级故障转移和权重；失败且未写客户端时遍历下一 Channel。
- Channel 可配置多个 Key 和 Base URL；Key 按累计成本最低选择，记录 `channel:key:model` 熔断。
- Group 有静态首 Token 超时和进程内会话保持。
- RelayLog 保存最终 TTFT、Token、成本、错误及 ChannelAttempt 数组。
- 管理 API 可维护 Channel、Group、Key、价格、日志、统计和导入导出。

关键源码：

- [Relay 入口和候选循环](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/relay/relay.go#L28)
- [Group 模型](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/model/group.go#L3)
- [Channel、Key 和 URL](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/model/channel.go#L18)
- [Attempt 追踪](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/relay/balancer/iterator.go#L10)
- [RelayLog](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/model/log.go#L13)

## 3. 外部资源选择边界

普通请求没有 `channel_id`、Key、URL 或候选计划字段。Relay 只读取 model，以 Group 名称查候选；客户端 API Key 只有模型限制和总成本限制，没有 Channel Binding：[API Key 模型](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/model/apikey.go#L3) [按 model 查 Group](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/op/group.go#L41)。

仍可构造受限的 stock L0 拓扑绑定：为每个真实资源创建私有 model alias；该 alias 对应只含一个 Channel 的 Group；Channel 只放一个 Key 和一个 URL；再创建只允许该 alias 的内部 API Key。外部适配器把客户端模型改写为 alias，即可按构造确定资源。

这个方式弱于 AxonHub Profile 和 ccLoad Channel allowlist：

- 它复用公开 model 命名空间表达内部资源，不是正式路由选择契约。
- 管理 API 误加第二个 GroupItem、Key 或 URL 会静默破坏精确归因。
- 响应没有实际 Channel/Key 证明；只能依赖配置和事后日志。
- 若要求正式的受认证 Channel/Key/URL 选择，需修改 Relay 和认证边界，属于 L2。

## 4. 首内容、取消和接管

Group 的 `first_token_time_out` 只在上游已返回 HTTP 响应头、进入 `handleStreamResponse` 后启动；DNS、连接、TLS 和响应头等待不计入该期限，HTTP Client 也没有总超时：[流式超时](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/relay/relay.go#L361) [HTTP Client](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/client/http.go#L98)。

首次非空转换结果会立即停止计时并写客户端，但 OpenAI Responses 转换器会先生成 `response.created` 和 `response.in_progress`，没有用户可见文本、推理或工具参数：[Responses 首事件](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/transformer/inbound/openai/response.go#L118)。因此内置 FTUT 不是本项目的首有效内容。

客户端 context 会进入上游请求，但流循环在取消时返回 `nil`，随后可能按成功记录。外部 SLA 层可以自行取消整条 HTTP 请求，却不能依赖 Octopus 日志确认取消结果。严格接管必须在外部层缓冲并判断首有效内容。

## 5. 价格、缓存、测活和 Attempt

master 支持 input/output/cache read/cache write 价格，并从 models.dev 同步；客户端 API Key 有累计 `MaxCost` 和到期限制。这些字段约束下游调用凭证，不代表上游渠道订阅。项目没有上游订阅固定费用、共享额度、有效期/续订、超额计费、预计到期未用额度，也没有上游账号余额、Key 独立余额、渠道倍率、采购价版本、币种、在途费用或账单核对。

Attempt 数组记录 Channel、Key ID、序号、耗时、状态和粘性；最终 RelayLog 才记录总 usage 和成本。单次失败 Attempt 没有独立 Token、缓存、费用和可靠 HTTP 状态码，不能核算重复费用。会话键只是 `客户端 API Key ID:model`，进程内保存且重选时只保持 Channel，不保证同一上游 Key：[会话状态](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/relay/balancer/session.go#L9)。

主动健康任务只对 Base URL 发无鉴权 `HEAD`，不检查模型能力，也不使用合资格真实业务请求：[URL 延迟检查](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/helper/delay.go#L9)。没有探索预算、故障域和测活历史。

## 6. master 特有可靠性问题

源码审查确认以下问题尚在本轮固定提交中：

1. 非 2xx 分支返回的 status code 为 0，导致按 429 判断的 Key 冷却不生效：[错误状态返回](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/relay/relay.go#L305)。
2. `ParamOverride` JSON 解析失败会 `return 0, nil`，上游未调用却进入成功路径：[参数覆盖](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/relay/relay.go#L265)。
3. Attempt 已更新 Channel 统计，`RelayMetrics.Save` 又对最终 Channel 更新一次，产生重复计数：[Attempt 统计](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/relay/relay.go#L181) [最终统计](https://github.com/bestruirui/octopus/blob/b7b053e7fd81911e2062359e93f9dcbd58114bb0/internal/relay/metrics.go#L83)。
4. Channel 始终选择最低延迟 Base URL；该 URL 失败时不会在同一 Channel 尝试其他 URL，且熔断键不含 URL。

`dev` 的两个后续提交修复了其中部分问题并替换约一万行协议转换代码，但这不属于用户指定的 `master` 研究基线。

## 7. Hureru 旁支补充

Hureru 增加了 Site、Account、Token、UserGroup、余额、ChannelBinding、缓存 Token、费用、同账号离群检测和每次尝试日志，比 bestruirui master 更接近聚合站场景。它仍使用固定 `ping` 主动探针，缺通用上游订阅账本、固定费用分摊、共享余额预留、到期预测、多层故障域、会话 TTFT 预算、探索预算和完整 Attempt 费用；补齐需要修改 relay、Site、账务、健康任务和管理面。详细证据仍固定在 [Site/Account 模型](https://github.com/Hureru/octopus/blob/0e1a7ee062c8a24a9c8b95e7d1778f265182a304/internal/model/site.go#L14) 和 [尝试指标](https://github.com/Hureru/octopus/blob/0e1a7ee062c8a24a9c8b95e7d1778f265182a304/internal/relay/metrics.go#L17)。

## 8. 适配和维护判断

| 使用方式 | 侵入等级 | 判断 |
| --- | --- | --- |
| 私有 model alias + 单资源 Group/Channel/Key/URL | L0 | 技术可行，但约束脆弱，只作次级适配 |
| 正式 Channel/Key/URL 路由契约及可靠取消回执 | L2 | 需要修改 Relay、认证和日志 |
| 在任一 Octopus 内实现完整 SLA 状态和决策 | L3 | 不接受长期私有分支 |

**结论：两个 Octopus 均不作为最终基础。** bestruirui master 的部署简单，但存在明确可靠性缺陷；Hureru 功能更多，却已是独立大型旁支。两者只保留为低优先级执行适配或协议实现参考。
