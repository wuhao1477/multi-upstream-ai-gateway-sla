# 多上游 AI 网关技术选型决策图

| 项目 | 内容 |
| --- | --- |
| 状态 | 调查完成，已形成唯一推荐 |
| 更新日期 | 2026-07-23 |
| 对齐 PRD 版本 | v1.2（含 FR-033～039 订阅、FR-110～117 平台可靠性与协议等） |
| 目标 | 在现有开源网关上，以最低核心改动实现 PRD 要求 |
| 首选集成方式 | 外部控制能力或正式扩展点 |
| 明确避免 | 大范围修改核心路由、长期维护私有分叉 |

## 调查原则

### 无侵入等级

| 等级 | 定义 | 选型态度 |
| --- | --- | --- |
| L0 | 不改源码，通过管理接口、Webhook、事件、动态配置或标准协议集成 | 首选 |
| L1 | 使用项目正式提供的插件、中间件、策略接口或适配器 | 可接受 |
| L2 | 在单一稳定边界增加小规模能力，改动可独立提交上游 | 谨慎接受 |
| L3 | 修改核心路由、请求链路或多个状态模块，并长期维护分叉 | 原则上淘汰 |

### 硬性判据

候选项目必须逐项给出源码或官方文档证据：

1. 支持目标模型协议、流式输出、工具调用和完整错误透传。
2. 能够管理多个渠道、账号和 Key，并区分独立额度与共享余额。
3. 能够从外部读取或更新渠道状态、权重、启停和路由相关配置。
4. 能够获得请求级首字、完整结果、失败、重试、缓存和费用信息。
5. 能够在首字前执行接管，或提供足够稳定的外部边界补充该能力。
6. 能够保存渠道健康、冷却、容量和会话亲和所需状态。
7. 核心维护活跃，发布、问题处理和升级路径可判断。

按本轮要求，许可证不参与淘汰、排序和最终推荐。

### 特别核查

- `Webhook` 必须区分同步请求前控制、异步请求后通知和单纯告警；只有请求前控制点才能直接改变当次路由。
- “自动回退”必须核查是否支持流式首字前接管、动态等待时间、取消原请求和重复费用记录。
- “成本控制”必须核查是否支持真实价格、倍率、账号余额、Key 额度和失败成本，而非仅按静态 Token 单价统计。
- “负载均衡”必须核查是否允许会话级预算、缓存亲和和故障域限制，而非仅轮询或固定权重。

### 证据要求

- 每个仓库调查必须固定到具体提交 SHA，不能只引用持续变化的默认分支。
- README 仅作为功能声明；扩展点、路由链路和状态能力必须由官方文档或源码证明。
- 克隆源码后，优先使用 CodeGraph 追踪请求入口、路由选择、失败处理、流式响应、事件和配置调用链；索引不存在时只初始化一次。
- 每项结论必须附源码文件、符号或官方文档链接，并注明验证版本。
- 不使用缺少证据的数值打分。先按硬性执行能力淘汰，再按侵入等级、覆盖范围及维护成本选择唯一推荐项。

## 候选仓库基线

以下仅为 2026-07-21 获取的仓库元数据，不代表选型结论。

| 候选 | 默认分支 | 研究分支 | 调查结论 |
| --- | --- | --- | --- |
| `looplj/axonhub` | `unstable` | `unstable` | 进入最终推荐，固定 SHA 见基线 |
| `Hureru/octopus` | `dev` | `dev` | 大型独立旁支；不推荐 |
| `bestruirui/octopus` | `dev` | `master` | 可受限 L0 拓扑绑定；不作为基础 |
| `fawney19/Aether` | `main` | `main` | 原生领域能力强；列入优先候选 |
| `diegosouzapw/OmniRoute` | `release/v3.8.49` | `main` | 可受限 L0 单 Connection 绑定；不作为基础 |
| `caidaoli/ccLoad` | `master` | `master` | Channel allowlist 支持 L0；轻量备选 |
| `zhfeng1/ai-gateway` | `main` | `main` | 单上游透明代理；排除 |
| `QuantumNous/new-api` | `main` | `main` | 成熟渠道管理与计费平台；列入备选 |
| `Wei-Shaw/sub2api` | `main` | `main` | 账号池调度较强；列入专业化候选 |

## #1: 技术选型目标与边界

Blocked by: 无

Type: Grilling

### Question

本次技术选型需要解决什么问题，什么结果不可接受？

### Answer

选型目标是复用成熟网关的数据转发、协议兼容、渠道管理和基础故障处理能力，把价格、余额、SLA 会话预算、缓存亲和及业务测活尽量放在外部控制能力或正式扩展点中。

优先选择 L0/L1；L2 必须证明改动集中、可测试、升级冲突小且适合提交上游；L3 原则上淘汰。技术选型只评估可行性与维护成本，不在本阶段编写开发计划。

## #2: 候选身份、分叉关系与维护状态

Blocked by: #1

Type: Research

### Question

每个候选项目的真实上游、分叉关系、最新稳定版本和维护状态是什么？

### Answer

已完成。固定 SHA、维护快照和 Hureru 的分叉差异见 [仓库固定基线](./research/00-repository-baseline.md)。Hureru 与 `bestruirui/octopus` 已形成 424 个文件差异；许可证只作元数据记录，不参与本轮技术排序。

## #3: AxonHub 深度评估

Blocked by: #2

Type: Research

### Question

`looplj/axonhub` 是否能通过外部接口或正式扩展点满足核心需求，所需侵入等级是多少？

### Answer

已完成。AxonHub 具备候选过滤、权重、配额、首内容前顺序回退、执行记录、缓存 Token 成本和管理 API。推荐通过单渠道 API Key Profile 由外部 SLA 决策网关逐请求选资源，AxonHub 本身保持 L0；完整证据见 [AxonHub 评估](./research/axonhub.md)。

## #4: Octopus 系列深度评估

Blocked by: #2

Type: Research

### Question

`bestruirui/octopus` 与 `Hureru/octopus` 各自提供哪些能力，哪个分支更适合作为长期基础？

### Answer

已完成。bestruirui `master` 可用私有 model alias + 单资源 Group/Channel 构造受限 L0，但没有正式 Channel/Key 契约，并存在首内容、状态码和统计缺陷。Hureru 技术能力更强，却已是大型独立旁支；两者均不作为基础。证据见 [Octopus 系列评估](./research/octopus.md)。

## #5: Aether 深度评估

Blocked by: #2

Type: Research

### Question

`fawney19/Aether` 的项目定位、成熟度和扩展能力是否满足生产网关要求？

### Answer

已完成。Aether 的 Provider/Key/Pool/路由/额度/观测基础较完整，并可通过预建单 Key Routing Group 以 L0 接入外部决策层；但流式直通不能普遍保证首个有效内容前切换，动态 TTFT 仍由外部层执行。证据见 [Aether 评估](./research/aether.md)。

## #6: OmniRoute 深度评估

Blocked by: #2

Type: Research

### Question

`diegosouzapw/OmniRoute` 的多供应商、额度感知和自动回退能力能否被独立复用，还是与桌面端及其他功能高度绑定？

### Answer

已完成。OmniRoute `main` 可通过“单 Key Connection + 单 Connection 白名单 API Key”构造受限 L0；宽权限 API Key 下 session pin 可能覆盖指定 Connection。提前 keepalive、宽松 readiness、Combo 取消信号和 Attempt 持久化均不满足严格 SLA，且产品耦合范围大；不作为最终基础。证据见 [OmniRoute 评估](./research/omniroute.md)。

## #7: ccLoad 深度评估

Blocked by: #2

Type: Research

### Question

`caidaoli/ccLoad` 的动态路由、冷却、多地址调度和实时监控能否通过外部控制实现本项目的 SLA 策略？

### Answer

已完成。ccLoad `master` 的 API Token 原生支持 Channel allowlist；采用单 Key/URL Channel 可形成清晰的 L0 单资源绑定。其流提交仍可能被元事件提前触发，且缺共享余额、故障域、真实业务测活和可关联 Attempt 账本；上调为轻量执行备选，不作为最终基础。证据见 [ccLoad 评估](./research/ccload.md)。

## #8: zhfeng1/ai-gateway 深度评估

Blocked by: #2

Type: Research

### Question

`zhfeng1/ai-gateway` 是生产网关、调试代理还是观察工具，是否具备可复用扩展基础？

### Answer

已完成。该项目是单上游透明代理和请求查看器，不具备多渠道调度数据面；补齐目标能力接近重写，因此排除。证据见 [zhfeng1/ai-gateway 评估](./research/zhfeng1-ai-gateway.md)。

## #9: PRD 能力覆盖矩阵

Blocked by: #3, #4, #5, #6, #7, #8, #14, #15

Type: Research

### Question

各候选项目对 PRD P0 需求的现有支持、外部可扩展支持和缺失项分别是什么？

### Answer

已完成，并于 2026-07-22 对齐 PRD v1.1。矩阵按 FR-001～FR-105 分组（含 v1.1 新增的 FR-033～039 订阅计划与额度），区分原生覆盖、L0 外置、L1/L2 和 L3 核心改造；已加入 NewAPI 与 Sub2API。九个候选均无现成订阅台账能力，订阅台账、共享额度识别、到期未用预测和引流上限统一由外部决策核心以 L0 外置承担；全部候选的价格可信版本、共享余额、会话、测活和 SLA 账目仍需外部控制组件。见 [PRD 能力覆盖矩阵](./research/capability-matrix.md)。

## #10: 外部 SLA 控制边界

Blocked by: #9

Type: Research

### Question

价格、余额、健康学习、会话预算和业务测活中，哪些可放在外部控制服务，哪些必须进入请求链路？

### Answer

已完成。同步请求前准入、逐请求资源选择、首字前取消/切换必须由外部 SLA 决策网关完成；价格采集、余额核对、测活和告警走异步控制路径；AxonHub Webhook 仅作异步输入。见 [外部 SLA 控制边界](./research/control-boundary.md)。

## #11: 维护成本与上游合并可能性

Blocked by: #3, #4, #5, #6, #7, #8, #14, #15

Type: Research

### Question

候选项目的升级频率、核心变更规模、测试基础和上游贡献流程会带来多大长期维护成本？

### Answer

已完成。AxonHub 推荐路线不改源码，不维护私有分叉。ccLoad、OmniRoute 和 bestruirui/octopus 也可通过预建单资源对象保持 stock 源码，但约束强度和执行证据依次减弱；动态首内容和 Attempt 账本均需外置。升级契约、上游 PR 边界和风险见 [维护性判断](./research/maintenance.md)。

## #12: 候选短名单与验证范围

Blocked by: #9, #10, #11

Type: Decision

### Question

哪一个候选最接近 L0/L1，最小验证需要证明哪些关键假设？

### Answer

已完成。重新研究 OmniRoute `main`、bestruirui/octopus `master` 和 ccLoad `master` 后，排名第一仍是 AxonHub；ccLoad 上调为轻量 L0 备选。选型生效条件验证 Profile 隔离、流式首内容/取消和执行回执，不在本票为其他候选开发原型。

## #13: 最终技术选型

Blocked by: #12

Type: Research

### Question

哪个项目应作为基础，采用何种无侵入集成边界，哪些项目应被明确排除？

### Answer

已完成。唯一推荐仍为 AxonHub + 外部 SLA 决策网关，AxonHub 保持 L0。Aether、ccLoad、Sub2API、OmniRoute、NewAPI 和 Octopus 均有不同程度的单资源执行方式，但在边界直接性、流式接管、通用性或 Attempt 证据上弱于推荐项。完整结论见 [技术选型结论](./TECHNICAL-SELECTION.md)。

## #14: NewAPI 深度评估

Blocked by: #1

Type: Research

### Question

`QuantumNous/new-api` 能否作为外部 SLA 决策层的通用执行数据面？

### Answer

已完成。NewAPI 的渠道、模型、倍率、计费和管理能力成熟，但外部精确控制依赖管理员 Token 指定 Channel，Channel 内多 Key 仍由内部随机或轮询；缺少首个有效内容期限和尝试级账目。采用时需一个真实 Key 一个 Channel，并由外部层执行首内容接管。见 [NewAPI 评估](./research/newapi.md)。

## #15: Sub2API 深度评估

Blocked by: #1

Type: Research

### Question

`Wei-Shaw/sub2api` 的账号池调度能否作为二十个异构上游的通用 SLA 执行数据面？

### Answer

已完成。Sub2API 原生账号、额度、调度、TTFT 和成本能力较强，OpenAI 路径还有首语义输出保护；但公开请求不能精确指定真实账号，跨平台首内容能力不统一，平台集合也不是任意 Provider 插件。L0 需每账号独立 Group/API Key。见 [Sub2API 评估](./research/sub2api.md)。

## #16: 无许可因素重排

Blocked by: #9, #10, #11

Type: Decision

### Question

完全忽略许可证后，哪个候选最适合作为平台中立 SLA 决策层的执行数据面？

### Answer

已完成。最终仍推荐 AxonHub。它通过单 Key Channel 和单渠道 Profile 提供最直接的 L0 资源选择，并具备流式预读和较完整的执行记录。ccLoad 的 Channel Token 是可用轻量备选；OmniRoute Connection 白名单和 Octopus model alias 需要更严格的配置约束。全部候选的首有效内容与财务级 Attempt 账本仍由外部 SLA 网关负责。
