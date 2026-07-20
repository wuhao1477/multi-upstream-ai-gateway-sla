# Octopus 系列源码评估

| 项目 | `bestruirui/octopus` | `Hureru/octopus` |
| --- | --- | --- |
| 评估提交 | `97d6a04c9f3d4ff515b96877d8890e39f42df9f6` | `0e1a7ee062c8a24a9c8b95e7d1778f265182a304` |
| 分支 / 最近发布 | `dev` / `v0.9.28` | `dev` / `v0.8.40` |
| 许可证 | AGPL-3.0 | AGPL-3.0 |
| 结论 | 不进入短名单 | 功能较强，但不作为生产基础 |

## 1. 两个项目的真实关系

Hureru 是 GitHub 明确标记的 `bestruirui/octopus` fork，但已不是小范围补丁分支：

- 当前公共祖先为 `bf07971d8af5f827aebd5c3073420aaf705bf437`（2026-03-18）。
- 上游分支独有 9 个提交，Hureru 分支独有 286 个提交。
- 当前树比较涉及 424 个文件，约 `+95,349/-3,933`。
- 未发现自动同步上游流程；只确认到一次较早的人工合并记录。

因此不能把 Hureru 视为“上游 Octopus + 少量可维护改动”。采用 Hureru 等同于选择一个独立的大型旁支，后续需分别处理其功能演进和上游安全修复。

## 2. bestruirui/octopus

### 已验证能力

上游 Octopus 有多渠道、多个 Key、优先级、权重、首字超时、会话保持、流式响应及写出前顺序回退：

- [请求入口](https://github.com/bestruirui/octopus/blob/97d6a04c9f3d4ff515b96877d8890e39f42df9f6/internal/server/server.go#L57)
- [候选、重试和写出边界](https://github.com/bestruirui/octopus/blob/97d6a04c9f3d4ff515b96877d8890e39f42df9f6/internal/relay/relay.go#L85)
- [首字超时切换](https://github.com/bestruirui/octopus/blob/97d6a04c9f3d4ff515b96877d8890e39f42df9f6/internal/relay/relay.go#L324)
- [渠道和 Key 模型](https://github.com/bestruirui/octopus/blob/97d6a04c9f3d4ff515b96877d8890e39f42df9f6/internal/model/channel.go#L20)
- [分组、权重和会话保持](https://github.com/bestruirui/octopus/blob/97d6a04c9f3d4ff515b96877d8890e39f42df9f6/internal/model/group.go#L3)

管理 API 可以 L0 更新渠道、Key、模型组、权重、启停和全局设置。项目没有运行时插件、外部策略回调或通用出站 Webhook；账号、用户组、余额、故障域和 SLA 账目也没有正式模型。

### 判定

它适合多上游代理和基础故障转移，但要满足价格、共享余额、缓存作用域、真实业务测活、会话预算、故障域和 SLA 审计，需要修改多个核心状态与请求模块，侵入等级达到 L3。

## 3. Hureru/octopus

### 相对上游新增的有效能力

Hureru 针对聚合站场景增加了较有价值的数据模型和请求处理：

- 原生识别 NewAPI、OneAPI、Sub2API 等站点类型。
- Site 下建模 Account、Token、UserGroup、Model 和 ChannelBinding，并记录账号余额、已用额度和同步时间。
- 支持动态权重、同渠道重试、首字超时取消、缓存 Token、费用和每次尝试日志。
- 按 `channel:key:model` 熔断，并对同账号关联渠道做离群检测、自动禁用和恢复。

关键源码：

- [Site 类型和账号关系](https://github.com/Hureru/octopus/blob/0e1a7ee062c8a24a9c8b95e7d1778f265182a304/internal/model/site.go#L14)
- [余额与同步时间](https://github.com/Hureru/octopus/blob/0e1a7ee062c8a24a9c8b95e7d1778f265182a304/internal/model/site.go#L236)
- [首字超时取消](https://github.com/Hureru/octopus/blob/0e1a7ee062c8a24a9c8b95e7d1778f265182a304/internal/relay/first_token_timeout.go#L51)
- [同渠道重试边界](https://github.com/Hureru/octopus/blob/0e1a7ee062c8a24a9c8b95e7d1778f265182a304/internal/relay/retry.go#L9)
- [请求尝试和缓存指标](https://github.com/Hureru/octopus/blob/0e1a7ee062c8a24a9c8b95e7d1778f265182a304/internal/relay/metrics.go#L17)
- [同账号离群退役](https://github.com/Hureru/octopus/blob/0e1a7ee062c8a24a9c8b95e7d1778f265182a304/internal/task/site_outlier.go#L29)
- [熔断状态](https://github.com/Hureru/octopus/blob/0e1a7ee062c8a24a9c8b95e7d1778f265182a304/internal/relay/balancer/circuit.go#L14)

### 关键缺口

- 健康探针仍使用固定 `ping` 和 1 Token，不符合 FR-060 的真实业务测活要求：[Probe](https://github.com/Hureru/octopus/blob/0e1a7ee062c8a24a9c8b95e7d1778f265182a304/internal/grouphealth/probe.go#L120)。
- 没有通用出站 Webhook、运行时插件或可替换调度策略接口。
- 价格和余额同步未形成不可覆盖可信版本、保守余额、费用预留和扣费差异核对。
- 没有多层故障域、会话前缀 TTFT 预算、缓存切换损失、探索预算和三类账目。
- 首字前接管仍是串行切换，没有动态会话等待预算和重复费用账本。
- 默认记录完整请求与响应正文，与 FR-094 冲突：[Metrics Body](https://github.com/Hureru/octopus/blob/0e1a7ee062c8a24a9c8b95e7d1778f265182a304/internal/relay/metrics.go#L260)。

要补齐这些能力，需要同时修改 relay、Site/Account 状态、日志、账务、健康任务和管理面，不是单一 L2 扩展。

## 4. 许可证与维护判断

两者均采用 AGPL-3.0。AGPL 第 13 条要求修改后的网络服务向远程交互用户提供对应源码，见 [上游许可证](https://github.com/bestruirui/octopus/blob/97d6a04c9f3d4ff515b96877d8890e39f42df9f6/LICENSE#L540)。该义务与私有修改、网络服务部署方式直接相关，正式采用前必须由法务确认；本报告不构成法律意见。

上游最近 30 天没有提交；Hureru 最近 90 天较活跃，但主要由少量维护者推进，且与上游分歧已经很大。对“尽量向上游提交小 PR、降低长期分叉成本”的目标不利。

## 5. 不推荐原因

- `bestruirui/octopus` 缺失的核心领域模型过多，补齐会进入 L3。
- `Hureru/octopus` 更接近聚合站场景，但本身已是大规模长期分叉，仍需多模块核心修改，并引入 AGPL 和敏感日志风险。

**结论：两个 Octopus 均不作为最终基础。**
