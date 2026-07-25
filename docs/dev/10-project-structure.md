# 10 工程结构与构建（Go 单仓 · M0 骨架 · v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | 草案，待评审（M0 开工即用） |
| 日期 | 2026-07-23 |
| 定位 | 补齐设计审查发现的空缺：M0 首个交付物是"Go 工程骨架"，但此前无目录/包/构建约定。本篇定 module、目录布局、包边界、构建与 CI |
| 输入 | [00 里程碑](./00-overview-and-milestones.md)、[01 架构六模块](./01-architecture.md)、[03 上游对接层](./03-upstream-layer.md)、[04 CollectorAdapter](./04-collector-adapter.md)、[06 部署](./06-deployment-and-operations.md)、[09 管理 API](./09-admin-api.md) |
| 约束 | 单仓单 module；包边界对齐 01 的模块划分；一期 Go（版本用 go.mod 里 `go 1.2x`，跟随稳定版，不锁死具体小版本） |

> **务实边界**：只定"能开工、不返工"的骨架结构，不预先造抽象。目录按 01 已定的模块切，一个模块一个包；接口（upstream.Client / CollectorAdapter）已在 03/04 定义，这里只定它们**放哪**。

---

## 1. 单仓布局

```
multi-upstream-ai-gateway-sla/
├── docs/                      # 现有文档(需求/选型/dev),不动
├── verify/                    # mock 上游场景集(转为透传层测试夹具)
├── go.mod                     # module: github.com/wuhao1477/multi-upstream-ai-gateway-sla
├── go.sum
├── Makefile                   # build/test/lint/migrate 入口
├── cmd/
│   ├── sla-core/              # 主服务入口(数据平面+管理平面同进程)
│   │   └── main.go
│   └── collector/             # 采集器子命令入口(06 已定:一期同二进制子命令)
│       └── main.go
├── internal/                  # 全部业务代码(internal 禁外部 import)
│   ├── protocol/              # 对外 OpenAI CC/Responses 端点、透传、会话标识提取(02 §4.5)
│   ├── policy/                # 别名→策略解析(FR-062/117)
│   ├── selector/              # 候选过滤+排序+RoutePlan(05 §1)
│   ├── executor/              # 流式执行、内容感知首字、期限、取消(03/05 §1.4;AC-31/32)
│   ├── ledger/                # 逐 Attempt 账本、对账、errorMessage 归并、隐藏重试补算(03 §4.4)
│   ├── steward/               # 测活/冷却/订阅倾斜/告警/错误预算(05 §2~5)
│   ├── upstream/              # 自研上游对接层(03):字节透传 + 旁路观察
│   │   ├── client.go          # Client 接口(03 §2)
│   │   ├── passthrough.go     # 字节透传管道 + tee
│   │   └── ssescan.go         # SSE 扫描器 + HasVisibleText 判定(03 §3)
│   ├── collector/             # CollectorAdapter 接口 + newapi/sub2api/asxs/ 实现(04)
│   │   ├── collector.go       # 接口(04 §1)
│   │   ├── newapi/ sub2api/ asxs/
│   ├── store/                 # PG 访问层、schema 迁移、UUIDv7、快照重建(02)
│   ├── admin/                 # 管理平面 /admin/* API + 二次确认(09)
│   ├── config/                # config_params 读写、ParamMeta 元数据(09 §4)
│   └── bootstrap/             # 启动选主(PG advisory lock)、迁移与快照初始化(06 §2.2)
├── migrations/                # SQL 迁移(sqlc 对齐)
├── deploy/
│   ├── docker-compose.yml     # Caddy + 2×core + PG + collector(06 §1)
│   ├── Caddyfile
│   └── .env.example
└── .github/workflows/ci.yml   # 或对应 CI
```

**包边界原则**：一个 `internal/` 子包对应 [01 §2](./01-architecture.md) 的一个模块，**依赖单向**：`protocol → policy → selector → executor → upstream`；`ledger`/`steward` 读写 `store`；`store` 不反向依赖业务包。

---

## 2. 关键包职责速查（对齐已有设计，不新增语义）

| 包 | 职责 | 主要来源 |
| --- | --- | --- |
| `protocol` | 入站 CC/Responses 端点；原样透传（不落正文 FR-112）；**会话标识 7 级提取**（02 §4.5） | FR-111、02 §4.5 |
| `selector` | 候选过滤（顺序不可交换）+ 排序（用满倍率）+ RoutePlan（每跳期限）+ 接管准入/防抖动 | 05 §1 |
| `executor` | SSE 逐事件解析 `HasVisibleContent`、内容感知 TTFT、期限到 `Close()` 传播取消 | 03 §4.3、AC-31/32 |
| `ledger` | Attempt 账本(单一真相源)、取消口径归并、gateway_overhead 计算 | 02 §4 |
| `upstream` | 自研直连：字节透传、SSE 扫描、取消传播、usage 提取、协议能力探测 | 03 |
| `store` | PG 访问（**`pgx` + `sqlc`**：手写 SQL 生成类型安全代码，零反射）、迁移、UUIDv7、月分区、内存快照重建；决策路径只读快照 | 02 §9、10 开放点3 |
| `bootstrap` | 启动 `pg_try_advisory_lock` 选主 → 取到锁的 core 跑迁移与初始化，其余跳过轮询就绪 | 06 §2.2 |
| `admin`/`config` | `/admin/*` 管理 API、二次确认、ParamMeta | 09 |

---

## 3. 构建与 CI

| 目标 | 内容 |
| --- | --- |
| `make build` | 编译 `cmd/sla-core`、`cmd/collector` 为静态二进制 |
| `make test` | 单测；**含 03 §5.1 要求的 `updateRetryPolicy` 全字段常量校验**（防假设 2 坑重现） |
| `make lint` | `golangci-lint` |
| `make migrate` | 应用 `migrations/`；本地/CI 用一次性 PG |
| **CI 门禁** | 构建 + 测试 + lint + **FR-112 不可存列断言**（[02 §9.2](./02-data-model.md)：账本表禁出现 `body/messages/prompt/headers` 列）+ 别名/config 加载冒烟 |
| 镜像 | `cmd/sla-core` 打最小镜像（distroless/alpine） |

---

## 4. M0 交付物映射（[00 M0 退出标准](./00-overview-and-milestones.md) → 本结构）

| M0 退出标准 | 落在 |
| --- | --- |
| `docker compose up` 一键起全栈 | `deploy/`、`cmd/sla-core` `/healthz` |
| 上游直连打通、Responses 零丢失 | `internal/upstream`（[03 §10](./03-upstream-layer.md)） |
| 停一个 core 实例服务不中断 | 无状态 + Caddy 摘除（`deploy/Caddyfile`） |
| verify/ harness 对 beta5 跑通 | 现有 `verify/`（不动） |
| CI：构建 + 加载 + 不可存列断言 | `.github/workflows/ci.yml`、`make test` |
| pg_dump/restore 演练脚本 | `deploy/` 脚本 |
| bootstrap 选主（避免双 core 重复迁移） | `internal/bootstrap` advisory lock；建议加双 core 并发冷启动的集成测试 |

---

## 5. 开放点

| # | 开放点 | 结论 |
| --- | --- | --- |
| 1 | Go 版本锁定策略 | ✅ **已定**：`go.mod` 声明 `go 1.2x` 跟随稳定版；不锁死 patch，CI 用固定 minor |
| 2 | 单 module vs 多 module | ✅ **已定**：单 module（个人项目、包间共享类型多），`internal/` 隔离 |
| 3 | 存储访问层选型 | ✅ **已定（2026-07-23）：`pgx` + `sqlc`** —— pgx 做驱动、sqlc 从手写 SQL 生成类型安全 Go 代码。零运行时反射、账本类重查询可控、与"决策只读内存快照 / 写路径异步"架构契合；避免重 ORM 的反射开销与不可控查询。迁移用纯 SQL（`migrations/`）+ sqlc 对齐 |
| 4 | 配置来源（文件 vs env vs config_params） | ✅ **已定**：基础设施走 env（[06 §4](./06-deployment-and-operations.md)），业务策略走 `config_params`（[09](./09-admin-api.md)），不混 |

---

_本篇是 M0 开工的工程约定：目录/包边界对齐 01 模块、构建 CI 含 FR-112 与假设2 的守卫。骨架照此搭，实现按 02~09 填。_
