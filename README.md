# 多上游 AI 网关 SLA 保障项目

本仓库用于记录多上游 AI 网关的产品需求与技术选型依据。

项目计划汇总约 20 个 AI 上游渠道，依据价格、余额、性能、稳定性、容量、缓存表现和模型能力等信息动态分配请求，在满足服务质量目标的前提下降低实际服务成本。

## 当前阶段

- 已完成需求梳理、PRD 和开源网关技术选型。
- 唯一推荐为 AxonHub 执行数据面 + 外部 SLA 决策网关。
- 不包含开发计划、技术栈选择和代码实现。

## 产品文档

- [多上游 AI 网关 SLA 保障系统 PRD](docs/PRD.md)
- [技术选型结论](docs/tech-selection/TECHNICAL-SELECTION.md)
- [技术选型决策图](docs/tech-selection/DECISION-MAP.md)
