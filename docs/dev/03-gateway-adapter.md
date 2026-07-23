# 03 GatewayAdapter 契约（v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | 草案，待评审（M1 前定稿） |
| 日期 | 2026-07-23 |
| 定位 | 自研 sla-core 与执行网关之间的**执行面契约**（同步请求路径 + 异步对账），不涉及采集侧（采集契约见 [ISSUE-002 §2](../issues/ISSUE-002-collector-adapter-design.md#2-适配器契约)） |
| 抽象 | `GatewayAdapter` Go 接口：`ProvisionBinding` / `Execute(stream)` / `Reconcile` / `SetPricing` / `ReadQuota` / `Teardown` + 显式 `Capabilities()` |
| 实现 | ① **AxonHub `v1.0.0-beta5`**（主）②**ccLoad `v3.6.1`** 退路空壳 |
| 输入 | [PRD v1.3](../PRD.md)、[DECISIONS](../DECISIONS.md)、[ISSUE-001 运行时结论](../issues/ISSUE-001-tech-assumption-verification.md)、[ISSUE-002 采集适配器设计](../issues/ISSUE-002-collector-adapter-design.md)、[ISSUE-002 探测结果](../issues/ISSUE-002-probe-results.md)、[00 总览](./00-overview-and-milestones.md)、[01 架构](./01-architecture.md) |
| 版本锁定 | AxonHub `v1.0.0-beta5`（镜像 `looplj/axonhub:v1.0.0-beta5`）；升级前须跑 [verify/](../../verify/README.md) 准入检查（硬约束 11） |

> 本篇只把 **ISSUE-001 运行时已证实的机制**固化为接口。凡"源码推断但未运行时收口"的行为，均在契约中标注为**降级项**或**自研层补偿项**，绝不写成默认可信。

---

## 1. 为什么需要这层抽象

`sla-core` 的 `selector` / `executor` / `ledger` 三模块（见 [01 架构 §2](./01-architecture.md#2-sla-core-内部模块)）必须对"底层用哪个执行网关"无感：

- **调度与账本在自研核心**，网关只做"受控执行 + 回传逐尝试记录"（[00 §1](./00-overview-and-milestones.md#1-要建什么一句话)）。
- 运行时验证证明 **AxonHub 与 ccLoad 的能力边界不同**（TTFT 字段都不可信、配额执行默认关闭、ccLoad 有 Codex 隐藏重试），因此 executor / ledger **不能内联任一网关的假设**，必须经统一接口 + **显式能力声明**消费。
- AxonHub 单实例是数据面单点（[01 §6 开放点 1](./01-architecture.md#6-开放点评审需拍板)），退路 ccLoad 必须是"换实现不换调用方"的空壳，随时可启用。

**能力声明原则**（沿用 ISSUE-002 §2）：适配器对不支持的能力返回 `Unsupported` 而非静默留空；对"可采但不可信"的字段返回 `Degraded` 并注明原因。调用方据 `Capabilities()` 决定是否启用自研层补偿。

---

## 2. Go 接口签名

```go
package gateway

import (
	"context"
	"time"
)

// GatewayAdapter 是 sla-core 与执行网关之间的唯一契约。
// 一个进程内每种网关一个实现；Binding 与渠道一一对应（Key-per-Channel）。
type GatewayAdapter interface {
	// 能力自述：调用方据此决定降级与补偿开关。必须在任何其他调用前可用。
	Capabilities() Capabilities

	// ProvisionBinding 幂等地开通"单渠道绑定"：在网关内建/复用一把锁死单渠道的凭证，
	// 返回可供 Execute 使用的 Binding 句柄。禁止人工建 Key（01 §6 开放点 3）。
	ProvisionBinding(ctx context.Context, spec BindingSpec) (Binding, error)

	// SetPricing 下发某渠道某模型的单价，使网关 usageLogs 产出可对账的 totalCost。
	// 幂等；价格版本由自研核心持有，网关侧只作执行期计价。
	SetPricing(ctx context.Context, b Binding, prices []ModelPrice) error

	// ReadQuota 读取该 Binding 所在渠道的 provider 配额状态（可能为 Unknown）。
	// 仅"读取并存证"；候选过滤由 selector 负责（FR-118），本方法不做任何排除。
	ReadQuota(ctx context.Context, b Binding) (QuotaStatus, error)

	// Execute 沿单个 Binding 执行一次 Attempt（一跳）。
	// 流式：返回 AttemptStream，由 executor 做内容感知首字判定与期限控制。
	// ctx 取消即向网关传播取消 → 网关拆上游连接止损（AC-32）。
	Execute(ctx context.Context, b Binding, req *ExecRequest) (AttemptStream, error)

	// Reconcile 按外部 RequestID / 时间窗从网关拉逐尝试记录与用量，
	// 归一为 AttemptRecord 交 ledger。含 errorMessage 归并、隐藏重试补算（FR-119）。
	Reconcile(ctx context.Context, key ReconcileKey) ([]AttemptRecord, error)

	// Teardown 回收 Binding 占用的网关资源（Key / profile）。幂等。
	Teardown(ctx context.Context, b Binding) error
}
```

### 2.1 关键类型

```go
type Capabilities struct {
	Name              string // "axonhub-beta5" | "ccload-v3.6.1"
	SingleChannelBind Support // 单资源绑定是否可信
	TrustsGatewayTTFT Support // 恒为 Unsupported：网关首字字段不可信（AC-31）
	CancelPropagation Support // mid-stream 取消是否传播到上游止损
	PerAttemptLedger  Support // 逐尝试记录是否可对账
	HiddenRetryRisk   HiddenRetryProfile // 网关是否会在同资源内隐藏重试
	QuotaEnforcement  Support // 网关自身配额执行是否可信（两者均 Degraded）
	DegradedFields    map[string]string  // 字段 -> 降级原因
}

type Support int
const (
	Supported Support = iota
	Degraded          // 可采但不可直接采信，需自研层补偿
	Unsupported       // 不提供，返回而非静默留空
)

type HiddenRetryProfile struct {
	MayHiddenRetry bool     // 是否存在"一次外部调用→多次上游调用"
	Triggers       []string // 触发条件，如 "codex+400+reasoning-body-rewrite"
	CountableFrom  string   // 补算依据："executions-rows" | "adapter-inference"
}

type BindingSpec struct {
	ChannelRef   string   // 渠道逻辑标识（自研核心侧）
	UpstreamURL  string   // 上游 base_url
	UpstreamKey  string   // 上游 Key（一期明文，FR-113）
	ChannelType  string   // "openai" | "anthropic" | "codex" | "claudecode" ...
	Models       []string // 该渠道支持的模型
}

type Binding struct {
	ID           string // 自研核心侧稳定 ID
	GatewayKeyID string // 网关内 API Key 的 relay gid
	ChannelGID   string // 网关内 Channel 的 relay gid，如 gid://axonhub/Channel/1
	ChannelNum   int    // gid 末段数字，供 channelIDs([Int]) 用（beta5 适配表）
}

type ModelPrice struct {
	Model            string
	InputPer1M       float64 // 每 1M prompt token 单价（美元，FR-018 1:1）
	OutputPer1M      float64
	CachedInputPer1M float64
	PriceVersionID   string  // 自研核心价格版本，回填到账本便于复算（FR-013）
}

type ExecRequest struct {
	Protocol string // "chat_completions" | "responses"（FR-111，一期仅这两种）
	Model    string
	RawBody  []byte // 原样透传（工具调用/多模态字段不解构，FR-111）
	Stream   bool
	// 注意：不落正文（FR-112）——RawBody 仅在内存中转发，不写账本
}

type AttemptStream interface {
	// Recv 返回下一个 SSE 事件（已解析出"是否含用户可见内容"）。
	// executor 用 HasVisibleContent 做内容感知首字判定，绝不用网关首字字段。
	Recv() (StreamEvent, error)
	Close() error // 关闭即向网关传播取消（AC-32）
}

type StreamEvent struct {
	Raw              []byte
	HasVisibleContent bool // 由适配器解析：排除 role-only delta / 空 delta / SSE 注释心跳
	Terminal          bool // finish / [DONE]
}

type QuotaStatus int
const (
	QuotaAvailable QuotaStatus = iota
	QuotaWarning
	QuotaExhausted
	QuotaUnknown // selector 默认按 FR-118 保守排除
	QuotaNotApplicable // 通用 openai/anthropic 渠道无 provider 配额
)

type ReconcileKey struct {
	ExternalRequestIDs []string  // 网关侧 request id（Execute 时回传）
	Since, Until        time.Time // 时间窗兜底关联
}

type AttemptRecord struct {
	ExternalRequestID string
	ChannelGID        string
	Status            AttemptStatus // 已归并口径，见 §5.2
	RawGatewayStatus  string        // 网关原始枚举，存证
	ErrorMessage      string
	ResponseStatus    int
	PromptTokens      int
	CompletionTokens  int
	CachedTokens      int
	TotalCost         float64
	CostPriceRefID    string
	GatewayTTFTMs     int  // 只存证不采信（AC-31）
	HiddenUpstreamAdd int  // 隐藏重试补算出的额外上游调用数（FR-119）
}

type AttemptStatus int
const (
	AttemptCompleted AttemptStatus = iota
	AttemptFailed
	AttemptCanceledByClient // 网关 canceled：客户端/上下文取消
	AttemptCanceledBySLA    // 自研层首字前接管取消（从网关看=客户端断开）
	AttemptUpstreamAborted  // 上游中途断开（网关归 failed，按 errorMessage 拆出）
	AttemptInternalTimeout  // 网关内部首事件超时（网关归 failed）
)
```

---

## 3. 方法 → 网关能力映射总表

| 接口方法 | AxonHub beta5 落点 | ccLoad v3.6.1 落点 | 关键约束 / FR·AC |
| --- | --- | --- | --- |
| `Capabilities` | 静态返回（TTFT=Unsupported、Quota=Degraded、无隐藏重试） | 静态返回（TTFT=Unsupported、Codex 隐藏重试=有） | AC-31、FR-118/119 |
| `ProvisionBinding` | `createAPIKey` + `updateAPIKeyProfiles`(channelIDs) + `updateChannelStatus(enabled)` + 全字段 `updateRetryPolicy{enabled:false}` | 建单 Channel（单 URL/单 Key `sequential`）+ 单 Channel Token allowlist 锁该渠道 | 假设 1；假设 2 补验坑 |
| `SetPricing` | `saveChannelModelPrices`（`usage_per_unit`，每 1M） | ccLoad 无渠道级定价：返回 `Unsupported`，价格由自研核心账本自算 | 假设 4；FR-010/012/013 |
| `ReadQuota` | 查询 `Channel.providerQuotaStatus` | 无 provider 配额概念：返回 `QuotaNotApplicable` | 假设 5；FR-118 |
| `Execute` | `POST /v1/chat/completions`（或 Responses）用该渠道 Key | `POST /v1/...` 用该 Channel Token | FR-111；AC-26/31/32 |
| `Reconcile` | `/admin/graphql` 拉 `requests{executions{} usageLogs{}}` | `GET /admin/logs`（单条记录，隐藏重试须补算） | 假设 2/4；AC-30；FR-119 |
| `Teardown` | 删 API Key + profile | 删 Channel Token + Channel | 01 §6 开放点 3 |

---

## 4. AxonHub `v1.0.0-beta5` 实现

> 全部 GraphQL 端点为 `/admin/graphql`（裸 `/graphql` 被 SPA catch-all 吃掉），系统/鉴权路由带 `/admin` 前缀（beta5 适配表）。ID 为 relay gid，`channelIDs([Int])` 需取 gid 末段数字。

### 4.1 `ProvisionBinding` —— Key-per-Channel（假设 1 + 假设 2 补验坑）

单渠道绑定是整套账本可信的地基：AxonHub 候选选择先按 `profile.ChannelIDs` 白名单过滤，重试循环只在候选集内切换，**锁死单渠道即无法越权回退**（假设 1 源码 + 运行时双证：mock-500 在渠道 A 内重试后报错，从未切渠道 B）。

开通步骤（幂等，全部经 `/admin/graphql`）：

| 步 | mutation / 字段 | 要点 | 依据 |
| --- | --- | --- | --- |
| 1 | `createChannel(...)` | 建渠道，登记 `baseURL` / 上游 Key / `channelType` / 模型 | 假设 1 |
| 2 | `updateChannelStatus(channelId, status: enabled)` | **必做**：渠道建后默认 `disabled`，不启用请求报 `model not found` | beta5 适配表 |
| 3 | `createAPIKey(projectID: <gid>)` | `projectID` 需 gid 形式；每渠道一把 Key | beta5 适配表 |
| 4 | `updateAPIKeyProfiles(apiKeyId, profiles:[{channelIDs:[<num>], active:true}])` | **channelIDs 取 gid 末段数字**（`Channel/1`→`1`）；active profile 只含该单渠道 | 假设 1、beta5 适配表 |
| 5 | `updateRetryPolicy{ enabled:false, streamFirstEventTimeoutSeconds:0, maxChannelRetries:0, maxSingleChannelRetries:0, loadBalancerStrategy:"failover" }` | **系统级、整体替换**：必须全字段下发，只传 `enabled` 会把其余重置为默认（假设 2 补验坑），残留 `maxSingleChannelRetries=2` 会在渠道内隐藏重试污染账本 | 假设 1/2 |
| 6 | 路由就绪轮询 | 渠道启用后异步同步有秒级延迟，须 warmup 轮询避免竞态 `model not found` | beta5 适配表 |

> **为何 retry 必须置零**：假设 1 实测单渠道失败时 AxonHub 默认在**同渠道重试 3 次**并各落一行 execution。自研核心的 executor 自己驱动接管与单次重试（FR-079 默认禁并发重试），网关侧任何自动重试都会让"一次 Attempt = 一行账"的不变式失真。置零后，网关的每行 execution 精确对应自研核心发起的一跳。

`Capabilities()` 返回：`SingleChannelBind=Supported`、`TrustsGatewayTTFT=Unsupported`、`PerAttemptLedger=Supported`、`HiddenRetryRisk={MayHiddenRetry:false}`（retry 置零后）、`QuotaEnforcement=Degraded`。

### 4.2 `SetPricing` —— 使 usageLogs 可财务级对账（假设 4）

`saveChannelModelPrices(channelId, input:[SaveChannelModelPriceInput!])`，每 item：

| 字段 | 值 | 说明 |
| --- | --- | --- |
| `pricing.mode` | `"usage_per_unit"` | 唯一验证过的计价模式 |
| `pricing.usagePerUnit` | 每 **1M** token 单价（美元） | FR-018 各币种 1:1 |
| `itemCode` | `prompt_tokens` / `completion_tokens` / `prompt_cached_tokens` | 缓存 token 走独立折扣价 item |

下发后，成功请求的 `usageLogs` 产出 `totalCost` + `costPriceReferenceID`（指向价格版本），并携 `promptTokens/completionTokens/totalTokens/promptCachedTokens`。假设 4 运行时验算吻合（如 prompt=6/completion=4/cached=2、单价 1/2/0.5 → `totalCost=1.3e-5`；缓存 token 走折扣价）。

> **前提**：财务级对账 = AxonHub 侧配价（本方法）**且**上游回传标准 OpenAI 流式末帧 `usage`。二者齐备缺一不可。失败/取消的请求 `usageLogs` 为空、`externalID` 为空（符合预期，无成功用量）。

自研核心仍持有权威价格版本（FR-012/013）：`PriceVersionID` 随 `AttemptRecord.CostPriceRefID` 交叉存证，历史成本可复算。

### 4.3 `Execute` —— 内容感知首字由 executor 判定（AC-31/32）

- 数据面 `POST /v1/chat/completions` 或 `/v1/responses`，`Authorization` 用该渠道的 AxonHub Key，body 原样透传（FR-111，不落正文 FR-112）。
- 返回 `AttemptStream`。适配器逐 SSE 事件解析 `HasVisibleContent`：**排除 role-only delta、空 delta、SSE 注释心跳**——沿用 AxonHub 源码 `llm/pipeline/empty_response.go:hasResponseContent` 的内容感知定义（该定义 AxonHub 自己只用于重试判断，**未用于对外 TTFT 字段**，故必须由适配器重算）。
- **禁止采信 `metricsFirstTokenLatencyMs`**：三条运行时铁证——mock-normal（role-only 后 sleep 0.5s 发首内容却记 10ms）、mock-empty-sse（零内容仍记 10ms）、mock-heartbeat（记 623ms=心跳后 role delta 时刻）。该字段打点在**首个流事件**而非首个可见内容。适配器把它塞进 `AttemptRecord.GatewayTTFTMs` 仅供存证。
- **取消传播（AC-32）**：`ctx` 取消 / `AttemptStream.Close()` → 断开到 AxonHub 的 HTTP 连接。从 AxonHub 视角这等同客户端断开，落 `canceled`（假设 2 已证实客户端断开→`canceled`）。executor 在动态期限到达或确认接管后调用它止损上游用量。

> **协议实测收口**（[07 §3](./07-axonhub-runtime-probes.md)）：AxonHub inbound `/v1/responses` **原生支持**；但 outbound 打上游原生 Responses **须把渠道配成 `ChannelType="openai_responses"`**——`openai` 型会**静默下转 Chat Completions**（丢 Responses-only 语义）。故 `ProvisionBinding` 对 Responses 渠道必须设 `openai_responses`。且 AxonHub 为**领域模型 round-trip 转译**（重签 item id、丢未知/自定义字段），非字节级透传：标准字段够用无需自研转换；若需严格保真则 `protocol` 层直连，`Capabilities().DegradedFields["responses_passthrough"]` 标注。真实上游保真度待 M1 用真实凭证补验。

### 4.4 `Reconcile` —— 逐尝试对账 + errorMessage 归并 + 隐藏重试补算

拉取（`/admin/graphql`，字段名按 beta5 适配表）：

```graphql
query($ids: [ID!]) {
  requests(where: { externalRequestIDIn: $ids }) {
    edges { node {
      modelID
      executions { edges { node {
        channelID responseStatusCode status errorMessage externalID
        metricsFirstTokenLatencyMs   # 只存证不采信（AC-31）
      } } }
      usageLogs { edges { node {
        promptTokens completionTokens totalTokens promptCachedTokens
        totalCost costPriceReferenceID
      } } }
    } }
  }
}
```

**(a) 取消/失败按 `errorMessage` 归并（AC-30、假设 2）**：beta5 的 `canceled` **专指客户端/上下文取消**；上游中途断开、内部首事件超时都归 `failed`。自研账本要统一"取消"口径**不能只看 status 枚举**，须按 `errorMessage` 拆分：

| 网关 status | errorMessage 特征 | 归并为 `AttemptStatus` |
| --- | --- | --- |
| `canceled` | `...context canceled` | `AttemptCanceledByClient`（若自研层主动取消则记 `AttemptCanceledBySLA`） |
| `failed` | `stream ended without terminal event...` | `AttemptUpstreamAborted` |
| `failed` | `stream first event timeout` | `AttemptInternalTimeout` |
| `failed` | `mock upstream 500` / 其他上游错误 | `AttemptFailed` |
| `completed` | —— | `AttemptCompleted` |

三路中断均逐 attempt 落 execution、以 `channelID` 标注、关联同一 request，可对账（假设 2 运行时三路证实）。

**(b) 隐藏重试补算（FR-119）**：AxonHub 在 §4.1 retry 置零后，**每次上游调用 = 一行 execution**，`HiddenUpstreamAdd=0`，直接按 executions 行数即真实上游调用数（`CountableFrom="executions-rows"`）。ccLoad 的 Codex 隐藏重试是**另一码事**，见 §6.3——本项在 AxonHub 实现里为常态 0，但接口保留该字段以承接退路。

**(c) 关联键**：`externalID`（上游响应 id，如 `chatcmpl-mock-...`）+ `costPriceReferenceID`（价格版本）+ 时间窗兜底。usageLog 挂在对应 Request 下。

### 4.5 `ReadQuota` —— 读但不过滤（FR-118、假设 5）

- 查询 `Channel.providerQuotaStatus` ∈ `available | warning | exhausted | unknown`；通用 `openai`/`anthropic` 渠道该字段 `null` → `QuotaNotApplicable`（provider 配额仅对 `opencode_go`/`claudecode`/`codex`/`github_copilot` 等订阅类渠道生效）。
- **本方法只读取存证，绝不排除任何渠道**。过滤在 `selector`：配额 `unknown` 默认保守排除（FR-118）。
- **为何不依赖 AxonHub 执行**：运行时决定性结论——配额 enforcement 是**系统级、默认 `enabled=false`**；即便显式开 `enabled=true, mode=DE_PRIORITIZE`，`unknown` 配额渠道**仍进候选且可被选中**（保守下限=保留而非排除）。这与 FR-118 "未知即保守排除"相反，故自研层补这层过滤，`Capabilities().QuotaEnforcement=Degraded`。
- **未覆盖**：`available`/`exhausted` 真实态的剔除与降序需真实订阅凭证（mock 无法伪造订阅面板协议），M3 接真实 `opencode_go`/`claudecode` 渠道另验。

### 4.6 `Teardown`

删该渠道的 API Key 与 profile（及必要时 disable channel）。幂等。Key 生命周期由适配器登记到 PG，禁止人工建/删（[01 §6 开放点 3](./01-architecture.md#6-开放点评审需拍板)）。

---

## 5. 错误与降级

### 5.1 分类与处置

| 错误面 | 场景 | 适配器行为 | 上抛给 executor/ledger |
| --- | --- | --- | --- |
| 开通 | `updateChannelStatus` 未生效→`model not found` | warmup 轮询重试；超时报 `ErrBindingNotReady` | selector 暂排除该 Binding |
| 开通 | `updateRetryPolicy` 只传部分字段被重置 | **代码层杜绝**：常量全字段下发，部署脚本单测校验 | —— |
| 配价 | `saveChannelModelPrices` 失败 | 报 `ErrPricingUnset`；该渠道 usageLogs 无 cost | ledger 按自研核心价格版本自算成本（不阻塞执行） |
| 执行 | 上游 500 / 断流 | AttemptStream 返回 error，落 execution `failed` | executor 按 RoutePlan 切下一 Binding |
| 执行 | 首字前期限到 | executor 主动 `Close()`→`canceled` | 计 `AttemptCanceledBySLA`，止损上游 |
| 对账 | GraphQL 字段名因升级变更 | 报 `ErrSchemaMismatch` + 字段名 | 触发 verify/ 准入检查（硬约束 11） |
| 配额 | 探测未就绪 `ready:false` | 返回 `QuotaUnknown` | selector 保守排除（FR-118） |
| 网关整体不可用 | AxonHub 进程宕 | 全方法快速失败 | 核心返回明确不可用，**禁旁路直连上游**（FR-110） |

### 5.2 降级字段清单（`Capabilities().DegradedFields`）

| 字段 | 降级原因 | 自研层补偿 |
| --- | --- | --- |
| `metricsFirstTokenLatencyMs` | 打点在首个流事件，非可见内容（AC-31） | executor 内容感知 TTFT 自算 |
| `providerQuotaStatus=unknown` | 网关默认"保留"而非排除（FR-118） | selector 保守排除 |
| `status` 枚举 | `canceled` 仅指客户端取消（AC-30） | ledger 按 errorMessage 归并 |
| `responses_passthrough` | 领域模型 round-trip 丢未知字段/重签 item id（[07 §3](./07-axonhub-runtime-probes.md) 实测）；误配 `openai` 型会静默下转 CC | 严格保真时 protocol 层直连；Responses 渠道强制配 `openai_responses` |

---

## 6. ccLoad `v3.6.1` 退路空壳

> 假设 6 已证实 ccLoad 单资源拓扑**资源绑定稳固**（会话粘性 / Key 轮换 / URL 轮换零破坏，12 次探测 1:1），可作 AxonHub 不可用预案（[01 §6 开放点 1](./01-architecture.md#6-开放点评审需拍板)）。空壳实现同接口，仅在预案启用时接管。

### 6.1 拓扑映射（单资源，L0）

| GatewayAdapter | ccLoad 落点 | 约束 |
| --- | --- | --- |
| `ProvisionBinding` | 单 Channel（**单 URL、单 Key、`sequential`**）+ **单 Channel Token allowlist 限定该渠道** | Channel Token 只放行单渠道 → 无越权切换 |
| `SetPricing` | 无渠道级定价 → 返回 `Unsupported` | ledger 全额自算成本 |
| `ReadQuota` | 无 provider 配额 → `QuotaNotApplicable` | —— |
| `Execute` | `POST /v1/...` 用 Channel Token | 同样自算 TTFT |
| `Reconcile` | `GET /admin/logs` | 单条记录 + 隐藏重试补算（§6.3） |
| `Teardown` | 删 Channel Token + Channel | —— |

`Capabilities()`：`SingleChannelBind=Supported`、`TrustsGatewayTTFT=Unsupported`、`CancelPropagation=Supported`（§6.2）、`HiddenRetryRisk={MayHiddenRetry:true, Triggers:["codex+400+reasoning-rewrite"], CountableFrom:"adapter-inference"}`。

### 6.2 取消传播（AC-32，假设 6 补验）

ccLoad 把首字后的 **mid-stream 取消即时传播到上游**并主动拆连接止损：改造探测（客户端收 3 chunk 后硬 RST）实测 mock 侧下一 chunk 写入即 `EPIPE`、停在中途；ccLoad `admin/logs` 记 status `499`、`context canceled`、`duration≈客户端断连时刻`。→ `Execute` 的 `Close()` 语义在 ccLoad 上成立。

### 6.3 隐藏重试补算（FR-119）—— ccLoad 的独有坑

ccLoad Codex 渠道遇 **400 + 错误体提及 reasoning/thinking** 触发 `strip_codex_thinking`：删 `reasoning` 字段**在同一 Key/URL 上重发**（`proxy_forward.go:1559`）。实测一次外部请求 → **2 次上游 POST**（400→删 reasoning→200），但 `admin/logs` **只留一条 200 记录**（`message="ok [strip_codex_thinking]"`），首个 400 被吸收、无独立用量/费用记录。

→ **"一次外部调用 = 一次上游调用"的不变式在 Codex 渠道被打破**（变 2 次），且单资源绑定未破（严格落回同一 Key/URL，不外溢）。`Reconcile` 对 `channelType="codex"` 的 ccLoad 渠道必须：

1. 识别 `message` 含 `strip_codex_thinking` 标记 → `HiddenUpstreamAdd=1`（`CountableFrom="adapter-inference"`）。
2. 按网关行为补算被吸收的那次 400 上游调用的用量与成本（ccLoad 无渠道级 `no_retry`，无法从源头关闭）。

AxonHub 侧无此坑（retry 置零 + 逐 execution 行），故该补偿只在 ccLoad 实现内生效。

---

## 7. 时序：一次带首字前接管的流式请求

```
1 selector 产出 RoutePlan [B1(期限5s), B2(期限8s)]        # 内存快照决策，配额 unknown 已排除(FR-118)
2 executor → Execute(ctx, B1, req)                        # AxonHub: 渠道A Key 调 /v1/chat/completions
3 AttemptStream.Recv 逐事件；HasVisibleContent 全为 false  # role-only / 心跳不算首字(AC-31)
4 5s 期限到未见可见内容 → AttemptStream.Close()            # 传播取消→AxonHub 拆上游连接止损(AC-32/假设6)
5 executor → Execute(ctx, B2, req)                        # 起 B2
6 收到首个 HasVisibleContent=true → 提交响应头+缓冲        # 此后只透传，不再切换(FR-078)
7 关单：ledger 落 Attempt#1(CanceledBySLA)、Attempt#2(Completed)
8 Reconcile(ExternalRequestIDs) 异步补 AxonHub 侧          # executions(逐行) + usageLogs(cost/token)
9   → errorMessage 归并(AC-30)、TTFT 自算不采信网关字段(AC-31)、隐藏重试补算 HiddenUpstreamAdd(FR-119)
```

---

## 8. FR / AC 覆盖对照

| 契约要素 | FR / AC | 运行时依据 |
| --- | --- | --- |
| 单渠道绑定（Key-per-Channel、channelIDs 取末段数字） | FR-002/004、AC-01 | 假设 1（源码+运行时双证） |
| AxonHub retry 置零、全字段整体替换 | FR-079 | 假设 2 补验坑 |
| 逐尝试账本可对账、externalID/价格版本关联 | FR-097/098、AC-16 | 假设 4 |
| SetPricing→usageLogs 产 totalCost+costPriceReferenceID | FR-010/012/013、AC-02 | 假设 4 验算吻合 |
| 内容感知 TTFT、不采信网关首字字段 | AC-31、FR-040/050 | 假设 3 三铁证、假设 6 同构短板 |
| 取消传播止损、Close() 语义 | AC-32、FR-080 | 假设 6 补验 mid-stream 取消 |
| errorMessage 归并对账（canceled 仅客户端取消） | AC-30 | 假设 2 三路语义澄清 |
| 配额 unknown 读但不过滤，selector 保守排除 | FR-118 | 假设 5 决定性结论 |
| 隐藏重试补算（Codex 400 body-rewrite） | FR-119 | 假设 6 补验 Codex 隐藏重试 |
| 显式能力声明 + 降级字段（Unsupported/Degraded） | FR-006、FR-115 | ISSUE-002 §2 原则 |
| OpenAI CC/Responses 透传、不落正文 | FR-111/112、AC-26 | 硬约束 2/3 |
| 网关不可用禁旁路直连 | FR-110、AC-27 | 01 §6 开放点 1 |
| ccLoad 退路同接口空壳（单资源拓扑、Channel Token allowlist） | FR-110 退路 | 假设 6 单资源稳固 |

---

## 9. 实现待办（M1 前）

1. `updateRetryPolicy` 全字段常量 + 部署脚本单测（防假设 2 坑重现）。
2. Responses 协议透传 M1 补一轮 verify/ 式 mock 探测（开放点 2）。
3. Binding/Key 生命周期登记 PG 的表结构 → 见 [02 数据模型](./02-data-model.md)（`bindings` 与 `upstream_keys.axonhub_api_key_id` / `channels.axonhub_channel_id`）。
4. GraphQL 客户端按 beta5 适配表生成；每次升级 AxonHub 先跑 verify/（硬约束 11）。
5. ccLoad 空壳先落 `Capabilities()` 与 `ProvisionBinding`/`Reconcile` 骨架，其余方法返回 `ErrFallbackNotEnabled`，仅预案演练时点亮。
