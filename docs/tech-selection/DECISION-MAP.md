# 多上游 AI 网关技术选型决策图

| 项目 | 内容 |
| --- | --- |
| 状态 | 调查准备完成，尚未形成选型结论 |
| 更新日期 | 2026-07-21 |
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

1. 许可证允许目标部署和后续修改方式，且不存在无法接受的披露义务。
2. 支持目标模型协议、流式输出、工具调用和完整错误透传。
3. 能够管理多个渠道、账号和 Key，并区分独立额度与共享余额。
4. 能够从外部读取或更新渠道状态、权重、启停和路由相关配置。
5. 能够获得请求级首字、完整结果、失败、重试、缓存和费用信息。
6. 能够在首字前执行接管，或提供足够稳定的扩展点补充该能力。
7. 能够保存渠道健康、冷却、容量和会话亲和所需状态。
8. 核心维护活跃，发布、问题处理和升级路径可判断。

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
- 不使用缺少证据的数值打分。先按许可证和硬性能力淘汰，再按侵入等级、覆盖范围及维护成本选择唯一推荐项。

## 候选仓库基线

以下仅为 2026-07-21 获取的仓库元数据，不代表选型结论。

| 候选 | 默认分支 | GitHub 识别许可证 | 当前需要确认 |
| --- | --- | --- | --- |
| `looplj/axonhub` | `unstable` | Other | 实际许可证、扩展接口、外部控制能力 |
| `Hureru/octopus` | `dev` | AGPL-3.0 | 与上游 Octopus 的关系及新增能力 |
| `bestruirui/octopus` | `dev` | AGPL-3.0 | 原始项目能力、扩展边界、商用义务 |
| `fawney19/Aether` | `main` | Other | 实际许可证、项目定位和成熟度 |
| `diegosouzapw/OmniRoute` | `release/v3.8.49` | MIT | 产品范围是否过宽、路由扩展边界 |
| `caidaoli/ccLoad` | `master` | MIT | 动态路由、冷却和监控是否可外部控制 |
| `zhfeng1/ai-gateway` | `main` | 未识别 | 是否存在有效许可证及可用于生产的网关能力 |

## #1: 技术选型目标与边界

Blocked by: 无

Type: Grilling

### Question

本次技术选型需要解决什么问题，什么结果不可接受？

### Answer

选型目标是复用成熟网关的数据转发、协议兼容、渠道管理和基础故障处理能力，把价格、余额、SLA 会话预算、缓存亲和及业务测活尽量放在外部控制能力或正式扩展点中。

优先选择 L0/L1；L2 必须证明改动集中、可测试、升级冲突小且适合提交上游；L3 原则上淘汰。技术选型只评估可行性与维护成本，不在本阶段编写开发计划。

## #2: 候选身份、分叉关系与许可证

Blocked by: #1

Type: Research

### Question

每个候选项目的真实上游、分叉关系、许可证义务、最新稳定版本和维护状态是什么？

### Answer

待调查。重点确认 `Hureru/octopus` 与 `bestruirui/octopus` 的提交关系，以及 AxonHub、Aether、`zhfeng1/ai-gateway` 的实际许可证内容。调查资产写入 `research/00-repository-baseline.md`。

## #3: AxonHub 深度评估

Blocked by: #2

Type: Research

### Question

`looplj/axonhub` 是否能通过外部接口或正式扩展点满足核心需求，所需侵入等级是多少？

### Answer

待调查。必须追踪一次完整请求的路由、流式输出、失败接管、观测和配置更新路径，并将证据写入 `research/axonhub.md`。

## #4: Octopus 系列深度评估

Blocked by: #2

Type: Research

### Question

`bestruirui/octopus` 与 `Hureru/octopus` 各自提供哪些能力，哪个分支更适合作为长期基础？

### Answer

待调查。除功能外，必须比较两者差异规模、上游同步方式、AGPL 影响、外部控制能力和核心改动位置。调查资产写入 `research/octopus.md`。

## #5: Aether 深度评估

Blocked by: #2

Type: Research

### Question

`fawney19/Aether` 的项目定位、成熟度、许可和扩展能力是否满足生产网关要求？

### Answer

待调查。调查资产写入 `research/aether.md`。

## #6: OmniRoute 深度评估

Blocked by: #2

Type: Research

### Question

`diegosouzapw/OmniRoute` 的多供应商、额度感知和自动回退能力能否被独立复用，还是与桌面端及其他功能高度绑定？

### Answer

待调查。调查资产写入 `research/omniroute.md`。

## #7: ccLoad 深度评估

Blocked by: #2

Type: Research

### Question

`caidaoli/ccLoad` 的动态路由、冷却、多地址调度和实时监控能否通过外部控制实现本项目的 SLA 策略？

### Answer

待调查。调查资产写入 `research/ccload.md`。

## #8: zhfeng1/ai-gateway 深度评估

Blocked by: #2

Type: Research

### Question

`zhfeng1/ai-gateway` 是生产网关、调试代理还是观察工具，是否具备可接受许可证和扩展基础？

### Answer

待调查。调查资产写入 `research/zhfeng1-ai-gateway.md`。

## #9: PRD 能力覆盖矩阵

Blocked by: #3, #4, #5, #6, #7, #8

Type: Research

### Question

各候选项目对 PRD P0 需求的现有支持、外部可扩展支持和缺失项分别是什么？

### Answer

待调查。结果必须逐项引用 PRD 的 `FR-xxx` 编号，并区分“已有”“L0/L1 可补充”“L2 可补充”“L3 或不可行”。资产写入 `research/capability-matrix.md`。

## #10: 外部 SLA 控制边界

Blocked by: #9

Type: Research

### Question

价格、余额、健康学习、会话预算和业务测活中，哪些可放在外部控制服务，哪些必须进入请求链路？

### Answer

待调查。必须明确同步请求前控制、异步结果回传、配置刷新时效和故障时的默认行为。资产写入 `research/control-boundary.md`。

## #11: 维护成本与上游合并可能性

Blocked by: #3, #4, #5, #6, #7, #8

Type: Research

### Question

候选项目的升级频率、核心变更规模、测试基础、上游贡献流程和许可证会带来多大长期维护成本？

### Answer

待调查。必须给出预计侵入等级、需要修改的模块数量、上游 PR 可接受性依据和升级冲突风险。资产写入 `research/maintenance.md`。

## #12: 候选短名单与验证范围

Blocked by: #9, #10, #11

Type: Prototype

### Question

哪一个候选最接近 L0/L1，最小验证需要证明哪些关键假设？

### Answer

待调查。本票只允许为排名第一的候选定义小范围验证，不为全部候选同时开发原型。

## #13: 最终技术选型

Blocked by: #12

Type: Research

### Question

哪个项目应作为基础，采用何种无侵入集成边界，哪些项目应被明确排除？

### Answer

待调查。最终只给出一个推荐方案，并附许可证、侵入等级、PRD 覆盖、维护成本和验证证据。
