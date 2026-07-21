# Sub2API 源码评估

| 项目 | 内容 |
| --- | --- |
| 仓库 | `Wei-Shaw/sub2api` |
| 评估提交 | `b8b72e1b18310c908668e79112e43c1e7c682696`（`main`） |
| 最近发布 | `v0.1.162` |
| 许可因素 | 按本轮要求，不参与排序 |
| 结论 | 原生账号调度能力强；属于专业化候选，不作为通用 SLA 执行数据面首选 |

## 1. 适用定位

Sub2API 面向 Anthropic、OpenAI、Gemini、Antigravity 和 Grok 等账号池，强调 OAuth/API Key 账号管理、并发分配、订阅共享、计费和模型协议接入。它在账号级调度和额度状态方面明显强于一般聚合网关。

它不是任意上游平台的热插拔 Provider 框架。大量请求链和认证逻辑按已知平台及账号类型实现，因此更适合管理受支持的官方或订阅账号池，而不是直接聚合二十个结构各异的 NewAPI/Sub2API 二开站点。

## 2. 已验证能力

### 账号、分组和额度

- Account 保存平台、认证类型、凭证、代理、并发、优先级、倍率、可调度状态、限流恢复时间和会话窗口；见 [Account Schema](https://github.com/Wei-Shaw/sub2api/blob/b8b72e1b18310c908668e79112e43c1e7c682696/backend/ent/schema/account.go#L50)。
- Group 支持倍率、日/周/月额度、模型路由、支持的模型范围和 fallback；见 [Group Schema](https://github.com/Wei-Shaw/sub2api/blob/b8b72e1b18310c908668e79112e43c1e7c682696/backend/ent/schema/group.go#L45)。
- API Key 支持分组、总额度和时间窗口用量限制；见 [API Key Schema](https://github.com/Wei-Shaw/sub2api/blob/b8b72e1b18310c908668e79112e43c1e7c682696/backend/ent/schema/api_key.go#L34)。

### 调度

- 通用选择器考虑分组、平台、模型、排除集合和会话粘性；见 [Gateway Scheduling](https://github.com/Wei-Shaw/sub2api/blob/b8b72e1b18310c908668e79112e43c1e7c682696/backend/internal/service/gateway_scheduling.go#L23)。
- OpenAI 高级调度器综合优先级、负载、排队、错误率、TTFT、额度余量、上游成本及会话粘性；见 [OpenAI Account Scheduler](https://github.com/Wei-Shaw/sub2api/blob/b8b72e1b18310c908668e79112e43c1e7c682696/backend/internal/service/openai_account_scheduler.go#L920)。
- 会话粘性来自系统内部 Redis 绑定，不是客户端可指定的账号选择接口。

### 首输出和观测

- OpenAI 流式路径可配置首个语义输出期限，在提交前暂存事件，并在超时后产生可切换错误；见 [First Output Guard](https://github.com/Wei-Shaw/sub2api/blob/b8b72e1b18310c908668e79112e43c1e7c682696/backend/internal/service/openai_gateway_forward.go#L751) 和 [Response Staging](https://github.com/Wei-Shaw/sub2api/blob/b8b72e1b18310c908668e79112e43c1e7c682696/backend/internal/service/openai_gateway_response_handling.go#L179)。
- Usage Log 保存账号、请求/上游模型、Channel、Token、缓存 Token、成本、倍率、总时长和首 Token；见 [Usage Log](https://github.com/Wei-Shaw/sub2api/blob/b8b72e1b18310c908668e79112e43c1e7c682696/backend/ent/schema/usage_log.go#L31)。
- 失败尝试可进入 Ops Error JSON，但成功和失败尝试没有统一的持久化 Attempt 表；见 [Upstream Error Event](https://github.com/Wei-Shaw/sub2api/blob/b8b72e1b18310c908668e79112e43c1e7c682696/backend/internal/service/ops_upstream_context.go#L190)。

## 3. 与 SLA 需求的关键差距

### 外部精确账号选择

公开执行链只接收 Group、Session、Model 和内部排除集合，没有受认证的 `account_id` 或真实 Key 选择契约。管理员配置的 Group Model Routing 也是静态配置。

L0 可以为每个真实账号建立独立 Group 和内部 API Key，由外部 SLA 网关选择该 Key；但这会显著增加 Account、Group 和 Key 数量，跨候选接管也需要外部重新发起请求。

### 跨平台首个有效内容

首个语义输出期限主要覆盖 OpenAI 特定路径。通用 Anthropic/Gemini 等流式路径以数据间隔超时为主，不能统一保证首个用户可见有效内容前完成取消和候选切换。因此不能把 OpenAI 路径的能力推导为全平台 SLA 能力。

### 价格、余额和测活

- 用户钱包、订阅额度、Group/Account 倍率和模型成本较完整，但不等于各外部渠道的采购余额和 Key 剩余额度。
- Upstream Billing Probe 只面向 OpenAI API Key 的 `/v1/sub2api/billing`，默认定时获取倍率快照，不是通用余额采集；见 [Billing Probe](https://github.com/Wei-Shaw/sub2api/blob/b8b72e1b18310c908668e79112e43c1e7c682696/backend/internal/service/upstream_billing_probe.go#L549)。
- Channel Monitor 使用指定 Endpoint、API Key 和模型做管理员合成检测，不是从真实业务流量分配探索机会；见 [Channel Monitor](https://github.com/Wei-Shaw/sub2api/blob/b8b72e1b18310c908668e79112e43c1e7c682696/backend/internal/service/channel_monitor_service.go#L428)。
- 未发现通用运行时插件注册器。

### 运维范围

单节点默认仍需要应用、PostgreSQL 和 Redis。项目功能面和数据库模型较大，适合需要账号池、订阅及计费能力的场景，但作为纯执行数据面比 AxonHub 和 NewAPI 更重。

## 4. 适配器边界

| 集成方式 | 侵入等级 | 判断 |
| --- | --- | --- |
| 每真实账号一个 Group + 内部 API Key | L0 | 可精确选择，但配置对象数量增加，接管由外部层完成 |
| 增加受认证的 `account_id` 选择并保留权限审计 | L2 | 修改单一调度入口，但必须覆盖各平台请求链 |
| 让外部 RoutePlan 替换内部 Scheduler，并统一跨平台首内容接管 | L3 | 涉及调度、流式、错误和账务多个核心模块，不接受 |

Sub2API Adapter 必须声明平台能力差异，尤其是 `first_valid_content_deadline`。不支持的字段必须返回 `degraded_fields`，不能静默使用数据间隔超时代替。

## 5. 采用判断

**列入第一梯队的专业化候选，但不作为最终推荐。** 若主要资源是 OpenAI/Anthropic/Gemini/Grok 账号池，Sub2API 的原生调度、并发和额度能力很有价值；本项目需要聚合约二十个异构上游，并由外部 SLA 层逐请求选择真实资源，因此它的通用性和外部控制边界不如 AxonHub。
