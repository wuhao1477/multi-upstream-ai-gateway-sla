# 07 AxonHub 运行时实测（部署开放点收口 · 2026-07-23）

| 项目 | 内容 |
| > 📌 **2026-08-29 补注**：本篇 §3 → §3bis 的翻案，是 [CLAUDE.md §1](../../CLAUDE.md)
> 「禁止 mock」那条规则的**原始证据**，比规则本身早一个月。§3 用 mock 推断出
> 「标准字段够用、无需自研转换」，§3bis 换真上游实测发现 AxonHub 丢弃 80% 顶层
> 字段 —— 原因写在 §4 结论里：**mock 响应本身就不含这些字段，所以 mock 永远暴露
> 不了字段丢失**。本篇的 mock 提及全部保留：它们是"当时用了什么方法"的史实，
> 改写会让这段翻案读不通。
>
> ⚠️ **历史记录（2026-07-25 标注）**：本篇实测已用于支撑 [11 架构转向决策](./11-decision-full-selfbuilt.md)——一期**移除 AxonHub、改为自研直连**。§3bis 的真实上游 Responses 基线（35 字段、reasoning item、`prompt_cache_key`）**转为自研透传层要 100% 保真的目标**（见 [03 §10](./03-upstream-layer.md)）。**本篇不再指导部署**；部署见 [06](./06-deployment-and-operations.md)。

--- | --- |
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

**判定 / 对设计的含义**：AxonHub 数据可直接用 PG，DSN 格式 `postgres://user:pass@host:5432/db?sslmode=disable`；与自研账本**共库分 schema**（`search_path` 指定），`Reconcile` 可本地 JOIN 对账。→ **收口 [02 开放点3](./02-data-model.md#11-开放点评审需拍板) / [06 §2](./06-deployment-and-operations.md#2-上游直连自研无外部网关)**。

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
- → **收口 [01 开放点1](./01-architecture.md#6-开放点评审需拍板) / [06 §2.3](./06-deployment-and-operations.md#23-上游不可用的处置)**。

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
- → **收口 [01 开放点2](./01-architecture.md#6-开放点评审需拍板) / [03 §3.2](./03-upstream-layer.md#32-两个独立判定shouldcommit-与-hasttftoutputac-31)**。

---

## 3bis. 真实上游保真度实测（2026-07-25 补测，**推翻 §3 的 mock 推断**）

§3 用 mock 只能推断"重签 item id、丢少量未知字段"。用**真实 Responses 上游**（NewAPI 中转站 `api.lyjxka.top`，模型 `gpt-5.5`）做直连 vs 经 AxonHub 的同请求逐字段 diff，**实际损耗远比 mock 推断严重**。

**方法**：同一请求体（含探针 `prompt_cache_key`、`metadata`）分别打直连上游与经 AxonHub（`openai_responses` 渠道），对比响应。

### 结果：顶层字段 35 → 7，丢失 28 个

| 字段 | 直连 | 经 AxonHub | 对本项目的影响 |
| --- | --- | --- | --- |
| `prompt_cache_key` | `1743acabce91c7ec` | ❌ **丢失** | **[02 §4.5](./02-data-model.md) 会话标识提取链序 3 失效** |
| `previous_response_id` | `null`（字段存在） | ❌ **丢失** | **提取链序 5 失效** |
| `reasoning` | `{context, effort:medium, mode, summary}` | ❌ **丢失** | Codex 主力字段 |
| `prompt_cache_retention` | `24h` | ❌ **丢失** | 缓存策略依据 |
| `store` / `service_tier` / `instructions` / `temperature` / `tools` / `text` / `truncation` 等 | 有 | ❌ **全丢** | 共 28 个 |
| `status` / `model` | `completed` / `gpt-5.5` | ✅ 保留 | — |

### 更严重：`output` 数组的 `reasoning` item 被整条吞掉

| | 直连 | 经 AxonHub |
| --- | --- | --- |
| `output` 条数 | **2**（`reasoning` + `message`） | **1**（仅 `message`） |
| `usage.reasoning_tokens` | `12` | **`0`** |

即 AxonHub 的领域模型 round-trip **不保留 reasoning 输出项**，且 usage 中的 reasoning token 计数归零。

### 落账（正常）

`requests.format=openai/responses`、`status=completed`、`externalID=resp_...`；`usageLogs` 的 `promptTokens=4401`、`promptCachedTokens=3840` 与上游一致（`totalCost=null` 因本次未配价格，符合预期）。

### 结论（**修正 §3 与 [03](./03-upstream-layer.md) 的判断**）

- §3 基于 mock 的"标准字段够用、无需自研转换"**不成立**。真实上游下 AxonHub 丢弃 80% 顶层字段与整条 reasoning item。
- **主力 Codex 的会话标识仍可用**——因其走 HTTP 头（`session_id`/`conversation_id`，[02 §4.5](./02-data-model.md) 序 1/2），AxonHub 不碰头部；但 **body 来源（序 3/5）经 AxonHub 后全部失效**。
- 若需保留 `reasoning`、缓存亲和键等 Responses 专有语义，**必须由自研 `protocol` 层直连转换**，不能依赖 AxonHub 透传。

## 4. 前提局限

- §3 的 mock 结论已由 §3bis 的真实上游实测**取代**（mock 无法暴露字段丢失，因 mock 响应本身不含这些字段）。
- §3bis 用的是 NewAPI 中转站的 `gpt-5.5`，非 OpenAI 官方端点；但其响应结构为标准 OpenAI Responses（35 字段齐全、含 reasoning/缓存字段），足以暴露 AxonHub 的 round-trip 损耗。对 OpenAI 官方端点的核对可在有官方凭证时补做，**预期结论不变**（损耗发生在 AxonHub 侧，与上游来源无关）。

---

## 5. 收口的开放点一览

| 开放点 | 原出处 | 实测结论 |
| --- | --- | --- |
| AxonHub 是否支持 PG 共库 | 01 开放点4、02 开放点3、06 §2.1 | ✅ 支持，`search_path` 分 schema |
| AxonHub 多实例能否消除数据面单点 | 01 开放点1、06 开放点1 | ✅ 稳态双活**可行**（迁移须串行）。**但决策取默认单实例**（[06 §2.3](./06-deployment-and-operations.md)）：FR-110 只约束核心，双实例复杂度不划算；本实测作为"需要时可启用"的后备验证留档 |
| Responses 协议透传完整度 | 01 开放点2、03 §4.3 | ⚠️ **已用真实上游收口（§3bis）**：原生支持需 `openai_responses` 渠道；**顶层字段 35→7（丢 28 个，含 `prompt_cache_key`/`previous_response_id`/`reasoning`），`output` 的 reasoning item 被整条吞掉**。主力 Codex 靠 HTTP 头取会话标识不受影响；但需保留 Responses 专有语义**必须自研 protocol 层直连** |

清理确认：`docker compose down -v` + 删 axonhub 镜像 + 删临时目录；`docker ps -a` 无残留、端口释放、本仓库 `git status` clean。
