# 多上游 AI 网关 SLA 保障项目

统一接入约 20 个 AI 上游渠道，依据价格、余额、性能、稳定性、容量、缓存表现、模型能力和订阅额度动态分配请求，在满足可定义 SLA 的前提下降低实际服务成本。

本仓库为**需求、选型与开发设计阶段的文档库**（暂不含代码实现）。开发设计见 [docs/dev/](docs/dev/00-overview-and-milestones.md)。

## 当前阶段（截至 2026-07-25）

| 事项 | 状态 |
| --- | --- |
| 产品需求 PRD | ✅ v1.3，16 项业务参数确认 + 运行时验证反馈已折入（ISSUE-003） |
| 开源网关技术选型 | ⚠️ 已被取代：2026-07-25 转向**彻底自研**，移除 AxonHub/ccLoad（见 [11 转向决策](docs/dev/11-decision-full-selfbuilt.md)） |
| 未决问题清单 | ✅ 全部逐项确认（[OPEN-ISSUES](docs/OPEN-ISSUES.md)、[DECISIONS](docs/DECISIONS.md)） |
| 上游采集调研（ISSUE-002） | ✅ 三家族（NewAPI/Sub2API/闭源 ASXS）接口全部打通，4 站实测 + 源码级解析 |
| AxonHub 6 项假设（ISSUE-001） | ✅ 已完成（2026-07-23）；其结论（首字非可见内容、Responses 吞 reasoning）成为**转向自研的直接依据**，现为历史记录 |

## 文档地图

### 核心交付
- [产品需求 PRD v1.3](docs/PRD.md) —— 需求主文档（FR-001～119、AC-01～32、默认策略、参数确认）
- [决策确认记录 DECISIONS](docs/DECISIONS.md) —— 16 项参数 + 5 条新需求的确认结果
- [未决问题清单 OPEN-ISSUES](docs/OPEN-ISSUES.md) —— 五部分问题及处理状态

### 技术选型
- [技术选型结论 TECHNICAL-SELECTION](docs/tech-selection/TECHNICAL-SELECTION.md)
- [技术选型决策图 DECISION-MAP](docs/tech-selection/DECISION-MAP.md)
- [候选仓库固定基线](docs/tech-selection/research/00-repository-baseline.md)
- 逐仓库评估（历史，转向后不再作为执行面候选）：[AxonHub](docs/tech-selection/research/axonhub.md)、[Aether](docs/tech-selection/research/aether.md)、[OmniRoute](docs/tech-selection/research/omniroute.md)、[NewAPI](docs/tech-selection/research/newapi.md)、[Sub2API](docs/tech-selection/research/sub2api.md)、[Octopus 系列](docs/tech-selection/research/octopus.md)、[ccLoad](docs/tech-selection/research/ccload.md)、[zhfeng1/ai-gateway](docs/tech-selection/research/zhfeng1-ai-gateway.md)
- 横向分析：[能力覆盖矩阵](docs/tech-selection/research/capability-matrix.md)、[外部控制边界](docs/tech-selection/research/control-boundary.md)、[维护性判断](docs/tech-selection/research/maintenance.md)
- [选型复审（2026-07-23）](docs/tech-selection/research/2026-07-resurvey.md) —— 新筛 10+ 家（LiteLLM/Bifrost/Portkey/gpt-load 等），未发现更优，维持推荐

### 专项调研（issues）
- [ISSUE-001：AxonHub 6 项技术假设验证](docs/issues/ISSUE-001-tech-assumption-verification.md)（历史记录，转向依据）
- [ISSUE-002：上游采集调研（第一阶段）](docs/issues/ISSUE-002-upstream-data-collection.md)
- [ISSUE-002：四站实测探测结果](docs/issues/ISSUE-002-probe-results.md)
- [ISSUE-002：多平台采集适配器设计](docs/issues/ISSUE-002-collector-adapter-design.md)
- [订阅数据采集验证记录](docs/tech-selection/research/subscription-data-verification.md)
- [ISSUE-003：运行时/采集反馈的需求候选（待评审并入 PRD v1.3）](docs/issues/ISSUE-003-runtime-feedback-requirement-candidates.md)

### 开发设计（2026-07-23 启动，07-25 转向自研）
- [00 开发总览与里程碑](docs/dev/00-overview-and-milestones.md) —— 技术栈决策（Go/PG/Compose）、硬约束清单、M0～M4
- [01 架构设计](docs/dev/01-architecture.md) —— 组件拓扑、sla-core 模块、上游直连、请求时序
- [02 数据模型](docs/dev/02-data-model.md) —— PG 单库 schema：逐 Attempt 账本、订阅台账（双倍率）、价格版本、健康冷却、月分区保留
- [03 上游对接层](docs/dev/03-upstream-layer.md) —— **自研直连**：字节透传 + 旁路观察、Codex 兼容硬约束、取消与超时
- [04 采集器契约](docs/dev/04-collector-adapter.md) —— CollectorAdapter 接口 + 三家族字段映射 + 凭证生命周期状态机
- [05 调度与经营策略](docs/dev/05-scheduling-and-operations.md) —— selector 排序/RoutePlan + steward 测活/冷却/订阅倾斜/告警
- [06 部署与运维](docs/dev/06-deployment-and-operations.md) —— 单机 Compose、上游直连与选主、版本治理、备份保留、M0 部署清单
- [07 AxonHub 运行时实测](docs/dev/07-axonhub-runtime-probes.md) —— 历史记录；其 Responses round-trip 损耗实测是转向的直接依据
- [08 参考项目评估：zhfeng1/ai-gateway](docs/dev/08-ref-eval-zhfeng1-ai-gateway.md) —— 源码级评估：TTFT 品类通病佐证、网关开销测法、TPS 分母口径、不吸收清单
- [09 管理 API 与设置面](docs/dev/09-admin-api.md) —— `/admin/*` 配置 API、关键项二次确认、策略元数据（FR-115 操作面）
- [10 工程结构与构建](docs/dev/10-project-structure.md) —— Go 单仓布局、包边界、构建/CI 门禁、M0 交付物映射
- [11 架构转向决策](docs/dev/11-decision-full-selfbuilt.md) —— **移除外部网关、彻底自研**的决策与依据
- [12 可调试性设计](docs/dev/12-debuggability.md) —— 自研透传层排障：FR-112 约束下只存元数据
- [13 调研资产重审](docs/dev/13-research-reassessment.md) —— Codex 三条硬约束；三项目在同一处翻车 → 字节透传
- [14 验收矩阵](docs/dev/14-acceptance-matrix.md) —— 32 条 AC → 里程碑 → 可执行判定方法（PM 与开发的验收契约）
- [15 P0 内部分层](docs/dev/15-p0-tiering.md) —— 92 条 P0 分 A/B/C 三层；延期时的取舍预案

### 运行时验证
- [verify/ — mock 上游场景集](verify/README.md)（AxonHub 六假设部分转历史；mock 场景转为自研透传层测试夹具）

## 推荐架构（一句话）

**客户端（主力 Codex CLI）→ 自研 SLA 决策核心（承担价格/余额/会话/缓存/测活/订阅额度决策）→ 自研上游透传层 → 真实上游。** 一期**无外部网关**（[11 转向](docs/dev/11-decision-full-selfbuilt.md)）；Responses 走**字节级透传**保真，订阅台账、双倍率、TTFT 判定、Attempt 账本全由自研核心负责。

## 关键约束

- **使用场景**：个人 / 内部使用，不对外提供服务（决定许可证、合规与鉴权边界）。
- **规模**：峰值 100～1000 QPS。
- **对外协议**：OpenAI Chat Completions 与 Responses。
- **一期数据**：不存请求正文/上下文/头/体；账本元数据保留 ≥180 天。
- **策略配置化**：所有调度与经营策略以建议值为默认、设置页可改，关键项修改需二次确认。

## 编号约定

需求 `FR-xxx`、验收 `AC-xx`、专项 `ISSUE-xxx`。开发计划、验收用例与上线检查必须引用这些编号；需求变更须更新 PRD 版本、日期、原因与受影响编号。
