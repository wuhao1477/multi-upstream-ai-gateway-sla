# 候选仓库固定基线

| 项目 | 内容 |
| --- | --- |
| 调查日期 | 2026-07-21 |
| 调查方式 | 固定提交源码、GitHub 元数据、许可证原文、CodeGraph 调用链 |
| 结论使用范围 | 本次技术选型；不代表生产验收 |

README 只用于识别项目定位。路由、流式处理、失败接管、外部控制和状态模型的结论均以固定提交源码为依据。

## 1. 固定版本

| 候选 | 固定提交 | 分支 / 最近发布 | 许可证 |
| --- | --- | --- | --- |
| `looplj/axonhub` | `ed6119a168483a205a85a2f38c7153f5cf1b61a6` | `unstable` / `v1.0.0-beta5` | `llm/` LGPL-3.0，其余 Apache-2.0 |
| `bestruirui/octopus` | `97d6a04c9f3d4ff515b96877d8890e39f42df9f6` | `dev` / `v0.9.28` | AGPL-3.0 |
| `Hureru/octopus` | `0e1a7ee062c8a24a9c8b95e7d1778f265182a304` | `dev` / `v0.8.40` | AGPL-3.0 |
| `fawney19/Aether` | `7756c0913f2e0089575d019f1ca90d9867f35c52` | `main` / `v0.7.11` | 自定义非商业许可证 |
| `diegosouzapw/OmniRoute` | `3ba3cd145eefafcb22efa16d6110daeb65c8df06` | `release/v3.8.49` / `v3.8.48` | MIT |
| `caidaoli/ccLoad` | `94231ef997120a85e2a00d8e20896596759ad03c` | `master` / `v3.5.0` | MIT |
| `zhfeng1/ai-gateway` | `3bf7431665669d5dc059e105e66f8806ecb13e3a` | `main` / `v0.1.13` | 未提供许可证 |

“最近发布”与评估提交不一定相同。所有源码链接均固定到上表提交，不引用持续变化的默认分支。

## 2. 维护状态快照

以下 GitHub 数字读取于 2026-07-21，只用于判断项目规模和近期活动，不作为功能评分。

| 候选 | Stars / Forks / Open Issues | 提交与近期活动 | 判断 |
| --- | --- | --- | --- |
| AxonHub | 4,744 / 594 / 99 | 约 1,400 commits；近 30 天 75 次；最近发布仍为 beta | 活跃，但升级变化快 |
| bestruirui/octopus | 2,293 / 345 / 70 | 296 commits；近 30 天无提交 | 上游近期活动偏低 |
| Hureru/octopus | 203 / 28 / 15 | 573 commits；近 90 天 137 次，主要由少量维护者推进 | 活跃旁支，维护集中 |
| Aether | 1,284 / 190 / 49 | 2,578 commits；近 30 天 188 次 | 高频演进 |
| OmniRoute | 21,232 / 2,909 / 214 | 约 5,400 commits；2026-02 创建 | 规模大、产品变化快 |
| ccLoad | 364 / 64 / 3 | 1,349 commits；最近提交 2026-07-20 | 活跃、维护者较少 |
| zhfeng1/ai-gateway | 3 / 0 / 0 | 25 commits；单一作者 | 早期观察工具 |

## 3. 分叉关系

`Hureru/octopus` 的 GitHub parent/source 均为 `bestruirui/octopus`，但两者已大幅分化：

- 公共祖先：`bf07971d8af5f827aebd5c3073420aaf705bf437`（2026-03-18）。
- 上游独有 9 个提交，Hureru 独有 286 个提交。
- 当前树差异涉及 424 个文件，约 `+95,349/-3,933`。
- 未发现自动同步上游的工作流。

Hureru 应按独立长期分支评估，不能按“小补丁 fork”估算维护成本。

## 4. 许可证门槛

| 候选 | 选型影响 |
| --- | --- |
| AxonHub | 主体 Apache-2.0；需单独审查 `llm/` LGPL-3.0，推荐不修改该目录 |
| 两个 Octopus | AGPL 网络服务源码义务与私有修改目标存在直接关系，采用前必须法务确认 |
| Aether | 商业或付费场景必须另获授权；未取得书面授权前排除 |
| OmniRoute / ccLoad | 标准 MIT，可接受 |
| zhfeng1/ai-gateway | 无明确授权，排除 |

本表是工程选型门槛说明，不构成法律意见。

## 5. 调查产物

- [AxonHub](./axonhub.md)
- [Octopus 系列](./octopus.md)
- [Aether](./aether.md)
- [OmniRoute](./omniroute.md)
- [ccLoad](./ccload.md)
- [zhfeng1/ai-gateway](./zhfeng1-ai-gateway.md)
- [能力覆盖矩阵](./capability-matrix.md)
- [外部控制边界](./control-boundary.md)
- [维护性比较](./maintenance.md)
