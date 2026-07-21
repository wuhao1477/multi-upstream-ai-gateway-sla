# 候选仓库固定基线

| 项目 | 内容 |
| --- | --- |
| 调查日期 | 2026-07-21 |
| 调查方式 | 固定提交源码、GitHub 元数据、CodeGraph 调用链 |
| 结论使用范围 | 本次技术选型；不代表生产验收 |

README 只用于识别项目定位。路由、流式处理、失败接管、外部控制和状态模型的结论均以固定提交源码为依据。

## 1. 固定版本

| 候选 | 固定提交 | 分支 / 最近发布 | 许可证 |
| --- | --- | --- | --- |
| `looplj/axonhub` | `ed6119a168483a205a85a2f38c7153f5cf1b61a6` | `unstable` / `v1.0.0-beta5` | `llm/` LGPL-3.0，其余 Apache-2.0 |
| `bestruirui/octopus` | `b7b053e7fd81911e2062359e93f9dcbd58114bb0` | `master`（非默认） / `v0.9.28` | AGPL-3.0 |
| `Hureru/octopus` | `0e1a7ee062c8a24a9c8b95e7d1778f265182a304` | `dev` / `v0.8.40` | AGPL-3.0 |
| `fawney19/Aether` | `7756c0913f2e0089575d019f1ca90d9867f35c52` | `main` / `v0.7.11` | 自定义非商业许可证 |
| `diegosouzapw/OmniRoute` | `698b6eb00d0a3f589c9ae8828edc7b23002270e0` | `main`（非默认） / 最近可达 `v3.8.48` | MIT |
| `caidaoli/ccLoad` | `665fec14f5eed2c00fa0aaf54c9a4643c04d0dab` | `master` / `v3.6.1` | MIT |
| `zhfeng1/ai-gateway` | `3bf7431665669d5dc059e105e66f8806ecb13e3a` | `main` / `v0.1.13` | 未提供许可证 |
| `QuantumNous/new-api` | `e0d5156115881780328d31fe9bce7fe25aa9c6c7` | `main` / `v1.0.0-rc.21` | AGPL-3.0 |
| `Wei-Shaw/sub2api` | `b8b72e1b18310c908668e79112e43c1e7c682696` | `main` / `v0.1.162` | LGPL-3.0-or-later |

“最近发布”与评估提交不一定相同。所有源码链接均固定到上表提交，不引用持续变化的默认分支。OmniRoute 的默认分支是 `release/v3.8.49`，bestruirui/octopus 的默认分支是 `dev`；本轮按要求分别研究其 `main` 和 `master`，两个仓库均不存在另一名称的分支。ccLoad 只有 `master`，不存在 `main`。

## 2. 维护状态快照

以下 GitHub 数字读取于 2026-07-21，只用于判断项目规模和近期活动，不作为功能评分。

| 候选 | Stars / Forks / Open Issues | 提交与近期活动 | 判断 |
| --- | --- | --- | --- |
| AxonHub | 4,744 / 594 / 99 | 约 1,400 commits；近 30 天 75 次；最近发布仍为 beta | 活跃，但升级变化快 |
| bestruirui/octopus | 2,296 / 345 / 57 | `master` 294 commits；近 30 天无提交 | 指定分支近期无维护，且落后默认 `dev` 两个提交 |
| Hureru/octopus | 203 / 28 / 15 | 573 commits；近 90 天 137 次，主要由少量维护者推进 | 活跃旁支，维护集中 |
| Aether | 1,284 / 190 / 49 | 2,578 commits；近 30 天 188 次 | 高频演进 |
| OmniRoute | 22,301 / 3,009 / 151 | `main` 4,562 commits；近 30 天 48 次 | 规模大；`main` 与默认发布分支持续分叉 |
| ccLoad | 364 / 64 / 2 | `master` 1,351 commits；近 30 天 159 次 | 活跃、维护者较少 |
| zhfeng1/ai-gateway | 3 / 0 / 0 | 25 commits；单一作者 | 早期观察工具 |
| NewAPI | 42,854 / 10,001 / 1,077 | 最近提交 2026-07-20；最近发布为 RC | 生态大、高频演进 |
| Sub2API | 33,175 / 6,823 / 2,184 | 最近提交与发布均为 2026-07-21 前后 | 生态大、高频演进、功能面较重 |

## 3. 分叉关系

`Hureru/octopus` 的 GitHub parent/source 均为 `bestruirui/octopus`，但两者已大幅分化。以下差异按双方此前固定的 `dev` 提交计算，不用于替代本轮 `master` 功能评估：

- 公共祖先：`bf07971d8af5f827aebd5c3073420aaf705bf437`（2026-03-18）。
- 上游独有 9 个提交，Hureru 独有 286 个提交。
- 当前树差异涉及 424 个文件，约 `+95,349/-3,933`。
- 未发现自动同步上游的工作流。

Hureru 应按独立长期分支评估，不能按“小补丁 fork”估算维护成本。

## 4. 许可信息

按本轮要求，许可证不参与候选淘汰、排序和最终推荐。下表仅保留版本基线信息，不形成采用结论。

| 候选 | 基线信息 |
| --- | --- |
| AxonHub | 主体 Apache-2.0；`llm/` 为 LGPL-3.0 |
| 两个 Octopus / NewAPI | AGPL-3.0 |
| Aether | 自定义非商业许可证 |
| OmniRoute / ccLoad | MIT |
| Sub2API | LGPL-3.0-or-later |
| zhfeng1/ai-gateway | 未提供许可证 |

本表仅记录固定提交与仓库元数据；许可证不参与本轮技术选型结论。

## 5. 调查产物

- [AxonHub](./axonhub.md)
- [Octopus 系列](./octopus.md)
- [Aether](./aether.md)
- [OmniRoute](./omniroute.md)
- [ccLoad](./ccload.md)
- [zhfeng1/ai-gateway](./zhfeng1-ai-gateway.md)
- [NewAPI](./newapi.md)
- [Sub2API](./sub2api.md)
- [能力覆盖矩阵](./capability-matrix.md)
- [外部控制边界](./control-boundary.md)
- [维护性比较](./maintenance.md)
