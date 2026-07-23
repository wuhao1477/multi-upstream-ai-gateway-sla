# 多上游 AI 网关技术选型结论

| 项目 | 结论 |
| --- | --- |
| 选型日期 | 2026-07-21（2026-07-23 对齐 PRD v1.2） |
| 对齐 PRD 版本 | v1.2（含 FR-033～039 订阅、FR-110～117 平台可靠性与协议等） |
| 关联验证 | [ISSUE-001](../issues/ISSUE-001-tech-assumption-verification.md) 6 项假设源码级预验证已完成 |
| 评估范围 | AxonHub、Aether、OmniRoute、NewAPI、Sub2API、Hureru/octopus、bestruirui/octopus、ccLoad、zhfeng1/ai-gateway |
| 许可证因素 | 不参与淘汰、排序和最终推荐 |
| 指定分支复核 | OmniRoute `main@698b6eb0`；bestruirui/octopus `master@b7b053e7`；ccLoad `master@665fec14` |
| 唯一推荐执行数据面 | `looplj/axonhub` |
| 推荐集成方式 | 平台无关 SLA 决策核心 + GatewayAdapter + stock AxonHub |
| AxonHub 源码侵入 | L0；不修改上游源码 |
| 选型状态 | 通过架构选型；不等同于生产验收 |

## 1. 最终推荐

**唯一推荐：AxonHub 作为执行数据面，外部 SLA 决策核心负责逐请求资源决策。**

忽略许可证后，AxonHub 仍然最适合本项目，原因不是它原生业务功能最多，而是它与外部决策层的边界最清晰：

1. 可用单渠道 API Key Profile 把一次请求限制到一个真实资源，适合 Key 级成本、余额和缓存归因。
2. 已有流式提交前预读、取消、顺序回退和 Request Execution/Usage Log，可提供外部账本所需的原始执行证据。
3. 管理 API 能处理渠道、Key、Profile、权重和状态，外部控制组件无需修改核心路由。
4. 价格可信版本、共享余额、会话 TTFT、故障域、真实业务测活和重复费用仍由外部 SLA 核心维护，不会形成第三方私有分支。

AxonHub 也不是完整 SLA 成品。动态会话等待时间、并行接管、取消后继续计费和全量探索账目仍必须由外部同步请求层承担。

三个指定分支复核后，最接近推荐边界的是 ccLoad：其单 Channel allowlist Token 可形成 L0 绑定。但 AxonHub 的 Profile 直接表达单渠道授权，执行记录也更接近请求尝试；ccLoad 仍依赖“单 Channel + 单 Key + 单 URL + 单 Token”拓扑约束，且缺统一 Attempt 关联。因此唯一推荐不变。

## 2. 全部候选摘要

| 平台 | 主要定位 | 可复用优势 | 关键不足 | 适配判断 | 结论 |
| --- | --- | --- | --- | --- | --- |
| AxonHub | 通用多供应商执行网关 | 单资源 Profile、流式提交前预读/回退、执行记录、缓存 Token 成本、管理 API | 首解析事件不一定等于用户可见内容；会话预算、真实测活、共享余额和重复费用需外置 | L0 | **唯一推荐** |
| Aether | 资源/Key/路由/额度模型较完整的通用网关 | Routing Group、`allowed_keys`、NewAPI/Sub2API 余额、候选与用量记录、会话亲和 | 普通流式路径不能普遍保证首个有效内容前切换；不能按请求下发任意候选序列和期限；Pool 会削弱 Key 精确过滤 | L0 预设组；完整接管需外置或 L2 | 优先备选 |
| OmniRoute `main` | 插件化、多 Provider、Combo 调度网关 | 单 Connection 白名单可受限 L0；Provider 覆盖、Plugin/Webhook、额度和缓存指标丰富 | keepalive 和元事件早于有效内容；宽权限 Key 下亲和可覆盖指定 Connection；Combo 取消与 Attempt 账目不完整 | 受限 L0；完整契约 L2 | 条件适配 |
| Sub2API | 官方/订阅账号池与计费网关 | 账号并发、优先级、负载、TTFT、额度余量、会话粘性；OpenAI 有首语义输出保护 | 公开请求不能精确指定 Account；跨平台首内容能力不一致；平台集合不是任意 Provider 插件；需 PostgreSQL/Redis | 单账号 Group/API Key 为 L0；通用控制需 L2/L3 | 专业化候选 |
| NewAPI | 渠道管理、协议转换和站内计费平台 | 渠道/模型/分组、倍率、余额、权重、重试、轻量部署和大生态 | 普通调用方不能指定 Channel；Channel 内多 Key 随机/轮询；无首有效内容期限和统一尝试账目 | 单 Key 独立 Channel 为 L0；通用 RoutePlan 需 L2/L3 | 渠道管理备选 |
| Hureru/octopus | 聚合站、Site/Account/Token 模型 | 账号余额、缓存、费用、熔断和会话能力较接近目标 | 外部策略入口弱，补齐价格、测活、故障域和账务需改多个核心模块 | L3 | 不作为基础 |
| bestruirui/octopus `master` | 轻量多渠道代理 | 私有 model alias + 单资源 Group/Channel 可构造 L0；有静态首 Token 超时和 Attempt 数组 | 无正式 Channel Binding；超时从响应头后开始；存在 429 状态、统计和参数覆盖缺陷 | 受限 L0；正式契约 L2；内置 SLA L3 | 低优先级适配 |
| ccLoad `master` | 轻量多协议故障转移代理 | Token Channel allowlist、延迟提交、取消、细分缓存/成本、models.dev 价格和多层冷却 | 元事件可提前提交；无共享余额、故障域、真实业务测活及可关联 Attempt 账本 | L0 单资源绑定；内置 SLA L3 | **轻量备选** |
| zhfeng1/ai-gateway | 单上游透明代理与调试查看器 | 原始请求、响应和首字节观察 | 没有多渠道调度、资源账目或执行数据面 | 接近重写 | 排除 |

## 3. 推荐集成边界

### SLA 决策核心负责

- 维护 `真实上游 → 账号 → Key → 模型/权限 → 地区/故障域` 资源目录。
- 定时采集 NewAPI、Sub2API 及二开版本的价格、倍率、余额、额度和权限，并保存来源与版本。
- 按价格可信度、确认余额、在途费用预留、安全储备、容量和故障域生成候选。
- 维护会话前缀 TTFT、缓存亲和、真实业务测活、探索预算、冷却和三类账目。
- 在首个有效内容提交前观察流，按剩余预算取消当前请求、选择下一资源并记录每次尝试。

### GatewayAdapter 负责

- 将平台无关的 `RoutePlan` 映射为目标平台的执行绑定。
- 转发流并报告响应头、首个有效内容、取消请求、取消确认和结束状态。
- 证明实际使用的 Provider、账号和 Key，并回传平台请求 ID、用量、缓存和费用。
- 声明能力，不支持的字段必须返回 `degraded_fields`，不能静默丢弃动态期限或候选顺序。

### 上游元数据适配器负责

NewAPI 和 Sub2API 既可能是候选平台，也可能是上游账号管理系统。价格、倍率和余额采集应独立于 Aether、AxonHub 等执行适配器，避免把某个平台的内部计费字段误认为真实采购成本。

## 4. 资源建模约束

推荐平台无关模型：

- `UpstreamResource`：真实站点、账号、Key、模型权限、余额关系、故障域和缓存作用域。
- `ExecutionBinding`：该资源在某个平台中的引用，例如 AxonHub Profile、Aether Routing Group、NewAPI Channel 或 Sub2API Group/API Key。
- `RoutePlan`：候选顺序、每次首个有效内容期限、缓存目标、故障域限制、费用上限、测活标记和取消规则。
- `AttemptResult`：实际资源、平台请求 ID、首字、完整结果、取消状态、Token、缓存、费用和是否已向客户端提交。

所有适配器都应采用“一个真实资源对应一个执行绑定”。除既有 AxonHub、Aether、NewAPI 和 Sub2API 映射外，ccLoad 使用单 Key/URL Channel + 单 Channel Token，OmniRoute 使用单 Key Connection + 单 Connection 白名单 API Key，Octopus 只能使用私有 model alias + 单资源 Group/Channel。把多个真实 Key 留给平台内部自由轮换，会破坏缓存亲和、余额归因和故障隔离。

## 5. 对业务目标的直接回答

| 目标 | 推荐边界 |
| --- | --- |
| 连续会话前缀平均首字时间 ≤10 秒 | SLA 核心按每轮实际 TTFT 和剩余预算选择 Binding；执行平台只负责转发和回执 |
| 低价与快速渠道动态分配 | SLA 核心按实际成功成本、价格版本、余额、容量和会话缓存选择；平台全局权重只作慢速调节 |
| 缓存率 ≥90% | SLA 核心维护会话/Key/模型缓存作用域和切换损失；执行平台提供缓存 Token 记录 |
| 价格和倍率变动 | NewAPI/Sub2API 采集器形成可信版本；过期或异常时限制付费流量 |
| 账号余额和 Key 余额 | 外部账本维护共享关系、在途预留和安全储备，平台额度只作辅助证据 |
| 成本限制内提高订阅额度有效利用率 | SLA 核心负责订阅台账（FR-033/034）、共享订阅额度识别（FR-035）、额度消耗预测与到期未用风险判断（FR-036）、到期紧迫度调度约束（FR-037）、成本上限与订阅引流上限控制（FR-038），以及不可安全利用时允许到期并记录原因与损失（FR-039）；执行网关（AxonHub）只回传每次请求的用量、缓存和费用 |
| 无缝测活 | 使用合资格真实业务请求，由 SLA 核心维护探索预算和冷却；不依赖固定 `hello`/`ping` |
| 首字前接管 | 同步 SLA 层缓冲首个有效内容，按期限取消并切换；已提交有效内容后不得拼接另一响应 |
| 失败和重复费用 | 按 Attempt 记录所有请求、取消、继续计费、缓存损失和余额差异，不能只看最终响应 |

## 6. 为什么不能只依赖平台权重或 Webhook

全局权重无法表达不同会话的剩余 TTFT、缓存归属、探索资格、租户预算和故障域限制。Webhook 主要是异步状态通知，也不能在当次请求提交前返回候选顺序或动态期限。

因此推荐架构必须让 SLA 决策核心处于同步请求路径；平台适配器负责执行，不把核心 SLA 规则写入任何一个第三方网关。

## 7. 选型生效条件

以下是采用推荐边界前必须确认的技术事实，不是开发计划：

1. 固定 AxonHub 可复现提交，验证单渠道 Profile 不会回退到其他未授权渠道。
2. 验证客户端断开、SLA 接管取消和上游取消均可关联到同一 Attempt。
3. 验证首个有效内容定义不把响应头、空 SSE 或 heartbeat 当作首字。
4. 验证价格、余额、缓存 Token、费用和平台请求 ID 可以与外部账本关联。
5. 接受外部 SLA 决策核心是同步请求链路的必要组成；只部署平台本身不能满足本 PRD。
6. 对备用适配器逐项验证单资源拓扑不会被会话亲和、内部 Key/URL 轮换、隐藏重试或元事件提前提交破坏。

## 8. 证据索引

- [仓库固定基线](./research/00-repository-baseline.md)
- [AxonHub 评估](./research/axonhub.md)
- [Aether 评估](./research/aether.md)
- [OmniRoute 评估](./research/omniroute.md)
- [NewAPI 评估](./research/newapi.md)
- [Sub2API 评估](./research/sub2api.md)
- [Octopus 系列评估](./research/octopus.md)
- [ccLoad 评估](./research/ccload.md)
- [zhfeng1/ai-gateway 评估](./research/zhfeng1-ai-gateway.md)
- [PRD 能力矩阵](./research/capability-matrix.md)
- [外部控制边界](./research/control-boundary.md)
- [订阅数据采集验证](./research/subscription-data-verification.md)
- [维护性判断](./research/maintenance.md)

本结论只回答基础项目和集成边界，不包含技术栈、开发计划或实施排期。
