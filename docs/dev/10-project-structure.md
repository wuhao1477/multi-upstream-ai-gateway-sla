# 10 工程结构与构建（Go 单仓 · M0 骨架 · v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ **v1.0 基线（2026-07-26 冻结）** —— 经 42 轮对抗性审查（含 5 轮开发视角）+ PM 开工前裁决；变更须走版本记录 |
| 日期 | 2026-07-23 |
| 定位 | 补齐设计审查发现的空缺：M0 首个交付物是"Go 工程骨架"，但此前无目录/包/构建约定。本篇定 module、目录布局、包边界、构建与 CI |
| 输入 | [00 里程碑](./00-overview-and-milestones.md)、[01 架构六模块](./01-architecture.md)、[03 上游对接层](./03-upstream-layer.md)、[04 CollectorAdapter](./04-collector-adapter.md)、[06 部署](./06-deployment-and-operations.md)、[09 管理 API](./09-admin-api.md) |
| 约束 | 单仓单 module；包边界对齐 01 的模块划分；一期 Go（版本用 go.mod 里 `go 1.2x`，跟随稳定版，不锁死具体小版本） |

> **务实边界**：只定"能开工、不返工"的骨架结构，不预先造抽象。目录按 01 已定的模块切，一个模块一个包；接口（upstream.Client / CollectorAdapter）已在 03/04 定义，这里只定它们**放哪**。

---

## 1. 单仓布局

```
下表 ✅ = 已存在，⏭ = 该阶段开工时新建。**空占位包不预先创建**
（2026-08-29：原先七个只含 `doc.go` 的占位包已删 —— 空包不编译出任何东西，
`go list ./...` 里多七行反而让"哪些模块真做了"读不出来。目录设计仍以本表为准）。

```
multi-upstream-ai-gateway-sla/
├── docs/                      # 现有文档(需求/选型/dev),不动
├── verify/                    # 验收脚本;上游走真站点(CLAUDE.md §1),仅 SSE 流夹具是造的
├── web/                    ✅ # 管理界面 Vue3 工程(源码);产物 embed 进二进制
├── go.mod                     # module: github.com/wuhao1477/multi-upstream-ai-gateway-sla
├── go.sum
├── Makefile                   # build/test/lint/migrate 入口
├── cmd/
│   ├── sla-core/           ✅ # 主服务入口(数据平面+管理平面同进程)
│   ├── collector/          ✅ # 采集器子命令入口(06 已定:一期同二进制子命令)
│   └── migrate/            ✅ # 迁移子命令(容器 entrypoint 与本地都用它)
├── internal/                  # 全部业务代码(internal 禁外部 import)
│   ├── protocol/           ⏭ # 对外 OpenAI CC/Responses 端点、透传、会话标识提取(02 §4.5)
│   ├── policy/             ⏭ # 别名→策略解析(FR-062/117)
│   ├── selector/           ⏭ # 候选过滤+排序+RoutePlan(05 §1)
│   ├── executor/           ⏭ # 流式执行、内容感知首字、期限、取消(03/05 §1.4;AC-31/32)
│   ├── ledger/             ⏭ # 逐 Attempt 账本 + outbox 持久化协议(01 §5.1, 02 §9.2bis)
│   ├── steward/            ⏭ # 冷却/样本门槛/余额信号/告警/错误预算(05 §3~5)
│   ├── upstream/           ⏭ # 自研上游对接层(03):字节透传 + 旁路观察
│   │   ├── client.go          # Client 接口(03 §2)
│   │   ├── passthrough.go     # 字节透传管道 + tee
│   │   └── ssescan.go         # SSE 扫描器 + ShouldCommit/HasTTFTOutput 双判定(03 §3.2)
│   ├── collector/          ✅ # CollectorAdapter 接口 + 各家族实现(04)
│   │   ├── collector.go       # 接口(04 §1)
│   │   ├── registry.go        # 站型注册表:加站型的唯一落点(04 §7bis)
│   │   ├── register_*.go      # 一家族一份 Registration,与下面的实现一一对应
│   │   └── newapi.go sub2api.go            # 平铺,一家族一文件(不再分子包:
│   │                          #   各族共用 httpx/auth/detect,分包只会互相 import)
│   │                          #   接一个自研站 = 加 register_X.go + X.go 两个文件,
│   │                          #   本目录其它文件都不用改(04 §7bis)
│   ├── store/              ✅ # PG 访问层、schema 迁移、UUIDv7、快照重建(02)
│   ├── admin/              ✅ # 管理平面 /admin/* API + 二次确认(09)
│   ├── config/             ✅ # config_params 读写、ParamMeta 元数据(09 §4)
│   ├── health/             ✅ # /healthz(只看实例自身,不看渠道健康,01 §6)
│   └── bootstrap/          ✅ # 启动选主(PG advisory lock)、迁移与快照初始化(06 §2.2)
├── migrations/                # SQL 迁移(sqlc 对齐)
├── deploy/
│   ├── docker-compose.yml     # Caddy + 2×core + PG + collector(06 §1)
│   ├── Caddyfile
│   └── .env.example
└── .github/workflows/
    ├── gate.yml            # 交付门禁：verify/gate.sh(文档12类+DDL真跑) + go build/test/lint
    └── build-arm.yml       # arm64 镜像构建(云端真机)
```

**包边界原则**：一个 `internal/` 子包对应 [01 §2](./01-architecture.md) 的一个模块，**依赖单向**：`protocol → policy → selector → executor → upstream`；`ledger`/`steward` 读写 `store`；`store` 不反向依赖业务包。

---

## 2. 关键包职责速查（对齐已有设计，不新增语义）

| 包 | 职责 | 主要来源 |
| --- | --- | --- |
| `protocol` | 入站 CC/Responses 端点；原样透传（不落正文 FR-112）；**会话标识 7 级提取**（02 §4.5） | FR-111、02 §4.5 |
| `selector` | 候选过滤（顺序不可交换）+ 排序（价格版本×倍率 + 会话粘性）+ RoutePlan（每跳期限）+ 接管准入/防抖动 | 05 §1 |
| `executor` | 旁路观察 SSE 事件、内容感知 TTFT、期限到 `Close()` 传播取消（**不重组字节流**） | 03 §3、AC-31/32 |
| `ledger` | Attempt 账本(单一真相源)、**outbox 写入与崩溃重放**、取消口径归并、gateway_overhead 计算 | 02 §4/§9.2bis、01 §5.1 |
| `upstream` | 自研直连：字节透传、SSE 扫描、取消传播、usage 提取、协议能力探测 | 03 |
| `store` | PG 访问（**`pgx` + `sqlc`**：手写 SQL 生成类型安全代码，零反射）、迁移、UUIDv7、月分区、内存快照重建；决策路径只读快照 | 02 §9、10 开放点3 |
| `bootstrap` | 启动 `pg_try_advisory_lock` 选主 → 取到锁的 core 跑迁移与初始化，其余跳过轮询就绪 | 06 §2.2 |
| `admin`/`config` | `/admin/*` 管理 API、二次确认、ParamMeta | 09 |

---

## 2bis. sqlc 查询契约（M0/M1 开工清单）

> ⚠️ **开发视角审查第 27 轮 [P1]**：[02](./02-data-model.md) 里全是**事务骨架**（带 `:参数` 的多语句 CTE），它们不是 sqlc 能直接生成的形态。开发拿到手第一步就卡在"`queries.sql` 该怎么写"。本节给出最小清单与切分原则，**不重复 02 的 SQL 正文**（那里是唯一真相源）。

**切分原则**：

| 形态 | 放哪 | 理由 |
| --- | --- | --- |
| 单语句查询/写入 | `queries.sql`，由 sqlc 生成 | 类型安全、零反射 |
| **多语句事务**（dispatch / finalize / recovery / adjust / closeout） | **手写 `pgx` 事务函数**，内部逐条调 sqlc 生成的语句或直接 `tx.Exec` | sqlc 不表达事务边界与"按行数分支"；而这套设计的正确性**恰恰依赖行数判定**（[02 §2bis](./02-data-model.md) 三态判定） |
| 应用层断言 | Go 代码 | `settled=1 / already_applied / conflict` 这类分支 sqlc 无法生成 |

**M0 必需（对应 AC-27 / AC-33-M0）**：

| `-- name:` | 类型 | 说明 |
| --- | --- | --- |
| `CreateGatewayClient` | `:one` | 签发凭证，返回 id 与 `secret_prefix`（**明文只在应用层返回一次，不入库**） |
| `GetClientBySecretPrefix` | `:many` | 按前缀取候选行，哈希校验在 Go 侧做（避免把明文送进 SQL） |
| `ListGatewayClients` | `:many` | **不得** SELECT `secret_hash`（FR-094 不回显） |
| `RevokeGatewayClient` | `:exec` | 置 `status='revoked'` + `revoked_at`/`revoke_reason` |
| `IncrementRPMWindow` | `:one` | [02 §2bis](./02-data-model.md) B 阶段原子语句，返回 `request_count`；**0 行 = 429** |
| `RecordAuthRejection` | `:exec` | `ON CONFLICT ... DO UPDATE` 分钟聚合；**必须能写匿名 401（两列 NULL）** |
| `UpsertConfigParam` / `ListConfigParams` | `:exec` / `:many` | `/admin/config` 读写 + 二次确认标记 |

**M1 追加（账本与配额）**：`CreateModel`（强校验两个 token 上界非空）、`InsertRequestAuthenticated`（C′ 阶段）、`SetRequestStage`、`DispatchFirstAttempt`（**事务函数**）、`DispatchNextAttempt`（**事务函数**）、`CloseoutAttempt`（**事务函数**）、`FinalizeUpstream` / `FinalizeAbort` / `FinalizeRecovery` / `FinalizeDelivery`（**均为事务函数**）、`AdjustReservation`（**事务函数**）、`ScanStaleRequests`（`:many`，含 outbox 反连接）。

> **命名与 02 的对应关系必须写在 `queries.sql` 注释里**（如 `-- 对应 02 §2bis D 阶段`），否则改了 02 没人知道该同步哪条查询。

---

## 3. 构建与 CI

| 目标 | 内容 |
| --- | --- |
| `make build` | 编译 `cmd/sla-core`、`cmd/collector` 为静态二进制 |
| `make test` | 单测；含 SSE 扫描器判定用例（STREAM 场景集）与 outbox 重放幂等性用例 |
| `make lint` | `golangci-lint` |
| `make migrate` | 应用 `migrations/`；本地/CI 用一次性 PG |
| **CI 门禁** | 构建 + 测试 + lint + **`verify/gate.sh`**（文档一致性 6 类检查 + DDL 真跑，见 [02 §9.1bis](./02-data-model.md)）+ **DDL 真跑**（`verify/ddl-check.sh`：抽取 02 的全部 DDL 在 postgres:16 上执行 + 匿名 401 聚合回归；M0 后扩为 `migrations/` + 建分区 + `sqlc generate`，[02 §9.1bis](./02-data-model.md)）+ **FR-112 不可存列断言** + 别名/config 加载冒烟 + **凭证脱敏断言**（日志/抓包不得出现 `sk-` 前缀，[15 O3](./15-scope-and-preflight.md)） |
| 镜像 | `cmd/sla-core` 打最小镜像（distroless/alpine） |

---

## 4. M0 交付物映射（[00 M0 退出标准](./00-overview-and-milestones.md) → 本结构）

> ⚠️ **本表曾把上游相关项列为 M0，与 [00](./00-overview-and-milestones.md) 的「M0 只做骨架与入站侧」冲突**（PM 评估指出）。已按 00 对齐。

| M0 退出标准 | 落在 |
| --- | --- |
| `docker compose up` 一键起全栈 | `deploy/`、`cmd/sla-core` `/healthz` |
| Caddy 只代理 `/v1/*` 与 `/healthz` | `deploy/Caddyfile`（[06 §1](./06-deployment-and-operations.md)） |
| 停一个 core 实例服务不中断 | 无状态 + Caddy 摘除（`deploy/Caddyfile`） |
| M0 固定种子加载 + 种子校验断言 | `migrations/0002_seed_m0.sql`、`internal/bootstrap` |
| 入站鉴权与配额基础（AC-27 + AC-33-M0） | `internal/protocol`、`internal/admin`、`internal/store` |
| `system-probe` 内置凭证（外呼必拒、不可吊销） | `migrations/0002_seed_m0.sql`、`internal/admin` |
| CI：构建 + **DDL 真跑**（`verify/ddl-check.sh`）+ 加载 + 不可存列 + 脱敏断言 | [`.github/workflows/gate.yml`](../../.github/workflows/gate.yml)（**已建**，当前跑文档 12 类 + DDL 真跑 + 抽取产物漂移校验）、`make test`、`make migrate`。⚠️ Go 相关的四项随 M0 骨架补入同一 workflow |
| pg_dump/restore 演练脚本 | `deploy/` 脚本 |
| bootstrap 选主（避免双 core 重复迁移） | `internal/bootstrap` advisory lock；建议加双 core 并发冷启动的集成测试 |

**移入 M1**（此前误列在本表）：上游直连打通与 Responses 保真 diff（`internal/upstream`）、`verify/sse_stream_fixture.py` 场景集接入 CI。
**M0 期间并行但非门禁**：Codex 实机 spike（[15 T1](./15-scope-and-preflight.md)）。

---

## 5. 开放点

| # | 开放点 | 结论 |
| --- | --- | --- |
| 1 | Go 版本锁定策略 | ✅ **已定**：`go.mod` 声明 `go 1.2x` 跟随稳定版；不锁死 patch，CI 用固定 minor |
| 2 | 单 module vs 多 module | ✅ **已定**：单 module（个人项目、包间共享类型多），`internal/` 隔离 |
| 3 | 存储访问层选型 | ✅ **已定（2026-07-23）：`pgx` + `sqlc`** —— pgx 做驱动、sqlc 从手写 SQL 生成类型安全 Go 代码。零运行时反射、账本类重查询可控、与"决策只读内存快照 / **关键账本同步直写、其余经 outbox 异步**"的写路径分工契合（[01 §5.1](./01-architecture.md)、[02 §2bis](./02-data-model.md)）；避免重 ORM 的反射开销与不可控查询。迁移用纯 SQL（`migrations/`）+ sqlc 对齐 |
| 4 | 配置来源（文件 vs env vs config_params） | ✅ **已定**：基础设施走 env（[06 §4](./06-deployment-and-operations.md)），业务策略走 `config_params`（[09](./09-admin-api.md)），不混 |

---

_本篇是 M0 开工的工程约定：目录/包边界对齐 01 模块、构建 CI 含 FR-112 与假设2 的守卫。骨架照此搭，实现按 02~09 填。_
