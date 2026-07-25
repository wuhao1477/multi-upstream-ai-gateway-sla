# 11 架构转向决策：移除外部网关，彻底自研（2026-07-25）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ 决策已定（对话式评审确认）；**文档返工待执行**，本篇先立档 |
| 日期 | 2026-07-25 |
| 决策 | **一期移除 AxonHub 与 ccLoad，上游对接由自研核心直连实现**；自写为主，遇到难点读 AxonHub/ccLoad 源码当参考 |
| 影响 | 推翻[选型结论](../tech-selection/TECHNICAL-SELECTION.md)的"AxonHub 唯一执行数据面 + L0"前提；docs/dev 00~10 中约 **257 处**提及需返工 |
| 前置结论 | 主力客户端 = Codex CLI（OpenAI Responses）；一期协议 = OpenAI CC + Responses（FR-111） |

---

## 1. 为什么转向（决策依据，非事后合理化）

### 1.1 我们本来就关掉了 AxonHub 的大半能力

| AxonHub 能力 | 我们的处置 | 出处 |
| --- | --- | --- |
| 重试 / 故障转移 | **全字段置零**（否则污染 Attempt 账本） | [03 §4.1](./03-upstream-layer.md)、假设 1/2 |
| 负载均衡 | 不用，自研 `selector` 决策 | [05 §1](./05-scheduling-and-operations.md) |
| 配额执行 | 不用（其默认关闭且 `unknown` 不排除），FR-118 自研补 | 假设 5、[07 §2](./07-axonhub-runtime-probes.md) |
| 首字指标 | **不采信**，自算内容感知 TTFT | AC-31、假设 3 |
| 首事件超时 | 不用，`executor` 自管每跳期限 | [05 §1.3](./05-scheduling-and-operations.md) |
| 逐次执行账本 | **与自研 `attempts` 账本重复**，还要额外做对账 | [02 §4](./02-data-model.md)、[03 §4.4](./03-upstream-layer.md) |

**实际仍在用的只剩"一把 Key 打一个渠道"**——这件事自研起来是平凡的。

### 1.2 两个实测发现成为决定性证据

- **Responses round-trip 损耗严重**（[07 §3bis](./07-axonhub-runtime-probes.md) 真实上游实测）：顶层字段 **35 → 7**，丢 `prompt_cache_key`/`previous_response_id`/`reasoning` 等 28 项；**`output` 的 reasoning item 被整条吞掉**、`reasoning_tokens` 归零。**主力 Codex 恰好依赖 reasoning。**
- **schema 漂移是持续开销**：beta5 GraphQL 字段名与评估提交已有差异，逼我们维护[适配表](../issues/ISSUE-001-tech-assumption-verification.md)与 verify/ 升级门禁；AxonHub 仍无稳定 1.0、近 30 天 ~75 次提交。

### 1.3 AxonHub 最有价值的两件事，我们一期用不上

| AxonHub 强项 | 一期是否需要 | 依据 |
| --- | --- | --- |
| 多协议翻译（CC↔Anthropic↔Gemini↔Responses） | ❌ 不需要 | 一期入站仅 OpenAI CC + Responses（FR-111）；上游全说 OpenAI 协议 |
| 订阅 OAuth 渠道（claudecode/codex 账号） | ❌ 不需要 | 约 20 个上游全是 **NewAPI/Sub2API/ASXS 中转站，用 `sk-` key**（[ISSUE-002](../issues/ISSUE-002-collector-adapter-design.md) 四站实测 + 2026-07-25 真实上游验证） |

→ **一期场景下上游对接本质是"OpenAI 协议纯透传"**，自研反而更简单、更可控。

### 1.4 转向后直接消失的复杂度

- 双账本与对账器（自研 `attempts` ↔ AxonHub `executions`/`usageLogs`）
- beta5 GraphQL 适配表维护 + verify/ 升级准入门禁
- Key-per-Channel 开通/回收编排、`updateRetryPolicy` 整体替换陷阱
- Responses round-trip 字段损耗（转向后不存在该问题）
- AxonHub 单点风险与其部署/迁移/选主配套

---

## 2. 实现策略

**自写为主，读源码当参考。**

- 用 Go 标准库 `net/http` 自写上游透传与 SSE 处理；遇到难点（流式切分、错误映射、usage 提取、取消传播）**读 AxonHub / ccLoad 源码看它们怎么处理**，取其思路不引其依赖。
- **不引入其 Go 包**：AxonHub `llm/` 为 LGPL-3.0，且会带入其抽象与版本跟随负担。
- **不 fork 二开**：冗余与无关设计多，且后续合上游变更痛苦。
- **许可证**：AxonHub 主体 Apache-2.0、`llm/` LGPL-3.0；个人/内部使用不分发，copyleft 义务不触发（[OPEN-ISSUES 第五部分](../OPEN-ISSUES.md)已确认）。仅借鉴思路、不复制代码时风险更低。

---

## 3. 依然有效的既有成果（**不是全废**）

转向只推翻"由谁执行上游调用"，以下结论与设计**继续成立**：

| 成果 | 为何仍有效 |
| --- | --- |
| **PRD v1.3 全部 FR/AC** | 需求与执行面选型无关 |
| **02 数据模型**（账本/台账/价格/健康/告警） | 自研账本本就是权威源；转向后**反而更纯粹**（去掉对账列即可） |
| **04 采集器契约**（三家族 NewAPI/Sub2API/ASXS） | 采集走上游站点管理面，与执行网关无关，**零影响** |
| **05 调度与经营策略** | selector/steward 本就在自研核心，**零影响** |
| **09 管理 API** | 自研控制面，**零影响** |
| **02 §4.5 会话标识提取链** | 转向后**更完整**：不再受 AxonHub round-trip 丢字段影响，body 侧序 3/5 恢复可用 |
| **ISSUE-001 六假设的运行时方法论** | 结论转为"为何不用 AxonHub"的证据；harness 的 mock 上游可复用于自研透传层测试 |
| **ISSUE-002 采集调研** | 完全有效 |
| **07 真实上游实测** | 其 Responses 基线（35 字段、reasoning、缓存字段）正是自研透传层要保真的目标 |

---

## 4. 需要返工的范围（约 257 处提及，15 份文档）

| 文档 | 提及数 | 处置 |
| --- | --- | --- |
| [03 上游对接层](./03-upstream-layer.md) | 44 | **重写**为《上游对接层》：自研透传/SSE/取消/usage 提取；保留能力声明与降级思想 |
| [ISSUE-001](../issues/ISSUE-001-tech-assumption-verification.md) | 41 | **转为历史记录**：标注结论已用于支撑"不采用 AxonHub"的决策 |
| [02 数据模型](./02-data-model.md) | 33 | 去掉 `axonhub_*` 对账列与相关索引；账本简化为单一真相源 |
| [06 部署运维](./06-deployment-and-operations.md) | 30 | 去 AxonHub 容器/PG 共库/升级门禁；拓扑简化为 Caddy + core×2 + PG + collector |
| [07 运行时实测](./07-axonhub-runtime-probes.md) | 25 | **转为历史记录**（其 Responses 基线仍作自研保真目标） |
| [TECHNICAL-SELECTION](../tech-selection/TECHNICAL-SELECTION.md) | 19 | 追加转向说明：选型结论在一期被"彻底自研"取代，理由见本篇 |
| [01 架构](./01-architecture.md) | 16 | 拓扑与请求路径去掉 AxonHub 跳 |
| [10 工程结构](./10-project-structure.md) | 15 | `internal/gateway/` 改为 `internal/upstream/`（自研透传实现） |
| [00 总览](./00-overview-and-milestones.md) | 10 | 硬约束表与里程碑去 AxonHub 相关项 |
| [README](../../README.md) | 9 | 架构一句话与状态表更新 |
| [PRD](../PRD.md) | 6 | 仅提及处更新（FR/AC 不变） |
| 04 / 05 / 08 / 09 | 各 1~5 | 零星引用修正 |
| `verify/` | — | 六假设 harness 转为历史；其 **mock 上游脚本复用**于自研透传层测试 |

---

## 5. 里程碑影响

- **M0**：去掉 AxonHub 容器与 bootstrap 编排；compose 简化。**选主锁仍保留**（避免双 core 重复初始化）。
- **M1**：由"受控执行 + 对账"变为"**自研上游透传 + 单一账本**"；原计划的 protocol 直连**成为主路径而非补丁**。
- **M2~M4**：不受影响（流式 SLA、采集订阅、经营验收本就在自研核心）。

---

## 6. 返工方式（已定）

| # | 事项 | 决策 |
| --- | --- | --- |
| 1 | 返工节奏 | ✅ **一次性全量返工**——15 份文档一并改完再开工，保证文档集始终自洽，不出现"边写代码边对着过时设计"的情况 |
| 2 | 历史文档处置 | ✅ **原地加标注**，不移动、不删除——ISSUE-001/07 是"为何不用 AxonHub"的唯一证据，删了就无法回答"当初为什么这么定"。头部加一行"结论已用于支撑 [11](./11-decision-full-selfbuilt.md) 的转向决策，不再指导实现"，链接不断、决策脉络可追溯 |

---

## 7. 评审记录（2026-07-25 对话式确认）

| 议题 | 决策 |
| --- | --- |
| 里程碑节奏 | 串行 M0→M4 不变 |
| M0 验收环境 | 尽量用真实上游 |
| Responses reasoning 丢失 | M1 就做 protocol 直连 → 进而升级为**彻底自研** |
| 外部网关去留 | **全移除**（AxonHub + ccLoad） |
| 实现策略 | **自写为主，读其源码当参考**；不引其包、不 fork |
| 直连渠道凭证管理 | 复用自研 PG 登记（`channels`/`upstream_keys`，一期明文 FR-113） |
| 返工节奏 | 一次性全量 |
| 历史文档 | 原地加标注 |

---

_本篇立档决策与影响面。返工按 §6 执行：一次性全量改完 15 份文档，历史文档原地标注。_
