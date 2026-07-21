# AxonHub 源码评估

| 项目 | 内容 |
| --- | --- |
| 仓库 | `looplj/axonhub` |
| 评估提交 | `ed6119a168483a205a85a2f38c7153f5cf1b61a6`（`unstable`，2026-07-20） |
| 最近发布 | `v1.0.0-beta5`（提交 `d061ac7df6aef0c5ec6cdfa9dc5002546a1c5a57`，2026-07-11） |
| 许可因素 | 按本轮要求，不参与排序 |
| 结论 | 唯一推荐的数据面基础；不能直接承担完整 SLA 决策 |

## 1. 结论

AxonHub 是本轮候选中最适合作为执行数据面的项目。它已经具备多供应商协议转换、流式响应、首事件前顺序重试、渠道和 Key 管理、限流、配额检查、缓存 Token 成本及逐次执行记录，并提供可远程调用的管理接口。

推荐边界不是把 SLA 算法写入 AxonHub，而是：

- 外部 SLA 控制组件维护价格、共享余额、故障域、会话预算、缓存作用域、业务测活和策略审计。
- 每个“真实上游 + 账号 + Key + 模型权限”配置为独立 AxonHub 渠道，避免 AxonHub 在同一渠道内切 Key 破坏缓存亲和。
- 每个实际资源建立单渠道 API Key Profile；外部组件通过选择对应 Profile，逐请求指定执行资源。
- AxonHub负责协议适配、上游认证、流式转换、基础重试和执行记录。

该方式不修改 AxonHub 源码，侵入等级为 L0；代价是外部 SLA 控制组件必须处于同步请求链路，不能只做定时任务。

## 2. 已验证请求链路

标准 OpenAI 请求调用链为：

`ChatCompletion` → `ChatCompletionWithRequest` → `orchestrator.Process` → `pipeline.Process` → `selectCandidates` → `LoadBalancer.Sort` → `processRequest` → `stream` → `WriteSSEStreamWithErrorFormatter`。

关键源码：

- [OpenAI 请求入口](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/server/api/openai.go#L293)
- [Orchestrator 入口与扩展字段](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/server/orchestrator/orchestrator.go#L104)
- [候选过滤](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/server/orchestrator/select_candidates.go#L20)
- [负载排序接口](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/server/orchestrator/load_balancer.go#L28)
- [请求重试循环](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/llm/pipeline/pipeline.go#L256)
- [流式首事件等待与切换](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/llm/pipeline/stream.go#L169)

### 首字前接管的真实边界

AxonHub 会在响应提交给客户端前预读流事件。首事件超时或首内容前发生错误时，会关闭当前流、取消请求并顺序尝试下一候选；一旦已经向客户端交付有效内容，后续中断不会拼接另一模型响应。

因此现有能力是“首内容前串行回退”，不是 FR-076 所需的并行稳定资源接管。它也没有根据会话前缀 TTFT 余量动态计算本轮等待时间，未记录取消后继续计费和重复请求总费用。

## 3. 可直接复用的能力

| 能力 | 源码证据 | 判定 |
| --- | --- | --- |
| 渠道、多个上游 Key、启停 | [Channel Schema](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/ent/schema/channel.go#L35) | 可复用；建议一个实际 Key 对应一个渠道 |
| Profile 限制渠道、标签、模型和租户额度 | [API Key Profile](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/objects/apikey.go#L14) | 可作为 L0 逐请求资源选择机制 |
| Profile 在排序前过滤 | [候选过滤](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/server/orchestrator/select_candidates.go#L29) | 单渠道 Profile 可以确定执行资源 |
| 渠道权重 | [ordering_weight](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/ent/schema/channel.go#L140) | 适合慢速全局调节，不足以表达会话预算 |
| RPM、TPM、并发和队列 | [Channel Limits](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/objects/channel.go#L231) | 可作为容量硬过滤数据 |
| 多供应商配额检查 | [Provider Quota](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/server/biz/provider_quota.go#L328) | 可复用检查框架；NewAPI/Sub2API 仍需外部适配 |
| 价格版本 | [Price Version Schema](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/ent/schema/channel_model_price_versions.go#L28) | 有版本和生效区间，缺可信来源与币种语义 |
| 缓存 Token 成本 | [Cost Calculation](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/server/biz/cost_calc.go#L139) | 可计算读写缓存费用 |
| 每次执行的状态和时延 | [Request Execution Schema](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/ent/schema/request_execution.go#L37) | 重试尝试可追踪 |
| 用量、缓存 Token、成本和价格引用 | [Usage Log Schema](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/ent/schema/usage_log.go#L43) | 可作为执行回执，不应作为唯一账本 |
| 管理 GraphQL / Service Account API | [Routes](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/server/routes.go#L99) | 支持外部配置和状态控制 |

## 4. 外部控制和扩展边界

### L0：无需修改源码

管理接口可新增、更新、启停渠道和 Key，批量修改权重；配置变更会通知本机和多实例缓存。单渠道 API Key Profile 能把一次请求限定到指定资源。因此，外部组件可完成以下动作：

- 定时采集 NewAPI/Sub2API 价格、倍率和余额，并维护独立可信版本。
- 根据会话状态选择资源，再使用该资源对应的 AxonHub Profile 发起请求。
- 在首字前观察、取消并重新选择稳定资源。
- 根据执行结果调用管理接口降级、冷却或停用资源。

### Webhook：只能做异步通知

现有 Webhook 只确认到 `channel.auto_disabled`。渠道状态先写入数据库，再异步发送通知：

- [Webhook Notifier](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/server/biz/webhook_notifier.go#L20)
- [Auto Disable](https://github.com/looplj/axonhub/blob/ed6119a168483a205a85a2f38c7153f5cf1b61a6/internal/server/biz/channel_auto_disable.go#L20)

它不能在请求前返回候选顺序，也不能直接改变当次路由。

### L2：仅在不采用外部同步转发时需要

代码中已有 Middleware、评分策略和 `WithChannelSelector` 接口，但它们位于 Go `internal/` 包，stock 应用没有运行时插件注册器。若要求 AxonHub 自己调用外部决策服务，或标准 API 直接接收逐请求候选顺序，仍需增加受认证的路由决策输入及装配代码，属于 L2 小范围修改，不是现成 L1 插件。

## 5. 与 PRD 的差距

| PRD 范围 | 较强覆盖 | 部分覆盖 | 缺失或冲突 |
| --- | --- | --- | --- |
| FR-001～006 | FR-001 | FR-002～005 | FR-006；部分原生工具候选为空时会退回全部渠道 |
| FR-010～019 | FR-012、013 | FR-010、017 | 价格采集、可信确认、币种、汇率、扣费核对 |
| FR-020～032 | FR-028 | FR-020、021、025、027、031、032 | 共享账号余额、费用预留、保守余额、容量保留 |
| FR-040～047 | 无完整项 | FR-040、041、046、047 | 高分位、样本可信度和故障域 |
| FR-050～059 | 无完整项 | FR-054、055 | 会话前缀预算、缓存损失预测和实际成功成本 |
| FR-060～068 | 无 | 无 | 业务测活资格、探索预算、冷却与决策快照全部缺失 |
| FR-070～080 | FR-078 | FR-076、080 | 三类账目、补偿模式、动态等待、副作用和重复费用 |
| FR-090～099 | FR-093 | FR-094～098 | SLA 目标、错误预算、调度原因和完整变更审计 |
| FR-100～105 | 无完整项 | FR-100、101 的单一事件 | 事件合并、分级阈值、策略版本和周期摘要 |

其他必须在选型时明确的缺口：

- 配额状态 `unknown` 仍可进入候选，不符合保守余额下限要求。
- 首 Token 以首个解析事件计时，未证明一定是用户可见有效内容。
- 模型能力主要登记在模型级，不能完整表达某个实际 Key 的权限差异。
- 没有官方供应商、代理层、账号、地区和网络等结构化故障域。
- 现有 `channel probe` 是对真实历史流量做聚合，不会分配受预算控制的业务测活机会。

## 6. 维护判断

评估提交时仓库约 1,400 次提交，近 30 天 75 次、近 90 天 277 次，仍处于高频演进；最近发布版本仍为 beta。采用时必须固定提交，不应直接跟随 `unstable`。

## 7. 采用判断

**采用，定位为执行数据面。** 选型成立的必要条件是接受“外部 SLA 控制组件处于请求链路”这一边界。若要求 stock AxonHub 单独完成全部 PRD，或要求仅靠异步 Webhook 完成会话级路由，则结论为不可行。
