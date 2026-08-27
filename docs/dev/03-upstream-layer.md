# 03 上游对接层（自研直连 · v0.2）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ **v1.0 基线（2026-07-26 冻结）** —— 经 42 轮对抗性审查（含 5 轮开发视角）+ PM 开工前裁决；变更须走版本记录 |
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
| 1bis | **请求体只允许一处改写：顶层 `model` 字段**（别名 → 该渠道的上游模型名）。其余字节一字不动，按字节区间替换、`Content-Length` 重算，**不得 Unmarshal→Marshal**。规则与理由见 [05 §1.0](./05-scheduling-and-operations.md) | 第 40 轮：原则 1 只约束了**响应**侧，请求侧要不要改写从未表态，而不改写必然 404 |
| 2 | **旁路观察而非接管**：内容感知 TTFT（AC-31）、usage 提取都在**旁路**做，不改变字节流 | AC-31 |
| 3 | **一次外部调用 = 一次上游调用**：不做隐藏重试；重试由 `executor` 按 RoutePlan 显式发起并各落一条 attempt | FR-119、[02 §4](./02-data-model.md) |
| 3bis | **串行执行，禁并发重试**：`executor` 在同一 request 内**任一时刻只有一跳在飞**——上一跳必须先 `closeout_attempt`（终态 + 费用行 + 释放 claim）才允许 `dispatch_next`（冻结顺序见 [02 §2bis「接管的执行顺序被冻结为」](./02-data-model.md)）。**不提供任何并发/影子请求开关**；有副作用的请求（支付、发送、写入）在任何配置下都不得自动并发或盲目重试 | **FR-079**、AC-13、参数 6 |
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

    // Models 读取上游真实模型目录。⚠️ **不对外暴露**（第 31 轮：/v1/models 已改为
    // 由网关按别名表合成，见 §4 第 4 条）——本方法降级为两个内部用途：
    //   ① 协议能力探测的输入；② 管理面比对"上游真实目录 vs 我方登记"是否漂移。
    Models(ctx context.Context, b Binding) ([]byte, http.Header, error)
}

// Binding 是执行一跳所需的全部**已解析**信息（第 40 轮补：此前 Client 四个方法都收它，
// 却从没定义过这个类型 —— 开发得自己发明字段，尤其"请求发到哪个 URL"无处可取）。
// 由 selector 从 02 bindings 行 + 关联表装配，executor 不再查库。
type Binding struct {
    ID           int64  // bindings.id → attempts.binding_id
    BaseURL      string // bindings.effective_url —— **实际发往的上游地址**，一 binding 一个
    APIKey       string // upstream_keys.secret（一期明文，FR-113）；覆盖 Authorization 头（§7）
    UpstreamModel string // = COALESCE(channel_models.upstream_model_name, models.canonical_name)
    Protocol     Protocol // 该 binding 在本协议下 support='supported'（05 §1.1 序 2 已保证）
}

type Request struct {
    Protocol   Protocol // ProtocolChatCompletions | ProtocolResponses
    // ⚠️ 已改写为**上游模型名**（COALESCE(channel_models.upstream_model_name, models.canonical_name)），
    //    不是调用方发来的别名。改写规则见 05 §1.0；每跳按该跳 binding 重算，不得复用上一跳的值。
    Model      string
    // 原样透传（不解构工具/多模态字段，FR-111）。**唯一例外**：顶层 model 字段已按上行
    //    做过字节区间替换（原则 1bis）。除此之外与调用方发来的字节完全一致。
    RawBody    []byte
    Headers    http.Header // 调用方请求头（经白名单过滤后转发）
    Stream     bool
}

type Response struct {
    Status  int
    Headers http.Header // 上游响应头（白名单透传，含 x-codex-* 等）
    Body    io.ReadCloser // 非流式：完整体
    Stream  Stream        // 流式：字节流 + 旁路观察
}

// ⚠️ Stream 不能是「io.ReadCloser + 旁路 channel」（开发视角审查第 28 轮 [P0]）：
//    两者之间没有任何同步点，无法表达「首字/终帧先落库再放行字节」——
//    Read() 一返回字节就已经在去下游的路上了，旁路 channel 拦不住它。
//
// 故改为**帧驱动**：扫描器切出帧并附带原始字节，由 executor 决定何时放行。
// 字节内容仍一字节不改（透传硬约束），只是**放行时机**由 executor 掌握。
type Stream interface {
    // NextFrame 阻塞返回下一帧（含原始字节 + 旁路判定）。io.EOF 表示上游流结束。
    NextFrame(ctx context.Context) (Frame, error)
    // Close 向上游传播取消（AC-32）
    Close() error
}

// Frame = 一个 SSE 事件的原始字节 + 对它的旁路判定。二者同时到达，executor 才能原子决策。
type Frame struct {
    Raw   []byte      // 该事件的**原始字节**（含 event:/data: 行与结尾空行），原样转发
    Obs   Observation // 对该帧的判定
}

// executor 的放行循环（伪码，说明同步边界）：
//   for {
//       f, err := stream.NextFrame(ctx)
//       switch {
//       case f.Obs.IsFirstCommit:   // 首次 ShouldCommit
//           if err := ledger.CommitFirstActionable(...); err != nil { abortDownstream(); return }
//           flushBufferedAndWrite(f.Raw)      // ← 落库成功后才放行（含此前缓冲的帧）
//       case f.Obs.IsTerminal:
//           if err := ledger.FinalizeUpstream(...); err != nil { abortDownstream(); return }
//           write(f.Raw); closeDownstream()   // ← 落库成功后才放行终帧
//           go ledger.EnqueueDeliveryEvent(...)  // socket write 返回后，异步经 outbox
//       default:
//           if committed { write(f.Raw) } else { buffer(f.Raw) }  // 未提交前只缓冲（T2 上限见 §6）
//       }
//   }

// Observation 是旁路观察事件——只含元数据，不含正文（FR-112、12 L1 层）
type Observation struct {
    Seq            int           // 事件序号
    OffsetMs       int           // 相对请求发出的到达偏移
    EventType      string        // SSE event 名，如 response.output_text.delta
    ShouldCommit   bool          // 已产出确定结果 → 提交并停止接管（AC-32，§3.2）
    HasTTFTOutput  bool          // 产出了实际内容 → 打 content_aware_ttft_ms（AC-31，§3.2）
                                 // ⚠️ 两者独立：空终态 ShouldCommit=true 而 HasTTFTOutput=false
    IsFirstCommit  bool          // 本帧是**首次** ShouldCommit（扫描器维护，executor 不自己判重）
    IsTerminal     bool          // 本帧是终帧
    TerminalKind   string        // 'completed'|'empty_completed'|'error'|'incomplete'；非终帧为空
    Bytes          int           // 该事件字节数
    Usage          *UsageSnapshot // 仅终帧或 usage chunk 携带（见 §3.3 到达顺序）
}

// UsageSnapshot：上游用量的归一化形态（第 28 轮 [P0]：此前被接口引用但从未定义）
type UsageSnapshot struct {
    PromptTokens          int
    CompletionTokens      int
    TotalTokens           int
    PromptCachedTokens    int  // 命中缓存的输入 token
    CacheablePromptTokens int  // **可缓存**的输入 token（FR-056 命中率的分母，见 §3.4）
    ReasoningTokens       int  // Responses 的推理 token；Chat 无此概念时为 0
    Source                string // 'upstream'（上游回传）| 'estimated'（上游未回，按价格版本估算）
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
上游 SSE 字节 ──> 扫描器切帧 ──> Frame{Raw, Obs} ──> executor 放行循环
                                                      ├─ 未提交：缓冲（T2 上限 §6）
                                                      ├─ 首帧 ShouldCommit：先落库 → 再放行缓冲+本帧
                                                      ├─ 中间 delta：直接放行（无同步写）
                                                      └─ 终帧：先 finalize_upstream → 再放行 → 关流
                                          旁路副产物 ├─ 事件轨迹 → 12 的 L1 环形缓冲
                                                      └─ usage → 账本（02 attempt_usage）
```

> ⚠️ **不是「一路直通 + tee 旁路」**（第 28 轮 [P0]）：那种结构下字节一经 `Read()` 就已在去下游的路上，旁路拦不住它，"先落库再放行"无从实现。字节内容仍**一字节不改**，改的只是**放行时机由 executor 掌握**。

### 3.0 提交顺序（**首字与终帧都必须先落库再放行字节**）

> ⚠️ 第 9 轮 [high]：字节流是**直接**写给下游的，而 `terminal_event`/usage 走旁路**之后**才落库。若进程在"客户端已收到完成终帧、outbox 尚未写入"之间崩溃，库里 `terminal_event IS NULL` → 恢复扫描走 [02 §4.2bis](./02-data-model.md) 的 ③b，把一个**实际已正常完成**的请求记成 `interrupted` + 估算费用 + 人工核对。客户端和账本对同一次请求的结论相反。

**两个时点的顺序被冻结**（首字同理，见 [01 §5.1](./01-architecture.md)——否则"用户看到半截输出、库里还是 pending"）：

```
旁路首次 ShouldCommit → ① 同步落库（attempt_status='committed'、response_committed_at、
                            has_ttft_output、content_aware_ttft_ms）
                     → ② 才放行响应头 + 已缓冲字节
```


```
旁路识别出终帧 → ① 同步执行 finalize_upstream 事务（**直写表**：attempts.terminal_event
                     + attempt_usage + reservation 结算 + attempt 终态；requests 保持 pending）
              → ② 才把终帧字节转发给下游 / 关闭下游流
              → ③ socket write 返回后，经 outbox 异步记 downstream_write_completed
```

- **仍满足字节透传硬约束**：字节内容**一字节不改**，只是放行前各多一次同步提交。
- **一次请求恰好两次同步写**（第 21 轮 [high] 修正——上一版残留"只对终帧生效""TTFT 不受影响"，与本页 §3.0 开头及 [01 §5.1](./01-architecture.md)、[14 判定口径](./14-acceptance-matrix.md) 要求的首字同步写直接冲突，照旧文实现会跳过首字同步写、破坏 AC-35 的 K2/K3 判定）：
  **首字放行前一次、终帧放行前一次；中间的 delta 帧一律直通，不引入任何同步写。**
  两次同步写的延迟由 `downstream_ttft_delay_ms` / `downstream_finish_delay_ms` 单独度量，阈值由 M4 压测冻结（[14](./14-acceptance-matrix.md)）——**不得**声称"对 TTFT 无影响"，首字那一次本来就在 TTFT 路径上。
- **写库失败时：不放行终帧，中断下游流，按 ③b2 `interrupted` 结算。**
- ⚠️ ① 是**直写表**不是写 outbox（[02 §2bis 写入路径分工](./02-data-model.md)）：outbox 多一跳投递，满足不了「先落库再放行字节」的顺序。outbox 只承载 ③ 这类字节写出**之后**才产生的异步事实。
  > ⚠️ 第 10 轮 [critical]：上一版写的是"写库失败仍照常转发终帧给下游（用户体验优先），退化为 ③b"——那会造成**客户端收到完整成功响应、账本却记为中断**的永久分裂，而且与 AC-35 要求同一窗口恢复为 `completed` 直接打架。**同一个失败必须只有一种语义。**
  >
  > **取舍（明示）**：这条规则的代价是——上游已经成功且**钱已经花了**，却因为我们的库写不进去而让用户拿不到结果。接受这个代价的理由是：账本是唯一真相源，**无法持久化就无法诚实地声称成功**；而且 outbox 写的是与全站同一个 PG，它不可用时我们本来就在全面失败，不存在"只有这一个请求受影响"的情形。

**两个独立事实，不可互相推导**（第 11 轮 [critical] 修正）：

> ⚠️ 上一版声称"客户端拿到完整响应 ⟺ 库中有终帧证据"——**这个等价物理上做不到**：数据库提交与 socket 写出无法原子化。冻结顺序只消除了**一个方向**（客户端有、库里没有）；反方向的窗口**必然存在且不可消除**——outbox 已提交、终帧字节尚未写出时崩溃，库里看到 `terminal_event IS NOT NULL` 会判 `completed`，而客户端实际收到的是**截断流**。首字处同理。
>
> 正确做法不是假装窗口不存在，而是**把"上游结果已知"与"下游交付已确认"建模成两个独立事实**，在二者不一致时**取保守解释**。

```sql
-- attempts 增两列：**本进程下游写入完成**（socket write 返回后异步经 outbox 落库）
downstream_first_byte_written_at TIMESTAMPTZ,   -- 首字节已写出本进程的下游连接
downstream_write_completed_at    TIMESTAMPTZ,   -- 终帧已写出 / 下游流正常关闭
```

> ⚠️ **这两列证明的是「我们写出去了」，不是「客户端收到了」**（第 12 轮 [critical] 修正）：`sla-core` 的下游对端是 **Caddy**，不是 Codex 客户端。socket write 返回只说明字节被本机内核／Caddy 接受；此后 Caddy、主机或网络仍可能失败，客户端照样拿到截断流。**上一版把它叫作「交付确认」并据此声称用户侧 SLA 诚实性，是第二次过度声称。**
>
> **端到端交付确认在当前拓扑下无法实现** —— 那需要客户端回一个应用层 ack，而主力客户端是 Codex CLI，**我们不能要求它配合**（与 [02 §4.5 会话标识](./02-data-model.md) 同一条纪律）。
>
> **故本设计能诚实声称的最强结论是**：`completed` = **我们已成功把完整响应写出到下游连接**。「客户端未收到」这一残余窗口**已知、不可观测、明确接受**，不写进任何保证里。

- 这两列**不参与计费**（成本只看上游事实），因此可以异步落库、允许滞后。
- **不一致时一律按「未写出 = 未完成」处理**：宁可把一次实际成功的响应记为失败，也不能把我们没写完的流记为成功。

> ⚠️ **`IS NULL` 只意味着「未确认写出」，不等于「下游一个字节都没收到」**（第 13 轮 [high]，同族过度声称第三次）：这两列是 socket write **返回之后异步**落库的，因此存在第三个残余窗口——**write 已返回、时间戳尚未持久化**时崩溃，DB 里是 NULL，而 Caddy 可能早已收到字节。
>
> 故：**数据库事实 ≠ 物理事实**。恢复只能依据数据库事实做**保守**判定（未确认即按未写出处理），**不得反过来声称物理上没写出去**。AC-35 必须**分别断言**「数据库里的终态」与「下游实际观测到的字节」，不允许由前者推导后者。

**四个 kill 时点的完整终态**（AC-12/AC-35 据此写判定）：

| # | kill 时点 | 下游（Caddy）**可能**收到 | 库中事实 | attempt | request（用户 SLA） | 计费 |
| --- | --- | --- | --- | --- | --- | --- |
| K1 | 首字同步写 **提交前** | 无内容 | `pending` | `unknown_billing` | `failed` | 估算 + 待核对 |
| K2 | 首字已提交、**写出未确认** | 通常无内容（**但 write 可能已返回**） | `committed`，`downstream_first_byte_written_at IS NULL` | `interrupted` | `failed`（**未确认写出** → 保守，不计流中断） | 估算 + 待核对 |
| K3 | `finalize_upstream` **提交前**、已确认写出过首字节 | 截断流 | `committed`，`terminal_event IS NULL`，`downstream_first_byte_written_at NOT NULL` | `interrupted` | `interrupted`（计流中断） | 估算 + 待核对 |
| K4 | `finalize_upstream` 已提交、**字节未写出** | 截断流 | `terminal_event NOT NULL`，`downstream_write_completed_at IS NULL` | **由 `terminal_event` 决定**（`completed`/`empty_completed` → `completed`；`error`/`incomplete` → `failed`） | **`interrupted`**（我们没写完 → 保守判失败、计流中断） | **实际用量，无需人工核对** |
| — | 全部完成 | 完整响应 | `terminal_event` + `downstream_write_completed_at` 均非空 | `completed` | `completed` | 实际 |

> **K4 是这次修正的核心**：attempt 层按上游终帧如实记（我们确实知道上游干了什么、花了多少钱），request 层记 `interrupted`（我们**没把它写完**）。两层结论不同不是矛盾，而是两个不同事实的如实记录。
>
> ⚠️ **attempt 终态永远由 `terminal_event` 决定**（第 12 轮 [high]）：上一版 K4 无条件写 `completed`，会在「上游返回 error 终帧、恰好我们没写完」时把**上游失败记成渠道成功**，污染 binding 成功率、冷却与路由选择。下游是否写完**只决定 request 的终态**，不得影响 attempt 的成败归因。

### 3.1 SSE 扫描器（只读不改）

**逐协议的解析优先级与终帧判定**（第 28 轮 [P1]：此前只有双判定表，没说"怎么认出这是哪种事件"，开发只能猜）：

| 项 | Responses | Chat Completions |
| --- | --- | --- |
| **事件类型以谁为准** | **JSON body 的 `type` 字段为准**，`event:` 行仅作校验。理由：实测部分中转站省略 `event:` 行但 body 始终带 `type`；两者不一致时以 body 为准并记一条 L1 告警 | 无 `event:` 行，只看 body |
| **终帧判定** | `type ∈ {response.completed, response.failed, response.incomplete}` | **`data: [DONE]` 为准**；`finish_reason` 非空只代表该 choice 结束，**不等于流结束**（usage chunk 常在其后） |
| **usage 到达顺序** | 随 `response.completed` 的 `response.usage` 一起到 | **可能在 `finish_reason` 之后单独一个 chunk**（`choices: []` + `usage: {...}`）。故**不可**见到 `finish_reason` 就关账 |
| **错误事件形态** | `type='response.failed'`，错误在 `response.error` | 非 SSE 的 JSON 错误体（见 §6bis），或流中 `{"error": {...}}` chunk |

**解析失败的分类**（必须显式定义，否则开发只能吞掉）：

| 情形 | 处置 |
| --- | --- |
| `data:` 行 JSON 解析失败 | 该帧 `Obs` 全 false，**字节照常透传**（不因我们看不懂就截断用户的流）；记 L1 告警 `frame_parse_failed` |
| 连续 ≥3 帧解析失败 | 判为**渠道格式故障** → `quality_events(event_type='format_broken')` + 该 binding 进冷却；当前请求**不中断**（已提交的照常透传到底） |
| 流结束但从未见终帧 | `terminal_event` 留 NULL → 走 [02 §4.2bis](./02-data-model.md) ③b 分支（`interrupted`） |

**基础解析要点**（借鉴 [zhfeng1](./08-ref-eval-zhfeng1-ai-gateway.md) 实现思路，非代码）：

- **换行归一化**：真实上游 `\r\n` 与 `\n` 混用，须先归一再按 `\n\n` 分块，否则切错事件
- **多行 `data:` 拼接**：SSE 规范允许一个事件多行 `data:`，须 `\n` 拼接后再解析
- **`[DONE]` 与注释行**：`data: [DONE]` 与 `:` 开头的心跳注释**不计入可见内容**（但 `[DONE]` 是 Chat 的终帧标记）
- **`Frame.Raw` 必须是原始字节**：包含 `event:`/`data:` 行与结尾空行，**不得**由解析结果重新拼装 —— 重拼就等于解析-重组，正是 [13 §5](./13-research-reassessment.md) 三个项目翻车的地方

### 3.2 两个独立判定：`ShouldCommit` 与 `HasTTFTOutput`（AC-31）

> ⚠️ **一个布尔值扛不住两个契约**（对抗性审查第 7 轮 [high]）：上一版把"是否可提交/停止接管"与"是否打内容感知 TTFT"合并为单一 `HasActionableOutput`，在**空终态**上直接自相矛盾——`response.completed` 无任何 delta 时，接管契约要求"算，别取消"，而 AC-31 要求该场景 `content_aware_ttft_ms` **为 NULL**。同一个 bool 不可能同时为 true 和 false。
>
> ⚠️ **也不能只认文本**（第 6 轮发现）：若只认"非空 text delta"，**只返回 `tool_calls` 的响应会被判为无内容** —— 而这正是主力 Codex 最常见的工作流，后果是把合法工具响应取消掉、另起上游、重复计费，与 AC-26 承诺的工具兼容直接冲突。

**故拆为两个正交判定，逐事件冻结取值**（按协议分别实现，对齐 [ISSUE-001 假设3](../issues/ISSUE-001-tech-assumption-verification.md) 的三条铁证）：

| 判定 | 语义 | 消费者 |
| --- | --- | --- |
| **`ShouldCommit`** | 上游已产出**确定结果**（含"确定地什么都没有"）→ 提交响应头+缓冲字节，**此后不再接管** | `executor` 期限判定（AC-32） |
| **`HasTTFTOutput`** | 上游产出了**实际内容**→ 打 `content_aware_ttft_ms` | 账本 TTFT / SLA 统计（AC-31） |

| 事件 | Responses | Chat Completions | `ShouldCommit` | `HasTTFTOutput` |
| --- | --- | --- | --- | --- |
| SSE 注释心跳（`: heartbeat`） | — | — | ❌ | ❌ |
| 生命周期元事件 | `response.created` / `in_progress` / `output_item.added`(非 function_call) / `content_part.added` | — | ❌ | ❌ |
| role-only / 空 delta | — | `delta` 仅含 `role` 或为空 | ❌ | ❌ |
| **文本输出** | `response.output_text.delta` 且 text 非空 | `choices[].delta.content` 非空 | ✅ | ✅ |
| **工具调用** ← 关键 | `response.function_call_arguments.delta`；或 `output_item.added` 且 `item.type='function_call'` | `choices[].delta.tool_calls[]` 首次出现 | ✅ | ✅ |
| **拒答** | `response.refusal.delta` | `choices[].delta.refusal` | ✅ | ✅ |
| **推理摘要**（若上游回传） | `response.reasoning_summary_text.delta` | — | ✅ | ✅ |
| **空终态** ← 两者相反 | `response.completed` 且**此前无任何上述内容事件** | `finish_reason` 非空且此前无内容 | ✅ **算**（确定结果，不取消） | ❌ **不算** → `ttft` 留 **NULL** |
| 错误终态 | `response.failed` / `response.incomplete` | `error` 事件 | ✅（确定结果 → 按失败关单，不接管重试） | ❌ |

**两条原则**：

1. `ShouldCommit`：**任何"上游已产出确定结果"的信号都算** —— 文本、工具调用、拒答、推理摘要、终态皆可；只有元事件与心跳不算。宁可少接管一次，也不能把合法响应取消掉重复计费。
2. `HasTTFTOutput`：**只有实际内容算**。空响应/错误终态不产生 TTFT，`content_aware_ttft_ms` 保持 NULL，**不得计入 SLA 首字统计**（否则空响应会伪造出漂亮的 TTFT）。

**蕴含关系**：`HasTTFTOutput ⇒ ShouldCommit`（反之不成立）。实现上 `Observation` 暴露两个独立字段，**不得由一个推另一个**。

> 实现参考：AxonHub 的 `llm/pipeline/empty_response.go:hasResponseContent` 思路可借鉴（[13 §2.1](./13-research-reassessment.md)），但**须扩展到工具调用与拒答**——其原实现同样只覆盖文本。

---

### 3.5 两类异常出口（第 28 轮 [P1]）

**A. T2 缓冲上限强制提交**（[15 T2](./15-scope-and-preflight.md)：缓冲达 256KB 或 5s 仍无 `ShouldCommit`）：

```
达上限 → commit_first_actionable(has_ttft=false, trigger='buffer_limit')
       → 放行已缓冲字节 → 此后纯透传、**不再接管**
```

- `has_ttft_output=false`、`content_aware_ttft_ms` 留 NULL、**不计入 TTFT 统计**——它不是真的首字，只是我们不能再缓冲了。
- 后续终帧照常走 `finalize_upstream`（attempt 已是 `committed`，链路不变）。
- 记 `quality_events(event_type='empty_response')` 供渠道健康归因。

**B. 非 2xx / 非 SSE 响应**（连流都没建立起来）：

| 情形 | attempt 终态 | 是否试下一跳 | 健康处置 |
| --- | --- | --- | --- |
| HTTP 4xx（除 429） | `failed`，`cancel_reason='upstream_error'` | **不试**——请求本身有问题，换渠道也会失败 | 不计入该 binding 失败率 |
| HTTP 429 / 5xx | `failed`，`cancel_reason='upstream_error'` | **试下一跳**（RoutePlan 还有候选时） | 计入失败率 + 冷却退避 |
| 2xx 但 `Content-Type` 非 `text/event-stream`（流式请求下） | `failed` | **试下一跳** | `quality_events(event_type='format_broken')` + 冷却 |
| 连接超时 / TCP 失败 | `failed`，`cancel_reason='upstream_disconnect'` | **试下一跳** | 计入失败率 + 冷却 |

- **每种情形都先 `closeout_attempt`** 收尾本跳（写终态与费用行——4xx/连接失败时费用为 0，`external_call_started_at` 决定是否计费），**再** `dispatch_next`（若要试下一跳）。
- **RoutePlan 耗尽仍失败** → `finalize_abort(request_terminal='failed')`；若一跳都没发出过 → `unavailable`（[02 §2bis](./02-data-model.md) 映射表）。
- **健康更新由 `steward` 异步消费 attempt 结果**，不在请求路径上同步写 `resource_health`——否则每个失败请求都要多一次同步写。

---

## 4. Codex 兼容硬约束（[13 §1](./13-research-reassessment.md)）

主力客户端是 Codex CLI，以下**不是建议而是契约**：

| # | 约束 | 我们的处置 |
| --- | --- | --- |
| 1 | **SSE 事件生命周期必须完整**（`created→in_progress→output_item.added→content_part.added→delta…→completed`） | **字节透传天然满足** —— 上游发什么原样到达。**这正是不做重组的核心理由**（LiteLLM 漏 4 个事件致 Codex 报 `OutputTextDelta without active item`） |
| 2 | `function_call.status` 须为 `in_progress` 而非 `completed` | 透传不改 → 天然满足 |
| 3 | 响应头 `x-codex-*`（限流快照）、`x-models-etag`、**`x-codex-turn-state`（会话粘性令牌，须跨轮回放）**、request id | **白名单透传**（§5） |
| 4 | ~~`GET /v1/models` 全量透传~~ | ⛔ **本条契约于 2026-07-26 被显式推翻**：改为**由网关合成**，见下方说明 |

> **推翻理由与适用边界**：字节透传硬约束保护的是 **SSE 响应流保真**（[13 §5](./13-research-reassessment.md) 三个项目翻车全在流式重组上）。`/v1/models` 是**控制面元数据**，不在该红线内。反之若透传真实模型目录，调用方会看到上游真名并直接用它请求——[FR-117](../PRD.md) 整套「别名即策略载体」会**当场失效**（别名的 SLA 等级、测活资格、数据许可全绕过）。
>
> **合成规则**：
>
> | 场景 | 行为 |
> | --- | --- |
> | `GET /v1/models` | 返回该凭证 `allowed_aliases` 内**已启用**的别名，OpenAI models 格式 |
> | `GET /v1/models/{alias}` | 单查；非别名同样 404 |
> | 请求体里的模型名不是别名 | **404**（不存在） |
> | 是别名但不在该凭证 `allowed_aliases` 内 | **403**（越权，与 [AC-33](./14-acceptance-matrix.md) 一致） |
> | `x-models-etag` | **我方按别名表版本自生成**，不透传上游值 |
> | `If-None-Match` 命中 | 返回 **304**（**可选**：实测 codex-cli 0.142.3 不发该头，见下方实测表） |
>
> **`Client.Models()` 不删除**：降级为**协议能力探测与管理面**用途（比对上游真实目录与我方登记是否漂移），**不对外暴露**。
>
> ✅ **已实测（2026-07-26，codex-cli 0.142.3，`verify/probe_codex_wire.py`）**：
>
> | 问题 | 实测结果 |
> | --- | --- |
> | Codex 会调 `GET /v1/models` 吗 | **会**，一次会话调 2 次（带 `?client_version=0.142.3`） |
> | 它用返回的列表**选模型**吗 | **不用**。mock 返回**空列表** `{"object":"list","data":[]}`，Codex 仍 POST `model="gpt-5.6-sol"` 并正常完成 |
> | 那个模型名从哪来 | **`~/.codex/config.toml` 的 `model =`**，与配置逐字一致 |
> | 会发 `If-None-Match` 吗 | **不会**（两次调用都没发），尽管我方响应带了 `x-models-etag` |
>
> **对本设计的三条结论**：
>
> 1. **合成 `/v1/models` 不会打断 Codex** —— 它不从这个列表取模型名。原先担心的方向不成立。
> 2. **真正的风险在反方向,且已确认**：Codex 会原样发送**用户配置里的模型名**。若该名字不在我们的别名表 → 按合成方案返回 404 → 请求失败。
>    例：本机当前配置是 `gpt-5.6-sol`，而 M0 种子别名是 `gpt-5.5`/`gpt-5.5-sla-1`/`gpt-5.5-cheap` —— **直接指过来会 404**。
>    → 这不是缺陷而是**接入前置**：使用者须把 Codex 的 `model` 改成我方别名之一。必须写进部署文档（[06 §7](./06-deployment-and-operations.md)），不能留成潜伏的 404。
>    → `alias_passthrough` 兜底开关保留，但降级为**迁移期便利**（老配置先跑通再改名），不再是"spike 结论不利时的逃生门"。
> 3. **`x-models-etag` 的 304 支持是可选的** —— 该版本根本不发 `If-None-Match`。我方仍自生成 etag（无害、且未来版本可能用），但**不必**为 304 单独实现协商逻辑。
>    ⚠️ 更正：本文档此前写"不支持 304 则 etag 形同虚设"，实测表明该说法对当前版本不成立。

---

## 5. 头部处理

| 方向 | 规则 |
| --- | --- |
| 请求头（下游→上游） | 剔除 hop-by-hop（`Connection`/`Transfer-Encoding`/`Keep-Alive` 等）；**覆盖** `Authorization` 为该 Binding 的上游 Key；保留 `session_id`/`conversation_id`/`X-Claude-Code-Session-Id` 等会话标识（[02 §4.5](./02-data-model.md) 提取后仍原样转发） |
| 响应头（上游→下游） | **默认透传**，仅剔除 hop-by-hop 与 `Content-Length`（流式重算）；`x-codex-*`/`x-codex-turn-state` **必须保留**（§4）；⚠️ **`x-models-etag` 例外——由我方自生成、不透传上游值**（§4 第 4 条） |
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

- `Probe()` 在渠道接入时与定期健康检查时探测每个模型的 `ProtocolSupport`，**逐协议**落 [02 `channel_models(channel_id, model_id, protocol)`](./02-data-model.md)：写入 `support`、`supports_streaming/tools`、`probed_at`、`probe_failure_reason`。`probed_at` 过期需重探。
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

1. SSE 扫描器 + `ShouldCommit`/`HasTTFTOutput` **双判定**，用 `verify/mock_upstream.py` 场景做夹具；**须新增 tool-only、refusal-only、reasoning-summary-only、空终态四类场景**，每类分别断言两个布尔值（[11 §3](./11-decision-full-selfbuilt.md)）。
2. 字节透传管道 + tee 旁路观察，验证 35 字段与 reasoning item **零丢失**（对照 [07 §3bis](./07-axonhub-runtime-probes.md) 的真实上游基线）。
3. 取消传播与连接池轮换的集成测试。
4. `Probe()` 的协议能力探测 + 落库。

---

_本篇取代原《03 GatewayAdapter 契约》。核心变化：不再适配外部网关，改为自研直连；**Responses 字节级透传为硬约束**，内容感知 TTFT 与 usage 提取全部走旁路观察。_
