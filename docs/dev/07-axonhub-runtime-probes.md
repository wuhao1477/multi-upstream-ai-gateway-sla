# 07 AxonHub 运行时实测（部署开放点收口 · 2026-07-23）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ 已实测收口（三项经验性开放点） |
| 日期 | 2026-07-23 |
| 目的 | 用 Docker 实测 AxonHub `v1.0.0-beta5` 的真实行为，为 01/02/03/06 的部署开放点定稿提供事实依据（不凭空判断） |
| 镜像 | `looplj/axonhub:v1.0.0-beta5`（digest `sha256:1859854…`） |
| 方法 | 单机 Docker（OrbStack）；PG 版 compose + mock 上游；证据为命令实际输出。跑完自清理无残留 |
| 配置发现 | beta5 内置 `axonhub config preview/get/validate` 子命令；DB 键 `db.dialect`/`db.dsn` ↔ 环境变量 `AXONHUB_DB_DIALECT`/`AXONHUB_DB_DSN` |

> 与 [ISSUE-001](../issues/ISSUE-001-tech-assumption-verification.md) 同源方法：只登记实测事实，无法验证的部分明确标注前提。

---

## 1. beta5 支持 PostgreSQL —— ✅ 坐实

**配置**：`AXONHUB_DB_DIALECT=postgres`、`AXONHUB_DB_DSN=postgres://ax:ax@postgres:5432/axonhub?sslmode=disable`。

**证据**：

- `config get db.dialect` → `postgres`；`config validate` → `Configuration is valid!`
- 容器启动无 fatal，`/health` → **200**；启动日志 `system not initialized, skipping migration`（迁移在 `/admin/system/initialize` 时跑，不在启动期）
- PG 版 `setup.py` 全通：`init 200` → 建渠道 `gid://axonhub/Channel/1` → `updateChannelStatus enabled` → 建 Key → warmup `/v1/chat/completions` 首轮 **200**
- PG `\dt`：`public` schema 建 **25 张表**（`api_keys/channels/requests/request_executions/usage_logs/users/systems/provider_quota_status…`）
- **schema 隔离**：DSN 加 `&search_path=ax_custom`（需预先 `CREATE SCHEMA`，ent 不自建）→ 25 表全落 `ax_custom`，`public` 不变

**判定 / 对设计的含义**：AxonHub 数据可直接用 PG，DSN 格式 `postgres://user:pass@host:5432/db?sslmode=disable`；与自研账本**共库分 schema**（`search_path` 指定），`Reconcile` 可本地 JOIN 对账。→ **收口 [02 开放点3](./02-data-model.md#11-开放点评审需拍板) / [06 §2.1](./06-deployment-and-operations.md#21-存储pg-共库分-schema01-开放点-4--02-开放点-3-的建议落地)**。

---

## 2. 多实例共一个 PG —— ⚠️ 稳态双活可行，迁移期无并发安全

同一 PG 上起 `axonhub-a`(8090)、`axonhub-b`(8091)，指向同一 DSN，分两种时序测。

### A. 顺序启动（a 先 init，b 后加入）—— 可行

- b `/health` **200**；b 日志：`skipping migration: system version is equal or newer than migration version`（读到 PG 已初始化，跳过迁移）
- **状态实时共享**：a、b 两侧 GraphQL `channels`/`apiKeys` 返回**完全一致**（`Channel/1 chan-mock enabled` + `APIKey/1`）
- 用 a 建的 Key 打 **b 的** `/v1/chat/completions` → **200**，成功路由
- PG 计数：`channels=1, api_keys=1, requests=2, request_executions=2`——单一共享数据集，无重复；无迁移锁报错

### B. 并发冷启动 + 并发 init（fresh PG，a、b 同时起同时 init）—— b 崩溃

- a：init 200、`public` 25 表齐全
- b：**panic 退出**（`Exited(2)`）：
  ```
  panic: sql/schema: create "api_keys" table: ERROR: duplicate key value
  violates unique constraint "pg_type_typname_nsp_index" (SQLSTATE 23505)
    at db.NewEntClient (internal/server/db/ent.go:76)
  ```
  两实例同时跑 ent auto-migration（`disable_auto_migration` 默认 false），在同一 `public` schema 争抢 PG 系统目录 `pg_type`，**无 advisory lock**，b 直接 panic。

**判定 / 对设计的含义**：

- **FR-110 数据面单点可用 AxonHub 双实例 + 共享 PG 消除**（稳态双活实测通过：状态共享、任一实例可读写与路由）。
- **硬约束（须写进部署 runbook）**：schema 迁移期不具并发安全——**初始化只让一个实例做；滚动升级须先单实例迁移完，再拉起其余实例**（或运维层加迁移锁 / init-container）。否则并发迁移崩实例。
- → **收口 [01 开放点1](./01-architecture.md#6-开放点评审需拍板) / [06 §2.3 与开放点1](./06-deployment-and-operations.md#23-单点风险与处置01-开放点-1)**。

---

## 3. OpenAI Responses 透传 —— ⚠️ 原生支持，但取决于渠道类型且为结构化转译

查路由表 → 给 mock 加 `/v1/responses`（流式 typed SSE + 非流式 JSON，带自定义 `MOCK_MARKER` 探针）→ 分别用 `openai` 与 `openai_responses` 渠道打。

**证据**：

- 路由表原生注册 `POST /v1/responses`(`CreateResponse`)、`/v1/responses/compact`——**inbound `/v1/responses` 原生支持，非 404**；`ChannelType` 枚举含专门的 **`openai_responses`**
- **`openai` 渠道**：客户端打 `/v1/responses` 返回结构良好的 Responses 对象（`object:response`、typed SSE 齐全）HTTP 200；但 **mock 上游实收 `/v1/chat/completions`**（内容 `hello from chat`、id `chatcmpl-…`）→ AxonHub 把 Responses **下转 Chat Completions 打上游再上转回**（不碰上游 `/v1/responses`）
- **`openai_responses` 渠道**：**mock 上游实收 `/v1/responses`**（id `resp_mock_…`、内容 `hello from responses`）→ **真打上游原生 Responses**。流式亦通（上游事件需带 `type` 字段；缺失则 axonhub 报 `Failed to aggregate chunks: empty stream chunks` 回空流）
- **非字节级透传**：顶层自定义字段 `MOCK_MARKER` 被丢弃；item id 被重签（`msg_mock_1`→`item_wpLcmzaW…`）、`created_at` 归零、`sequence_number` 重编 → **解析进 AxonHub Responses 领域模型再重序列化**（已知字段忠实，未知/额外字段丢失）
- **落账**：`requests.format` 全记 `openai/responses`，`status=completed`；7 条 `request_executions` 全 `completed`

**判定 / 对设计的含义**：

- beta5 **支持** OpenAI Responses 端到端（inbound 原生 + `openai_responses` 渠道 outbound 到上游原生）。
- **两个必须知道的约束**：① 是否打上游原生 Responses **取决于渠道 type**——`openai` 型会**静默下转** Chat Completions（上游 Responses-only 语义丢失），要保原生须配 `openai_responses`；② 即便用 `openai_responses`，也是**经领域模型 round-trip 的结构化转译**，重签 item id、丢未知字段，**非透明字节代理**。
- **对 FR-111**：若只需"标准 Responses 字段收发"，AxonHub 用 `openai_responses` 渠道即满足，无需自研直连转换。若需**严格保真**（保留 Responses-only 结构/自定义字段/原始 item id、`reasoning`/tool 专有内容零丢失），须自研核心 protocol 层直连转换补齐；且 GatewayAdapter 须确保 Responses 渠道被配成 `openai_responses`（误配 `openai` 会静默降级）。
- → **收口 [01 开放点2](./01-architecture.md#6-开放点评审需拍板) / [03 §4.3](./03-gateway-adapter.md#43-execute--内容感知首字由-executor-判定ac-3132)**。

---

## 4. 诚实的前提局限

- 实测项 3 的"上游原生 Responses"用**自造 mock**（响应结构按 OpenAI Responses 规范手写）。对**真实** OpenAI Responses API 的字段级保真度，**因无真实 OpenAI 凭证、未出网直连验证**。`MOCK_MARKER` 丢弃 / item id 重签 / created_at 归零为实测事实，据此推断 AxonHub 走领域模型 round-trip；若要对真实上游做保真度定量核对，需真实 OpenAI API Key + 允许出网（列为 M1 待补，与 verify/ 准入检查一并）。

---

## 5. 收口的开放点一览

| 开放点 | 原出处 | 实测结论 |
| --- | --- | --- |
| AxonHub 是否支持 PG 共库 | 01 开放点4、02 开放点3、06 §2.1 | ✅ 支持，`search_path` 分 schema |
| AxonHub 多实例能否消除数据面单点 | 01 开放点1、06 开放点1 | ✅ 稳态双活可行；⚠️ 迁移须串行（runbook 硬约束） |
| Responses 协议透传完整度 | 01 开放点2、03 §4.3 | ⚠️ 原生支持需 `openai_responses` 渠道；结构化转译有字段级损耗，严格保真需自研层 |

清理确认：`docker compose down -v` + 删 axonhub 镜像 + 删临时目录；`docker ps -a` 无残留、端口释放、本仓库 `git status` clean。
