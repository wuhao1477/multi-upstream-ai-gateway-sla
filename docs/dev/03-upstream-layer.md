# 03 上游对接层（自研直连 · v0.2）

| 项目 | 内容 |
| --- | --- |
| 状态 | 草案，待评审（M1 前定稿） |
| 日期 | 2026-07-25（v0.2：按 [11 转向决策](./11-decision-full-selfbuilt.md) 由《GatewayAdapter 契约》重写为自研直连） |
| 定位 | 自研核心与**真实上游**之间的执行面：请求转发、流式处理、取消传播、用量提取。**不再经任何外部网关** |
| 核心取向 | **Responses 走字节级透传（硬约束级）** —— 依据 [13 §5](./13-research-reassessment.md)：三个独立项目走解析-重组路线全部在 Codex 兼容性上翻车 |
| 输入 | [11 转向决策](./11-decision-full-selfbuilt.md)、[13 调研重审](./13-research-reassessment.md)、[02 数据模型](./02-data-model.md)、[05 调度](./05-scheduling-and-operations.md)、[12 可调试性](./12-debuggability.md) |
| 前身 | 原《03 GatewayAdapter 契约》（AxonHub/ccLoad 适配）已废止；其运行时结论见 [ISSUE-001](../issues/ISSUE-001-tech-assumption-verification.md)、[07](./07-axonhub-runtime-probes.md) |

---

## 1. 设计原则

| # | 原则 | 依据 |
| --- | --- | --- |
| 1 | **字节级透传优先**：Responses/CC 的响应体原样回送，不解析进领域模型再重发 | [13 §5](./13-research-reassessment.md) 三例翻车 |
| 2 | **旁路观察而非接管**：内容感知 TTFT（AC-31）、usage 提取都在**旁路**做，不改变字节流 | AC-31 |
| 3 | **一次外部调用 = 一次上游调用**：不做隐藏重试；重试由 `executor` 按 RoutePlan 显式发起并各落一条 attempt | FR-119、[02 §4](./02-data-model.md) |
| 4 | **响应头白名单透传**：上游响应头默认透传，仅剔除 hop-by-hop 与我方要覆盖的 | [13 §1.3](./13-research-reassessment.md) |
| 5 | **取消必须传播到上游**：下游断开或期限到达 → 关闭上游连接止损 | AC-32 |
| 6 | 不落正文：转发在内存中完成，账本只存元数据 | FR-112 |

---

## 2. Go 接口

```go
package upstream

// Client 是自研核心与真实上游之间的唯一执行契约。
// 一个 Binding（渠道+Key+URL，见 02 bindings 表）对应一次可执行的上游调用。
type Client interface {
    // Do 沿单个 Binding 执行一次 Attempt（一跳）。
    // 流式：返回 Stream，字节级透传给下游，同时旁路产出观察事件。
    // ctx 取消 → 关闭上游连接（AC-32）。
    Do(ctx context.Context, b Binding, req *Request) (*Response, error)

    // Probe 探测某 Binding 对某模型支持哪些协议端点。
    // 实测发现：同一站点部分模型仅支持 Responses，CC 返回 503（13 §3）。
    Probe(ctx context.Context, b Binding, model string) (ProtocolSupport, error)

    // Models 透传上游模型目录（Codex 靠 GET /v1/models 刷新，13 §1.3）。
    Models(ctx context.Context, b Binding) ([]byte, http.Header, error)
}

type Request struct {
    Protocol   Protocol // ProtocolChatCompletions | ProtocolResponses
    Model      string
    RawBody    []byte      // 原样透传（不解构工具/多模态字段，FR-111）
    Headers    http.Header // 调用方请求头（经白名单过滤后转发）
    Stream     bool
}

type Response struct {
    Status  int
    Headers http.Header // 上游响应头（白名单透传，含 x-codex-* 等）
    Body    io.ReadCloser // 非流式：完整体
    Stream  Stream        // 流式：字节流 + 旁路观察
}

// Stream 是字节级透传的流。Read 出来的字节原样写给下游；
// Observe 返回旁路观察通道，供 executor 做首字判定与 usage 提取。
type Stream interface {
    io.ReadCloser              // 字节级透传；Close 即向上游传播取消（AC-32）
    Observe() <-chan Observation
}

// Observation 是旁路观察事件——只含元数据，不含正文（FR-112、12 L1 层）
type Observation struct {
    Seq            int           // 事件序号
    OffsetMs       int           // 相对请求发出的到达偏移
    EventType      string        // SSE event 名，如 response.output_text.delta
    HasVisibleText bool          // 是否含用户可见内容 → 内容感知首字判定（AC-31）
    Bytes          int           // 该事件字节数
    Usage          *UsageSnapshot // 仅终帧携带
}

type ProtocolSupport struct {
    ChatCompletions bool
    Responses       bool
}
```

**与旧 GatewayAdapter 的差别**：不再有 `ProvisionBinding`/`SetPricing`/`Reconcile`/`Teardown` —— 渠道与 Key 直接来自 [02](./02-data-model.md) 的 `channels`/`upstream_keys`（一期明文，FR-113），无需向外部网关开通；账本是**单一真相源**，无对账环节。

---

## 3. 流式处理：字节透传 + 旁路观察

```
上游 SSE 字节 ──┬──> 原样写给下游客户端（零改动）
                └──> tee 到 SSE 扫描器 ──> Observation 通道
                                            ├─ HasVisibleText → executor 判定内容感知首字（AC-31）
                                            ├─ 事件轨迹 → 12 的 L1 环形缓冲
                                            └─ 终帧 usage → 账本（02 attempt_usage）
```

### 3.1 SSE 扫描器（只读不改）

解析要点（借鉴 [zhfeng1](./08-ref-eval-zhfeng1-ai-gateway.md) 的实现思路，非代码）：

- **换行归一化**：真实上游 `\r\n` 与 `\n` 混用，须先归一再按 `\n\n` 分块，否则切错事件
- **多行 `data:` 拼接**：SSE 规范允许一个事件多行 `data:`，须 `\n` 拼接后再解析
- **`[DONE]` 与注释行**：`data: [DONE]` 与 `:` 开头的心跳注释**不计入可见内容**

### 3.2 内容感知首字判定（AC-31）

`HasVisibleText` 的判定标准（对齐 [ISSUE-001 假设3](../issues/ISSUE-001-tech-assumption-verification.md) 的三条铁证）：

| 事件 | 是否算首字 |
| --- | --- |
| SSE 注释心跳（`: heartbeat`） | ❌ |
| `response.created` / `in_progress` / `output_item.added` / `content_part.added` | ❌ 元事件 |
| role-only delta（CC）/ 空 delta | ❌ |
| `response.output_text.delta` 且 text 非空 | ✅ **首字** |
| CC 的 `choices[].delta.content` 非空 | ✅ **首字** |

> 实现参考：AxonHub 的 `llm/pipeline/empty_response.go:hasResponseContent` 内容感知逻辑写得是对的（[13 §2.1](./13-research-reassessment.md)），可借鉴其判定思路。

---

## 4. Codex 兼容硬约束（[13 §1](./13-research-reassessment.md)）

主力客户端是 Codex CLI，以下**不是建议而是契约**：

| # | 约束 | 我们的处置 |
| --- | --- | --- |
| 1 | **SSE 事件生命周期必须完整**（`created→in_progress→output_item.added→content_part.added→delta…→completed`） | **字节透传天然满足** —— 上游发什么原样到达。**这正是不做重组的核心理由**（LiteLLM 漏 4 个事件致 Codex 报 `OutputTextDelta without active item`） |
| 2 | `function_call.status` 须为 `in_progress` 而非 `completed` | 透传不改 → 天然满足 |
| 3 | 响应头 `x-codex-*`（限流快照）、`x-models-etag`、**`x-codex-turn-state`（会话粘性令牌，须跨轮回放）**、request id | **白名单透传**（§5） |
| 4 | `GET /v1/models` 全量透传 | `Client.Models()` |

---

## 5. 头部处理

| 方向 | 规则 |
| --- | --- |
| 请求头（下游→上游） | 剔除 hop-by-hop（`Connection`/`Transfer-Encoding`/`Keep-Alive` 等）；**覆盖** `Authorization` 为该 Binding 的上游 Key；保留 `session_id`/`conversation_id`/`X-Claude-Code-Session-Id` 等会话标识（[02 §4.5](./02-data-model.md) 提取后仍原样转发） |
| 响应头（上游→下游） | **默认透传**，仅剔除 hop-by-hop 与 `Content-Length`（流式重算）；`x-codex-*`/`x-models-etag`/`x-codex-turn-state` **必须保留**（§4） |
| 日志/抓包 | `Authorization` 等凭证头**永不落盘**（[12 §6](./12-debuggability.md)） |

---

## 6. 取消与超时（[13 §4](./13-research-reassessment.md) 的风险 4~6）

| 场景 | 处置 |
| --- | --- |
| 期限到达（首字前接管） | `Stream.Close()` → 关闭上游连接；attempt 记 `canceled_by_sla`、`cancel_propagated=true`（AC-32） |
| 下游客户端断开 | 检测到下游写失败 → 立即关上游连接止损；记 `client_disconnect`（[02](./02-data-model.md) `cancel_reason`） |
| **流式空闲超时** | 上游读取停滞超阈值（默认 120s，可配）→ 主动断开，防连接被永久占住 |
| **连接池楔死** | 上游连接卡死会饿死后续请求 → 每个 Binding 独立连接池 + 健康检测；连续超时触发**连接池轮换**（新建一代 client），不等重启 |
| 上游中途断流 | 记 `upstream_disconnect`（无终帧）；已提交内容不拼接第二个响应（FR-078） |

---

## 7. 用量与成本

- **usage 来自旁路观察的终帧**（Responses 的 `response.completed` / CC 的末帧 `usage`），落 [02 `attempt_usage`](./02-data-model.md)。
- 上游不回 usage 时，按 [02 `price_versions`](./02-data-model.md) 的价格版本 + token 估算，`cost_source='estimated'`。
- **无对账环节**：自研账本即唯一真相源（转向后不再有外部网关账本需要比对）。

---

## 8. 协议能力探测（[13 §3](./13-research-reassessment.md) 实测发现）

真实上游实测：同一 NewAPI 站点，`gpt-5.5` 的 **Responses 返回 200 而 CC 返回 503** —— **不能假设两个协议都通**。

- `Probe()` 在渠道接入时与定期健康检查时探测每个模型的 `ProtocolSupport`，落 [02 `channel_models`](./02-data-model.md)。
- `selector` 候选过滤时**按请求协议筛**：请求走 Responses 就只选支持 Responses 的 Binding（[05 §1.1](./05-scheduling-and-operations.md) 序 2 模型能力层）。

---

## 9. FR / AC 覆盖

| 契约要素 | FR / AC |
| --- | --- |
| OpenAI CC + Responses 透传、工具/多模态原样 | FR-111、AC-26 |
| 内容感知 TTFT 自算、不采信任何上游首字字段 | AC-31 |
| 取消传播止损 | AC-32、FR-080 |
| 一次外部调用 = 一次上游调用（无隐藏重试） | FR-119 |
| 逐 attempt 账本、usage/成本 | FR-097/098、FR-058 |
| 不落正文 | FR-112 |
| 上游不可用返回明确错误、禁旁路 | FR-110、AC-27 |

---

## 10. 实现待办（M1）

1. SSE 扫描器 + `HasVisibleText` 判定，用 `verify/mock_upstream.py` 的既有场景（role-only / 空 SSE / 心跳 / 慢首帧 / abort）做夹具（[11 §3](./11-decision-full-selfbuilt.md)）。
2. 字节透传管道 + tee 旁路观察，验证 35 字段与 reasoning item **零丢失**（对照 [07 §3bis](./07-axonhub-runtime-probes.md) 的真实上游基线）。
3. 取消传播与连接池轮换的集成测试。
4. `Probe()` 的协议能力探测 + 落库。

---

_本篇取代原《03 GatewayAdapter 契约》。核心变化：不再适配外部网关，改为自研直连；**Responses 字节级透传为硬约束**，内容感知 TTFT 与 usage 提取全部走旁路观察。_
