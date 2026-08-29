# 06 部署与运维（单机 Docker Compose · v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ **v1.0 基线（2026-07-26 冻结）** —— 经 42 轮对抗性审查（含 5 轮开发视角）+ PM 开工前裁决；变更须走版本记录 |
| 日期 | 2026-07-23 |
| 形态 | **单机 Docker Compose**：Caddy LB + 2× sla-core（Go）+ PostgreSQL + collector。**无外部网关**（[11 转向](./11-decision-full-selfbuilt.md)） |
| 约束 | 个人/内部使用；**进程级冗余**：任一 sla-core 实例宕机不影响服务（FR-110/AC-27）。⚠️ 单机部署下 **Caddy / 宿主机 / 单 PG 为共享故障点**，一期不承诺整体可用性数字；上游直连，渠道凭证来自 PG（FR-113） |
| 输入 | [01 架构](./01-architecture.md)、[00 硬约束](./00-overview-and-milestones.md)、[03 上游对接层](./03-upstream-layer.md)、[verify/](../../verify/README.md)、[ISSUE-001 beta5 适配表](../issues/ISSUE-001-tech-assumption-verification.md) |

---

## 1. 拓扑与组件

```
                 ┌───────────────── 单机 Docker Compose ─────────────────┐
客户端 → :443 Caddy(LB/TLS) → sla-core-a :8080 ┐   ← 只代理 /v1/* 与 /healthz
运维 → 容器网络/本机直连 ────→ /admin/* 与 /metrics（不经 Caddy）
                            └→ sla-core-b :8080 ┘─直连→ ~20 上游渠道
                                    ├→ postgres :5432 (账本/台账/价格/健康，多实例共享)
                                    └→ collector (子命令) → 上游站点管理面
```

| 服务 | 镜像/构建 | 端口 | 依赖 | 备注 |
| --- | --- | --- | --- | --- |
| `caddy` | caddy:2 | 443/80 | core-a/b | TLS + 轮询 LB + 健康探测摘除故障实例。⚠️ **`Caddyfile` 只代理 `/v1/*` 与 `/healthz`；`/admin/*` 与 `/metrics` 一律不代理**——二者仅容器网络/本机可达。一期不做的是**多用户/RBAC**（仅本人使用），但**仍有一把静态 `ADMIN_TOKEN`**（[09 §1](./09-admin-api.md) 冻结：网络边界 + 管理令牌**两层都要**）。**两层缺一不可**：网络边界防外部，令牌防同机其它进程（compose 里还跑着 collector 与 mock） |
| `sla-core-a/b` | 本仓库构建（Go） | 8080 | postgres | 无本地状态；`/healthz` 就绪探针；**内含自研上游透传层**（[03](./03-upstream-layer.md)）。**必须 ≥2 实例**（FR-110） |
| `postgres` | postgres:16 | 5432 | — | 单库；账本/台账/价格/健康（单一真相源） |
| `collector` | 同 core 二进制 `collector` 子命令 | — | postgres、上游站点 | 异步旁路；限速；凭证明文（FR-113） |

> **为何 core ≥2**：FR-110 要求任一实例宕机不中断。Caddy 对 `/healthz` 失败的实例自动摘除；两实例无差别（状态全在 PG，账本主键 UUIDv7 无序列争用，[02 §9.3](./02-data-model.md)）。转向自研后**数据面不再有外部网关单点**（[11](./11-decision-full-selfbuilt.md)）。

---

## 2. 上游直连（自研，无外部网关）

转向自研后（[11](./11-decision-full-selfbuilt.md)），**部署里不再有 AxonHub/ccLoad 容器**，随之消失的还有：PG 共库分 schema、Key-per-Channel 开通编排、`updateRetryPolicy` 下发、GraphQL schema 适配、升级准入门禁。

### 2.1 上游渠道与凭证

- 渠道、Key、URL 全部登记在 PG 的 `channels`/`upstream_keys`（一期明文，FR-113），经 [09 `/admin/bindings`](./09-admin-api.md) 管理。
- sla-core 启动时从 PG 加载并构建内存快照；新增/变更渠道经管理 API 落库后刷新快照，**无需重启**。
- 每个 Binding 独立 HTTP 连接池（[03 §6](./03-upstream-layer.md)），避免单个上游卡死拖垮其他渠道。

### 2.2 启动初始化

sla-core 启动做一次幂等 bootstrap：建表/迁移（`migrations/`）、加载 `config_params` 与别名策略、构建内存快照、协议能力探测（[03 §8](./03-upstream-layer.md)）。

**选主**：两个 core 实例并存，为避免重复迁移，bootstrap 前取一把 **PG 咨询锁** `pg_try_advisory_lock(<常量>)`——取到的实例执行迁移与初始化，其余跳过并轮询就绪。用咨询锁而非新建表：无额外依赖、连接断开自动释放不留死锁。

### 2.3 上游不可用的处置

- 单个上游渠道故障 → `selector` 按健康状态摘除（[05 §3.1](./05-scheduling-and-operations.md) 冷却退避）。
- **全部候选不可用** → 按等级排队等待后返回明确的"服务暂不可用"，**禁止旁路直连未授权渠道**（FR-110/AC-27、参数7）。
- sla-core 实例故障 → Caddy 据 `/healthz` 摘除，另一实例继续服务（FR-110）。
- ⚠️ **上游全不可用 ≠ 实例不健康**：此时实例仍应报 `/healthz` 健康并由请求路径返回明确错误；若反向把它判为不健康，Caddy 会摘光实例、客户端只看到 LB 层错误，故障被掩盖（详见 §6 健康语义分层）。

---

## 3. 版本与依赖治理

转向自研后**没有外部网关版本需要跟随**，升级门禁大幅简化：

| 对象 | 治理方式 |
| --- | --- |
| sla-core 自身 | 常规发布流程：构建 → CI 全绿（含 FR-112 不可存列断言）→ 灰度一个实例 → 观察 → 全量 |
| Go 依赖 | `go.mod` 锁定；升级前跑全量测试 |
| **上游协议变更** | 真正需要盯的风险从"网关版本"变为"**上游协议/模型变更**"：`Probe()` 定期探测协议能力（[03 §8](./03-upstream-layer.md)）；`verify/mock_upstream.py` 的场景集作为回归夹具（role-only / 空 SSE / 心跳 / 慢首帧 / abort） |
| Codex 客户端演进 | 其 SSE 生命周期契约与响应头要求（[13 §1](./13-research-reassessment.md)）纳入回归测试；**字节透传使我们对客户端演进天然免疫**——上游发什么原样到达 |

> 对比转向前：不再需要"锁 AxonHub tag + 每次升级跑六假设 harness + 核对 GraphQL 适配表"这一整套（[11 §1.4](./11-decision-full-selfbuilt.md)）。

---

## 4. 配置与密钥

| 类别 | 方式 | 依据 |
| --- | --- | --- |
| 策略参数 | 存 PG `config_params`，非环境变量；关键项二次确认（FR-115） | [02 §2](./02-data-model.md) |
| 基础设施配置 | 环境变量/`.env`（DSN、端口、监听地址） | — |
| 上游 Key / 采集凭证 | **一期明文**存 PG（`upstream_keys.secret`、`collector_credentials.*`）；日志与管理界面脱敏不显示完整 Key（FR-094/113） | FR-113 |
| TLS | Caddy 自动证书（内部可用自签） | — |

> 一期明文是已确认决策（DECISIONS 遗漏 4）；对外提供服务前须重评加密与轮换（FR-113 备注）。

---

## 5. 备份、保留与恢复

| 对象 | 策略 |
| --- | --- |
| PG 全库 | 每日 `pg_dump`；保留 ≥30 天备份 |
| 账本分区 | 月 RANGE 分区，保留 ≥180 天（7 分区），到期 `DETACH`+`DROP`（[02 §9](./02-data-model.md)，FR-112） |
| 分区滚动 | 后台定时任务提前建下月分区 + DROP 超期分区（一期自研任务，非 pg_partman，02 开放点 1） |
| 恢复演练 | M4 前做一次 dump→restore 演练，确认账本可复算（FR-013 历史按原价格版本复算） |
| 不可存守卫 | CI schema 断言：账本表禁出现 `body/messages/prompt/headers` 列（FR-112 硬约束 3，[02 §9.2](./02-data-model.md)） |

---

### 5bis. 最小告警外发（M4，FR-103 的可达性保障）

`alert_events` 落库**之后**，单条 HTTP POST 到 IM webhook：

- URL 走 `config_params['alert_webhook_url']`，为空则不外发；
- **失败仅记日志、不重试、不阻塞落库** —— 告警通道故障绝不能拖垮请求路径；
- ⚠️ 但"仅记日志"意味着**静默失败**：webhook 挂了没人知道，P1 的 15 分钟就又回到纸面。故**必须**同时递增 `alert_webhook_failed_total`（§6 指标清单），并对该计数本身设一条告警规则。
- 一期只发 P1；P2/P3 仍靠 `/admin/alerts` 查询。

---

## 6. 可观测性与健康

| 面 | 做法 |
| --- | --- |
| 就绪/存活 | sla-core `/healthz` **只反映实例自身**：进程存活 + PG 可达 + 配置快照已加载。**绝不把上游渠道可用性计入**（见下方警示）；Caddy 据此摘除实例 |
| 决策延迟 | 自监控 P99 决策开销 ≤50ms（FR-110）。**测法**：发往上游前多打一个时间戳，`决策/网关开销 = 总延迟 − 上游耗时`（对应 [02](./02-data-model.md) `attempts.full_latency_ms − upstream_latency_ms`）；超标告警 |
| **同步写代价** | ⚠️ 上一行的差值**测不到同步写**——首字同步写落在 `upstream_latency_ms` 内被减掉（第 11 轮 [high]）。故另建两个派生指标并单独设阈值：`downstream_ttft_delay_ms = downstream_first_byte_written_at − upstream_first_actionable_at`、`downstream_finish_delay_ms = downstream_write_completed_at − upstream_terminal_at`（列见 [02 `attempts`](./02-data-model.md)，阈值由 [M4 压测](./14-acceptance-matrix.md)冻结） |
| **PG 事务与连接池** | 同步写在请求路径上 → 必须监控 PG 提交延迟 P50/P99、连接池等待时长与**耗尽次数**、每秒同步事务数。任一恶化会直接表现为用户可感的首字变慢 |
| **取消成本敞口** | 客户端取消时拿不到终帧 usage，成本按旁路字节数估算（[02 §2bis](./02-data-model.md)）→ 监控 `取消请求占比` 与 `取消请求估算成本 / 总成本`；后者超阈值（默认 15%）触发 **P3**，提示抽样核对上游账单并调整 `cancel_cost_safety_ratio`。**转向自研后无对账环节，该误差不会被自动纠正**，只能靠这条监控暴露 |
| 账本写入滞后 | **关键账本事实是同步直写**（[01 §5.1](./01-architecture.md)），故这里监控的是 **outbox 未投递积压**（`ledger_outbox.delivered_at IS NULL` 的行数与最老行龄）与投递延迟。积压增长意味着 `finalize_delivery` 落后 → request 终态迟迟不落定 |
| 采集健康 | 凭证状态（`collector_credentials.status`）、快照陈旧率（查 `collector_snapshots_v.is_stale` 视图）；凭证失效告警 P2 |
| 业务告警 | P1/P2/P3 经 `alert_events` 出（[05 §5.2](./05-scheduling-and-operations.md)）；P1 不得延迟（FR-103）。写入路径（含 `dedup_key` 同因合并与生命周期行锁）**M3 交付**，M4 只补经营闭环类 |
| **指标暴露** | sla-core 暴露 **Prometheus 文本端点 `/metrics`**，**仅内网监听**（与 `/admin` 同边界，Caddy 不代理）。`deploy/` 附**可选** `prometheus`(+grafana) compose profile，**默认不启动** —— 一期不自建面板，M4 压测直接从 `/metrics` 取数 |

**`/metrics` 必须暴露的指标**（清单即上表各项 + 下列）：

| 指标 | 类型 | 用途 |
| --- | --- | --- |
| `gateway_overhead_ms`（P50/P99） | histogram | FR-110 自监控 |
| `downstream_ttft_delay_ms` / `downstream_finish_delay_ms` | histogram | 两次同步写的真实代价（AC-34/36 门禁） |
| `pg_commit_latency_ms`、`pg_pool_wait_ms`、`pg_pool_exhausted_total` | histogram/counter | 同步写在请求路径上，池耗尽会直接表现为首字变慢 |
| **`cache_hit_rate`**（按 binding / cache_scope） | gauge | `cache_switch_min_hit_rate=0.6` 可调优的前提是有数可看 |
| `outbox_undelivered`、`outbox_oldest_age_s` | gauge | `finalize_delivery` 落后 → request 终态迟迟不落定 |
| `canary_inflight`、`probe_budget_used` | gauge | 两轨验证的实时占用 |
| **`alert_webhook_failed_total`** | counter | 见 §6bis：webhook 静默失败时唯一的可见信号 |
| `ledger_batch_size` / `ledger_batch_flush_ms` / `ledger_commit_total` | histogram/counter | 组提交是否生效、攒批是否拖慢首字（[14 §2ter](./14-acceptance-matrix.md) 要求开/关对比） |
| `requests_by_final_status` | counter | SLA 聚合与错误预算 |

> 🔒 `/metrics` **不得**包含任何凭证、URL 中的 key、请求正文片段（与 [12 §6](./12-debuggability.md) 脱敏同标准）。

> ⚠️ **健康语义必须分层（对抗性审查发现）**：若把"上游渠道可用性"计入 `/healthz`，则**全部上游不可用时两个 core 都会被判不健康 → Caddy 摘除全部实例 → 客户端收到的是 LB 层 502/503**，而不是我们设计的"明确不可用响应 + 事件编号 + 建议重试时间"（AC-15/AC-27），**账本不记录、告警不触发、故障被掩盖**。
>
> 因此三层语义严格分开：
>
> | 层 | 判据 | 失败后果 |
> | --- | --- | --- |
> | `/healthz`（LB 就绪） | 实例自身：进程 + PG 可达 + 快照已加载 | Caddy 摘除该实例，另一实例继续服务 |
> | 渠道健康（selector 状态） | 各 binding 的健康度/冷却/余额/配额 | 该候选被排除出 RoutePlan，**不影响实例就绪** |
> | 全部候选不可用 | selector 输出空候选集 | **请求路径**按等级排队后返回明确错误 + 事件编号，落账本、触发 P1 告警（§2.3、AC-15/27） |
>
> 另设 `GET /admin/health` 暴露渠道级健康（[09](./09-admin-api.md)），供运维观察——它与 `/healthz` 是两回事，**不参与 LB 判定**。

---

## 7. M0 部署清单（退出标准）

> ⚠️ **2026-07-26 裁决澄清**：此前本清单混入了上游相关项，与 [00](./00-overview-and-milestones.md) 的「M0 只做骨架与入站侧」冲突，开发无从判断 M0 到底要交付什么。**以下清单已按 00 对齐**——上游透传、35 字段 diff、mock 场景集全部移入 M1。

> ⚠️ **2026-07-27 再拆分（[ISSUE-005](../issues/ISSUE-005-phase1-upstream-inventory.md) 交付切分后）**：本清单原本假定 M0 之后直接进 M1（网关），故把**别名种子、`system-probe`、AC-33 入站鉴权**一并列为 M0 门禁。但交付顺序已改为 M0 → **P1（采集与管理）** → P2（网关），而这三项**服务的都是 P2**：别名映射路由策略、`system-probe` 是探测凭证、AC-33 是 `/v1/*` 的入站鉴权 —— **P1 没有 `/v1/*`、不做路由**（ISSUE-005 §2 与 D7）。
>
> 若不拆分，P1 会被卡在自己用不到的前置上。故按"谁需要谁负责"分成两段。

**M0 门禁（P1 开工前必须全绿）**：

- [ ] `docker compose up` 一键起全栈；`/healthz` 全绿。
- [ ] `Caddyfile` **只代理 `/v1/*` 与 `/healthz`**；`/admin/*` 与 `/metrics` 不可从外部到达（§1）。**须反向确认管理面在容器网络内可用**，否则"外部 404"可能只是服务挂了而非未代理。
- [ ] 停掉 sla-core-a，服务经 core-b 不中断（FR-110/AC-27）。⚠️ **M0/P1 阶段打 `/healthz`**（`/v1/*` 属 P2），P2 起改打 `/v1/chat/completions` 并**重跑 AC-27**（[14 AC-27](./14-acceptance-matrix.md)）。
- [ ] 迁移与 `config_params` 种子幂等；**运维改过的值不被重启抹回**。
- [ ] 双实例并发冷启动**只有一个执行迁移**（PG 咨询锁选主，§2.2）。
- [ ] `GET /admin/config` 可回显全部配置项；关键项强制二次确认；表外键 400。
- [ ] **`admin_token` 不落 `config_params`**（[09 §4bis](./09-admin-api.md)：避免自己改自己 —— 落表等于管理 API 能改自己的鉴权令牌）。
- [ ] CI：Go 构建/vet/单测 + **DDL 在临时 PG 真跑** + 迁移与文档一致 + FR-112 不可存列断言。
- [ ] `pg_dump`/restore 演练脚本就位。

**⏭ P2 开工前才需要的（原列在 M0，2026-07-27 移出）**：

- [ ] 加载固定种子（1 model + 3 别名 + 策略 + `sla_targets`）并经 `GET /admin/config` 回显一致。
- [ ] 种子校验断言：**`is_committed=true` 的策略必须有对应 `sla_targets` 行**（禁止空承诺）。
- [ ] `system-probe` 内置凭证已创建，且 `/v1/*` 用它鉴权**必被拒**、`/admin/clients` 吊销它**返回 403**。
- [ ] **AC-33 的 M0 子集**通过（401/403/429、RPM 跨实例、重启不清零、`rpm_limit IS NULL` 不限速、凭证脱敏、匿名 401 落库）。
- [ ] CI 追加：别名策略加载 + 凭证脱敏断言（`sk-` 前缀）。

**⚠️ 客户端接入前置（实测确认，不是可选项）**：

Codex 发送的模型名来自**它自己的 `~/.codex/config.toml`**，与我方 `/v1/models` 返回什么无关（[03 §4 实测](./03-upstream-layer.md)）。故接入时必须：

```toml
# ~/.codex/config.toml
model = "gpt-5.5"                     # ← 必须是我方**别名**，不是上游真实模型名

[model_providers.sla_gateway]
name = "SLA Gateway"
base_url = "http://<网关地址>/v1"
wire_api = "responses"
```

- **不改这一行 → 每个请求都 404**（别名表里没有 `gpt-5.6-sol` 这类上游真名）。
- 迁移期可临时开 `alias_passthrough`（`config_params`，默认关）让老配置先跑通，但**它会绕过别名策略**（SLA 等级、测活资格、数据许可全失效），改完配置应立即关掉。

**M0 期间并行、但不作门禁**：

- [ ] **Codex 实机 spike**（[15 T1](./15-scope-and-preflight.md)）：临时最小透传脚本 + 真实 Codex CLI + 真实上游抓包。timebox 2~3 天；**必答**「Codex 请求的模型名从哪来」（决定 `/v1/models` 合成会不会 404 打不通）与「35 字段是否零丢失」。跑不通则记录阻塞，**不拖 M0**。

**移入 M1**：上游直连打通与 35 字段 diff、`verify/mock_upstream.py` 的 **SSE 流夹具**场景集接入 CI（[14 §1bis](./14-acceptance-matrix.md) 已冻结 12 个场景与 diff 规范）。这一项**能**进 CI：它造的是本机 Python 起的流，不需要任何真凭证 —— 与"要令牌所以只能本地跑"的那三项（[14 §0bis](./14-acceptance-matrix.md)）不同。

---

## 8. 开放点（评审需拍板）

| # | 开放点 | 结论 |
| --- | --- | --- |
| 1 | 数据面单点 | ✅ **已消除**：转向自研后无外部网关容器（[11](./11-decision-full-selfbuilt.md)）；上游直连由 ≥2 个 core 实例承载 |
| 2 | LB 选型 | ✅ **定** Caddy（自动 TLS、配置简单、单机足够） |
| 3 | collector 打包 | ✅ **定** 一期 core 二进制子命令（部署简单）；量级上来再拆独立容器/独立扩缩 |
| 4 | 备份加密（含明文 Key 的 dump） | ✅ **定** 一期 dump 落本机加密卷；对外提供服务前随 FR-113 一起升级 |

---

_本篇为 M0 部署基线。转向自研后不再有外部网关版本治理；需盯的风险转为**上游协议变更**与 **Codex 客户端演进**（§3）。_
