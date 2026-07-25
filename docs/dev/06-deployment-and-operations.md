# 06 部署与运维（单机 Docker Compose · v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | 草案，待评审（M0 前定稿） |
| 日期 | 2026-07-23 |
| 形态 | **单机 Docker Compose**：Caddy LB + 2× sla-core（Go）+ PostgreSQL + AxonHub `v1.0.0-beta5`（stock，L0）+ collector |
| 约束 | 个人/内部使用；任一 sla-core 实例宕机不影响服务（FR-110）；AxonHub 锁定 beta5、升级须过 verify/ 门禁（硬约束 11） |
| 输入 | [01 架构](./01-architecture.md)、[00 硬约束](./00-overview-and-milestones.md)、[03 上游对接层](./03-upstream-layer.md)、[verify/](../../verify/README.md)、[ISSUE-001 beta5 适配表](../issues/ISSUE-001-tech-assumption-verification.md) |

---

## 1. 拓扑与组件

```
                 ┌───────────────── 单机 Docker Compose ─────────────────┐
客户端 → :443 Caddy(LB/TLS) → sla-core-a :8080 ┐
                            └→ sla-core-b :8080 ┘─同步→ axonhub :8090 → 上游
                                    │                        (stock beta5, retry 置零, 默认单实例)
                                    ├→ postgres :5432 (账本/台账/价格/健康，多实例共享)
                                    └→ collector (子命令/独立容器) → 上游站点管理面
```

| 服务 | 镜像/构建 | 端口 | 依赖 | 备注 |
| --- | --- | --- | --- | --- |
| `caddy` | caddy:2 | 443/80 | core-a/b | TLS + 轮询 LB + 健康探测摘除故障实例 |
| `sla-core-a/b` | 本仓库构建（Go） | 8080 | postgres、axonhub | 无本地状态；`/healthz` 就绪探针。**核心必须 ≥2 实例**（FR-110） |
| `postgres` | postgres:16 | 5432 | — | 单库；账本 + `axonhub` 分 schema |
| `axonhub` | `looplj/axonhub:v1.0.0-beta5`（**锁定 tag**） | 8090 | postgres | stock 不改源码；**默认单实例**（进程自愈兜底）；启动由 core bootstrap 下发 retryPolicy 置零。**双实例为可选**（见 §2.3） |
| `collector` | 同 core 二进制 `collector` 子命令 | — | postgres、上游站点 | 异步旁路；限速；凭证明文（FR-113） |

> **为何 core ≥2 而 AxonHub 默认 1**：FR-110 **只约束自研决策核心**（≥2 实例、任一宕机不中断），**未要求 AxonHub 多实例**。核心是策略/账本/合规的必经之路，必须高可用；AxonHub 是受控执行面，单实例挂了由进程自愈 + 核心返回明确不可用（禁旁路）兜底，个人/内部场景足够。是否上 AxonHub 双实例**由部署者按需决定**（§2.3），默认不上以免引入非必要复杂度。

---

## 2. AxonHub 集成部署（beta5，L0）

### 2.1 存储：PG 共库分 schema（01 开放点 4 / 02 开放点 3 的建议落地）

- AxonHub 用 PG 而非默认 SQLite：`AXONHUB_DB_DIALECT=postgres`、DSN 指向同一 `postgres` 服务的 **`axonhub` schema**（与自研账本 `public` 隔离）。
- 收益：备份/运维统一；`Reconcile` 拉取的 `requests/executions/usageLogs` 可与自研账本**本地 JOIN 对账**，免跨库。
- ✅ **已实测**（[07 §1](./07-axonhub-runtime-probes.md)）：DSN 格式 `postgres://user:pass@host:5432/db?sslmode=disable`（dialect=`postgres`）；`search_path=<schema>`（需预先 `CREATE SCHEMA`，ent 不自建）实现与自研账本共库分 schema，25 张表落指定 schema。

### 2.2 启动后置初始化（由 sla-core 幂等下发，非人工）

core 启动时对 axonhub 执行一次幂等 bootstrap（经 `/admin/graphql`，字段名按 beta5 适配表）：

1. `system/initialize` + `auth/signin`（均带 `/admin` 前缀）。
2. **全字段 `updateRetryPolicy{enabled:false, streamFirstEventTimeoutSeconds:0, maxChannelRetries:0, maxSingleChannelRetries:0, ...}`** —— 整体替换的坑（假设 2 补验），部署脚本单测校验全字段下发（[03 §5.1](./03-upstream-layer.md)）。
3. 按 PG 中登记的 binding 逐个 `ProvisionBinding`（建渠道 → `updateChannelStatus(enabled)` → 建 Key → 锁单渠道 profile → `saveChannelModelPrices`），全部经 GatewayAdapter，**禁止人工建 Key**（[03 §4.1/4.6](./03-upstream-layer.md)）。

### 2.3 AxonHub 实例数：默认单实例，双实例可选（01 开放点 1）

**决策（2026-07-23）：默认单实例；双实例作为部署者可选项，不默认启用、一期不建配套复杂度。**

理由（务实）：

- FR-110 只约束**自研核心**多实例；AxonHub 是受控执行面，非合规/账本必经点。
- 个人/内部场景绝大多数时间单实例足够；单实例挂了由 compose `restart: unless-stopped` 进程自愈 + 核心对 axonhub 不可用**快速失败、返回明确不可用、禁旁路直连上游**（FR-110/AC-27）兜底。
- 双实例的收益（消除数据面单点）在此场景多数用不上，却要背上并发迁移/选主/滚动串行的复杂度——**性价比不划算，需要时再开发**。

**单实例下的 bootstrap（简单）**：2 个 sla-core 都会连这一个 axonhub，为避免双 core 重复初始化/建绑定，bootstrap 仍走一把 **PG 咨询锁** `pg_try_advisory_lock(<常量>)`：取到锁的 core 执行 `system/initialize` + 全字段 retryPolicy 置零 + `ProvisionBinding`，其余 core 跳过、轮询就绪。**AxonHub 自身的 schema 迁移由它单实例独占完成，无并发冲突**（07 的 panic 只在双实例并发冷启动才出现，单实例天然规避）。

**双实例（可选，已验证可行）**：若部署者确需消除数据面单点，[07 §2](./07-axonhub-runtime-probes.md) 已实测**稳态双活可行**（状态实时共享、任一实例可读写与路由）。启用时须满足**一条硬约束**：schema 迁移期无并发锁——**初始化/升级迁移只允许一个实例执行**（用上面同一把咨询锁串行），否则并发迁移会 panic。这条作为"双实例部署 runbook"留档，需要时启用，不进默认路径。

---

## 3. 升级门禁（硬约束 11：AxonHub 更新快，锁版本 + 准入检查）

AxonHub 近 30 天 ~75 次提交、仍无稳定 1.0，"今天验证通过的行为下个版本可能变"（选型风险 2）。三条纪律固化为流程：

| 纪律 | 落地 |
| --- | --- |
| 生产锁定具体 tag | compose 用 `looplj/axonhub:v1.0.0-beta5`，**不用 `latest`/`unstable`** |
| 升级前跑准入检查 | 换 tag 前先在 verify/ 跑 [ISSUE-001 六假设 harness](../../verify/README.md)：单渠道隔离/取消对账/首字/账本/配额/隐藏重试全绿 + [beta5 schema 适配表](../issues/ISSUE-001-tech-assumption-verification.md) 逐项核对（GraphQL 字段名漂移是首要风险） |
| 固定升级窗口、可回滚 | 建议每季度一次。**单实例（默认）升级极简**：停 axonhub → 换 tag → 起（它单独迁移自己的库，零并发冲突）→ 跑 harness → 全绿才恢复流量；失败 `docker compose` 回滚旧 tag。升级窗口内有短暂 axonhub 不可用（核心此间返回明确不可用，不旁路），个人/内部可接受。**双实例（可选）**才需"先单实例迁移完再拉起其余"的串行滚动（[07 §2](./07-axonhub-runtime-probes.md)：并发迁移崩实例） |

> **分离带来的升级隔离**：AxonHub 是 stock、L0、躲在 [GatewayAdapter](./03-upstream-layer.md) 接口后、版本锁定。升级风险被三层关住——① 只可能在 GatewayAdapter 边界出问题，不渗进自研核心；② verify/ harness 是每次升级的**准入门**（不全绿不准升）；③ 默认单实例让升级本身就是"停-换-验-起"，无滚动复杂度。GraphQL 报字段错时按 [beta5 适配表](../issues/ISSUE-001-tech-assumption-verification.md) 修正 `Reconcile` 查询（[03 §4.4](./03-upstream-layer.md)）。

---

## 4. 配置与密钥

| 类别 | 方式 | 依据 |
| --- | --- | --- |
| 策略参数 | 存 PG `config_params`，非环境变量；关键项二次确认（FR-115） | [02 §2](./02-data-model.md) |
| 基础设施配置 | 环境变量/`.env`（DSN、端口、AxonHub 地址） | — |
| 上游 Key / 采集凭证 | **一期明文**存 PG（`upstream_keys.secret`、`collector_credentials.*`）；日志与管理界面脱敏不显示完整 Key（FR-094/113） | FR-113 |
| TLS | Caddy 自动证书（内部可用自签） | — |

> 一期明文是已确认决策（DECISIONS 遗漏 4）；对外提供服务前须重评加密与轮换（FR-113 备注）。

---

## 5. 备份、保留与恢复

| 对象 | 策略 |
| --- | --- |
| PG 全库 | 每日 `pg_dump`（含 `public` + `axonhub` schema）；保留 ≥30 天备份 |
| 账本分区 | 月 RANGE 分区，保留 ≥180 天（7 分区），到期 `DETACH`+`DROP`（[02 §9](./02-data-model.md)，FR-112） |
| 分区滚动 | 后台定时任务提前建下月分区 + DROP 超期分区（一期自研任务，非 pg_partman，02 开放点 1） |
| 恢复演练 | M4 前做一次 dump→restore 演练，确认账本可复算（FR-013 历史按原价格版本复算） |
| 不可存守卫 | CI schema 断言：账本表禁出现 `body/messages/prompt/headers` 列（FR-112 硬约束 3，[02 §9.2](./02-data-model.md)） |

---

## 6. 可观测性与健康

| 面 | 做法 |
| --- | --- |
| 就绪/存活 | sla-core `/healthz`（PG 可达 + axonhub 可达）；Caddy 据此摘除实例 |
| 决策延迟 | 自监控 P99 决策开销 ≤50ms（FR-110）。**测法**：发往上游前多打一个时间戳，`决策/网关开销 = 总延迟 − 上游耗时`（对应 [02](./02-data-model.md) `attempts.full_latency_ms − upstream_latency_ms`）；超标告警 |
| 账本对账滞后 | 监控 `attempts.reconciled=false` 积压（[02 idx_attempts_unrecon](./02-data-model.md)） |
| 采集健康 | 凭证状态（`collector_credentials.status`）、快照陈旧率（`is_stale`）；凭证失效告警 P2 |
| 业务告警 | P1/P2/P3 经 `alert_events` 出（[05 §5.2](./05-scheduling-and-operations.md)）；P1 不得延迟（FR-103） |

---

## 7. M0 部署清单（退出标准）

- [ ] `docker compose up` 一键起全栈；`/healthz` 全绿。
- [ ] AxonHub beta5 用 PG 共库；bootstrap 幂等下发 retryPolicy 置零（单测校验全字段）。
- [ ] 停掉 sla-core-a，服务经 core-b 不中断（FR-110 验证）；停掉唯一 axonhub，核心返回明确不可用、不旁路（AC-27）。
- [ ] verify/ harness 对当前 beta5 跑通（升级门禁基线）；**M0 起把 07 的 PG 共库 + Responses 渠道两项固化进 harness**（默认单实例，故不含"双实例并发迁移"项——那项仅在选择双实例部署时按 07 §2 runbook 验）。
- [ ] CI：Go 构建 + `config_params`/别名策略加载 + 不可存列 schema 断言。
- [ ] `pg_dump`/restore 演练脚本就位。

---

## 8. 开放点（评审需拍板）

| # | 开放点 | 结论 |
| --- | --- | --- |
| 1 | AxonHub 实例数（单 vs 多） | ✅ **已定（2026-07-23）：默认单实例**（FR-110 只约束核心，AxonHub 单点由进程自愈+禁旁路兜底）；**双实例为部署者可选**，[07 §2](./07-axonhub-runtime-probes.md) 已验证可行但需迁移串行 runbook，需要时再启用，不进默认路径 |
| 2 | LB 选型 | ✅ **定** Caddy（自动 TLS、配置简单、单机足够） |
| 3 | collector 打包 | ✅ **定** 一期 core 二进制子命令（部署简单）；量级上来再拆独立容器/独立扩缩 |
| 4 | 备份加密（含明文 Key 的 dump） | ✅ **定** 一期 dump 落本机加密卷；对外提供服务前随 FR-113 一起升级 |

---

_本篇为 M0 部署基线。生产永远锁定 AxonHub 具体 tag；任何 AxonHub 升级先过 verify/ 准入门（硬约束 11）。_
