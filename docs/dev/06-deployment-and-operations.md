# 06 部署与运维（单机 Docker Compose · v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | 草案，待评审（M0 前定稿） |
| 日期 | 2026-07-23 |
| 形态 | **单机 Docker Compose**：Caddy LB + 2× sla-core（Go）+ PostgreSQL + AxonHub `v1.0.0-beta5`（stock，L0）+ collector |
| 约束 | 个人/内部使用；任一 sla-core 实例宕机不影响服务（FR-110）；AxonHub 锁定 beta5、升级须过 verify/ 门禁（硬约束 11） |
| 输入 | [01 架构](./01-architecture.md)、[00 硬约束](./00-overview-and-milestones.md)、[03 GatewayAdapter](./03-gateway-adapter.md)、[verify/](../../verify/README.md)、[ISSUE-001 beta5 适配表](../issues/ISSUE-001-tech-assumption-verification.md) |

---

## 1. 拓扑与组件

```
                 ┌───────────────── 单机 Docker Compose ─────────────────┐
客户端 → :443 Caddy(LB/TLS) → sla-core-a :8080 ┐
                            └→ sla-core-b :8080 ┘─同步→ axonhub :8090 → 上游
                                    │                        (stock beta5, retry 置零)
                                    ├→ postgres :5432 (账本/台账/价格/健康，多实例共享)
                                    └→ collector (子命令/独立容器) → 上游站点管理面
```

| 服务 | 镜像/构建 | 端口 | 依赖 | 备注 |
| --- | --- | --- | --- | --- |
| `caddy` | caddy:2 | 443/80 | core-a/b | TLS + 轮询 LB + 健康探测摘除故障实例 |
| `sla-core-a/b` | 本仓库构建（Go） | 8080 | postgres、axonhub | 无本地状态；`/healthz` 就绪探针 |
| `postgres` | postgres:16 | 5432 | — | 单库；账本 + `axonhub` 分 schema（开放点） |
| `axonhub` | `looplj/axonhub:v1.0.0-beta5`（**锁定 tag**） | 8090 | postgres | stock 不改源码；启动即由 core 下发 retryPolicy 置零 |
| `collector` | 同 core 二进制 `collector` 子命令 | — | postgres、上游站点 | 异步旁路；限速；凭证明文（FR-113） |

> **为何 2 个 core**：FR-110 要求任一实例宕机不影响服务。Caddy 对 `/healthz` 失败的实例自动摘除；两实例无差别（状态全在 PG，账本主键 UUIDv7 无序列争用，[02 §9.3](./02-data-model.md)）。

---

## 2. AxonHub 集成部署（beta5，L0）

### 2.1 存储：PG 共库分 schema（01 开放点 4 / 02 开放点 3 的建议落地）

- AxonHub 用 PG 而非默认 SQLite：`AXONHUB_DB_DIALECT=postgres`、DSN 指向同一 `postgres` 服务的 **`axonhub` schema**（与自研账本 `public` 隔离）。
- 收益：备份/运维统一；`Reconcile` 拉取的 `requests/executions/usageLogs` 可与自研账本**本地 JOIN 对账**，免跨库。
- **M0 验证项**：beta5 的 PG DSN 连通性与 schema 隔离（[verify/](../../verify/README.md) 目前用 SQLite，M0 补一轮 PG 形态）。

### 2.2 启动后置初始化（由 sla-core 幂等下发，非人工）

core 启动时对 axonhub 执行一次幂等 bootstrap（经 `/admin/graphql`，字段名按 beta5 适配表）：

1. `system/initialize` + `auth/signin`（均带 `/admin` 前缀）。
2. **全字段 `updateRetryPolicy{enabled:false, streamFirstEventTimeoutSeconds:0, maxChannelRetries:0, maxSingleChannelRetries:0, ...}`** —— 整体替换的坑（假设 2 补验），部署脚本单测校验全字段下发（[03 §5.1](./03-gateway-adapter.md)）。
3. 按 PG 中登记的 binding 逐个 `ProvisionBinding`（建渠道 → `updateChannelStatus(enabled)` → 建 Key → 锁单渠道 profile → `saveChannelModelPrices`），全部经 GatewayAdapter，**禁止人工建 Key**（[03 §4.1/4.6](./03-gateway-adapter.md)）。

### 2.3 单点风险与处置（01 开放点 1）

- **AxonHub 单实例是数据面单点**（FR-110 只约束自研核心）。一期接受：compose `restart: unless-stopped` 进程级自愈；core 侧对 axonhub 不可用**快速失败并返回明确不可用，禁旁路直连上游**（FR-110/AC-27）。
- 二期评估 AxonHub 双实例共 PG（beta5 是否支持多实例共库待验，列观察项）。

---

## 3. 升级门禁（硬约束 11：AxonHub 更新快，锁版本 + 准入检查）

AxonHub 近 30 天 ~75 次提交、仍无稳定 1.0，"今天验证通过的行为下个版本可能变"（选型风险 2）。三条纪律固化为流程：

| 纪律 | 落地 |
| --- | --- |
| 生产锁定具体 tag | compose 用 `looplj/axonhub:v1.0.0-beta5`，**不用 `latest`/`unstable`** |
| 升级前跑准入检查 | 换 tag 前先在 verify/ 跑 [ISSUE-001 六假设 harness](../../verify/README.md)：单渠道隔离/取消对账/首字/账本/配额/隐藏重试全绿 + [beta5 schema 适配表](../issues/ISSUE-001-tech-assumption-verification.md) 逐项核对（GraphQL 字段名漂移是首要风险） |
| 固定升级窗口、可回滚 | 建议每季度一次；升级 = 改 tag + 跑 harness + 灰度一个 core 实例；失败 `docker compose` 回滚旧 tag |

> harness 已入库（`verify/`），是每次升级的**准入门**而非一次性验证；GraphQL 报字段错时按适配表修正 `Reconcile` 查询（[03 §4.4](./03-gateway-adapter.md)）。

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
| 决策延迟 | 自监控 P99 决策开销 ≤50ms（FR-110）；超标告警（内部指标，非账本） |
| 账本对账滞后 | 监控 `attempts.reconciled=false` 积压（[02 idx_attempts_unrecon](./02-data-model.md)） |
| 采集健康 | 凭证状态（`collector_credentials.status`）、快照陈旧率（`is_stale`）；凭证失效告警 P2 |
| 业务告警 | P1/P2/P3 经 `alert_events` 出（[05 §5.2](./05-scheduling-and-operations.md)）；P1 不得延迟（FR-103） |

---

## 7. M0 部署清单（退出标准）

- [ ] `docker compose up` 一键起全栈；`/healthz` 全绿。
- [ ] AxonHub beta5 用 PG 共库；bootstrap 幂等下发 retryPolicy 置零（单测校验全字段）。
- [ ] 停掉 sla-core-a，服务经 core-b 不中断（FR-110 验证）。
- [ ] verify/ harness 对当前 beta5 跑通（升级门禁基线）。
- [ ] CI：Go 构建 + `config_params`/别名策略加载 + 不可存列 schema 断言。
- [ ] `pg_dump`/restore 演练脚本就位。

---

## 8. 开放点（评审需拍板）

| # | 开放点 | 建议 |
| --- | --- | --- |
| 1 | AxonHub beta5 是否支持多实例共 PG（消除数据面单点） | M0/M1 期实测；不支持则维持单实例 + 进程自愈（一期可接受） |
| 2 | LB 选 Caddy vs Nginx | Caddy（自动 TLS、配置简单、单机足够） |
| 3 | collector 独立容器 vs core 子命令 | 一期同二进制子命令（部署简单）；量级上来再拆独立容器/独立扩缩 |
| 4 | 备份加密（含明文 Key 的 dump） | 一期 dump 落本机加密卷；对外前随 FR-113 一起升级 |

---

_本篇为 M0 部署基线。生产永远锁定 AxonHub 具体 tag；任何 AxonHub 升级先过 verify/ 准入门（硬约束 11）。_
