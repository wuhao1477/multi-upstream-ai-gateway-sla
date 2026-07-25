# 13 调研资产重审（转向自研后的参考价值再评估 · 2026-07-25）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ 重审完成；**新发现的实现风险需落入 03 重写与 02/12**（见 §6） |
| 日期 | 2026-07-25 |
| 缘起 | [11 转向决策](./11-decision-full-selfbuilt.md)后，全部存量调研的**评估标准变了**，须整体重审 |
| 范围 | 19+ 网关项目、5 个客户端、3 个上游家族、18 份调研文档 |

## 0. 评估标准的根本变化

| | 转向前 | 转向后 |
| --- | --- | --- |
| **网关项目** | "能否作为我们的执行数据面？"（采纳/排除） | **"实现时能借鉴什么？"**（参考价值） |
| **客户端项目** | "会不会发会话标识？"（能否识别会话） | **"它到底怎么发请求、期待什么响应？"**（我们要亲自接住它） |
| **上游家族** | 采集侧对接（不变） | 采集侧对接 + **执行侧协议细节**（新增） |

> **关键反转**：被排除的项目现在可能有**更高**参考价值——AxonHub/ccLoad 是 Go、同领域、已踩过我们即将踩的坑。

---

## 1. 客户端重审：Codex CLI（主力，价值最高）

转向前只需知道"它发不发 `session_id`"；**转向后它的每一个线上行为都是我们的实现契约**。重审发现三条硬约束，此前完全没有记录。

### 1.1 ⚠️ SSE 事件生命周期必须完整（否则 Codex 直接报错）

Codex 的客户端状态机**要求完整的事件序列**，只发 delta 会被拒绝：

```
response.created → response.in_progress → response.output_item.added →
response.content_part.added → response.output_text.delta (×N) →
response.output_text.done → response.content_part.done →
response.output_item.done → response.completed
```

**真实故障案例**：LiteLLM 曾遗漏 `response.created` / `in_progress` / `output_item.added` / `content_part.added` 四个事件（[BerriAI/litellm#20975](https://github.com/BerriAI/litellm/issues/20975)），Codex 报错：

```
codex_core::util: OutputTextDelta without active item
```

→ **一个主流网关正是在这里翻车。** 我们自研透传层若做流式重组（而非字节级透传），**必须保证事件序列完整**，否则主力客户端直接不可用。
→ **最稳的实现选择：对 Responses 路径做字节级透传，不重组事件**（见 §6 落点）。

### 1.2 ⚠️ `function_call` 的 status 语义

Codex 期待 `function_call` item 的 `status` 为 **`in_progress`** 而非 `completed`——该字段告诉 Codex"工具已被调用但尚未产出结果"。**误置 `completed` 会让 Codex 以为工具已执行完毕**，工具调用链断裂。

### 1.3 ⚠️ 上游响应头必须原样透传

来自专为 Codex 写的代理 [codexcomp](https://github.com/dzshzx/codexcomp) 的实践：

| 头 | 作用 | 后果 |
| --- | --- | --- |
| `x-codex-*`（限流快照） | Codex 读取上游限流状态 | 丢失则 Codex 无法感知限流 |
| `x-models-etag` | 模型目录刷新 | 丢失则模型列表不更新 |
| **`x-codex-turn-state`** | **会话粘性路由令牌，须跨轮回放** | **丢失则会话亲和断裂** |
| request id 类 | 排障关联 | 丢失则无法与上游对账 |

→ `x-codex-turn-state` 是 [02 §4.5](./02-data-model.md) **此前未记录的会话亲和信号**，须补入提取链认知。
→ 另需 **`GET /v1/models` 全量透传**（Codex 靠它刷新模型目录）。

### 1.4 其他客户端（结论不变）

| 客户端 | 重审结论 |
| --- | --- |
| Claude Code | 一期不直连（Anthropic Messages 不在 FR-111）。其 `X-Claude-Code-Session-Id` 设计仍是"头部优于 body"的佐证 |
| OpenCode / Gemini CLI / Cline | 结论不变（[02 §4.5](./02-data-model.md)）。Cline 明确不适配 |

---

## 2. 网关项目重审：从"候选"到"参考实现"

### 2.1 AxonHub —— 已排除为执行面，**升为首要参考代码库**

| 维度 | 参考价值 |
| --- | --- |
| 语言/领域 | **Go、同领域**，可直接读实现 |
| 内容感知判定 | `llm/pipeline/empty_response.go` 的 `hasResponseContent`/`hasMessageContent`——排除 role-only/空 delta 的逻辑，**正是我们 AC-31 要实现的** |
| 渠道/凭证模型 | Channel/Key/Profile 的组织方式可参考 |
| 许可证 | 主体 Apache-2.0、`llm/` LGPL-3.0；个人内部不分发不触发义务，**仅借鉴思路** |

> 讽刺但真实：我们**不用它做执行面**，恰恰是因为它的对外 TTFT 字段不可信；但它**内部**的内容感知判定写得是对的，值得参考。

### 2.2 ccLoad —— 已排除，**取消传播与延迟提交的参考**

| 维度 | 参考价值 |
| --- | --- |
| 取消传播 | [假设6 补验](../issues/ISSUE-001-tech-assumption-verification.md)实测证明其 mid-stream 取消**能传播到上游止损**（mock 侧立即 EPIPE）——AC-32 的可行性证据与实现参考 |
| 延迟提交 | `deferredWriter.Commit()` 的提交时机控制，与我们"首字前可切换、提交后不可切换"（FR-078）同构 |
| 反面教材 | 其 Codex 400 body-rewrite 隐藏重试**只留一条审计记录**——我们自研**不做隐藏重试**，一次外部调用严格对应一次上游调用 |

### 2.3 zhfeng1/ai-gateway —— 调试能力参考（已落地）

已重审并产出 [12 可调试性设计](./12-debuggability.md)：SSE 解析的换行归一化与多行 `data:` 拼接、延迟两段分解、抓取截断保护（反面用）。

### 2.4 其余网关（低参考价值，但有一条教训）

| 项目 | 重审结论 |
| --- | --- |
| **LiteLLM** | **最大价值是其 bug**（§1.1）：主流网关在 Responses SSE 生命周期上翻车，是我们的前车之鉴 |
| Bifrost / Portkey / Manifest / gpt-load / uni-api | 无特别可借鉴项；架构与我们（决策在核心、执行纯透传）不同 |
| NewAPI / Sub2API / Octopus / Aether / OmniRoute | 作为**上游站型**的价值远大于作为网关（见 §3） |
| claude-relay-service | 订阅账号池管理，一期用不上（上游全是 `sk-` key 中转站） |

---

## 3. 上游家族重审：ISSUE-002 全部有效，新增执行侧视角

| 家族 | 采集侧（不变） | 执行侧（新增关注） |
| --- | --- | --- |
| NewAPI | [ISSUE-002](../issues/ISSUE-002-collector-adapter-design.md) 完全有效 | 说 OpenAI 协议；**2026-07-25 实测**：同一站 `gpt-5.5` 支持 `/v1/responses` 与 `/v1/chat/completions`，但**部分模型仅 Responses**（CC 返回 503）——自研层须按模型探测能力，不能假设两协议都通 |
| Sub2API | 完全有效 | 同上 |
| ASXS | 完全有效 | 一期未接执行 |

**新增实测事实**（[07 §3bis](./07-axonhub-runtime-probes.md)）：真实上游 Responses 响应含 **35 个顶层字段**、`reasoning` item、`prompt_cache_key`/`prompt_cache_retention`、`usage.cached_tokens`——**这就是自研透传层要 100% 保真的目标**。

---

## 4. 转向后新增的实现风险（此前无人负责，现在归我们）

| # | 风险 | 来源 | 严重度 |
| --- | --- | --- | --- |
| 1 | **SSE 事件生命周期不完整 → Codex 报错不可用** | LiteLLM#20975 实例 | 🔴 高 |
| 2 | `function_call.status` 语义错误 → 工具链断裂 | Codex 契约 | 🔴 高 |
| 3 | 上游响应头被吞（`x-codex-turn-state` 等）→ 会话亲和/限流感知断裂 | codexcomp 实践 | 🟠 中高 |
| 4 | **连接池楔死**：上游连接卡住会饿死后续所有请求 | codexcomp 实践 | 🟠 中高 |
| 5 | **流式空闲超时**：上游读取停滞会永久占住连接 | codexcomp（>120s 阈值） | 🟠 中高 |
| 6 | 下游中途断开的清理与上游取消传播 | codexcomp + 假设6 | 🟠 中 |
| 7 | `GET /v1/models` 未透传 → Codex 模型目录不刷新 | codexcomp 实践 | 🟡 中低 |

---

## 5. 一条关键的架构启示

§1.1 与 [07 §3bis](./07-axonhub-runtime-probes.md) 合起来看，得到一个**此前没意识到的结论**：

> AxonHub 的 Responses round-trip 会**吞掉 reasoning item**——而 Codex 恰恰要求 reasoning 保真且事件生命周期完整。
> **即使我们不转向自研，AxonHub 这条路对主力 Codex 也是走不通的。**

这把 [11 的转向](./11-decision-full-selfbuilt.md)从"权衡后的选择"提升为"**必然选择**"。

**由此确定实现取向**：Responses 路径应做**字节级透传**（passthrough），而非解析-重组。理由：

- 重组就要负责完整事件序列（§1.1 的坑），LiteLLM 已示范其难度
- 字节级透传天然保真 35 字段、reasoning item、上游响应头
- 我们需要的内容感知 TTFT（AC-31）只需**旁路观察**流内容，不必重组它

---

## 6. 落点（需改的文档）

| 发现 | 落到 |
| --- | --- |
| SSE 生命周期完整性 + 字节级透传取向 | **03 重写**（上游对接层）核心原则 |
| `function_call.status` 语义 | 03 |
| 上游响应头透传清单（`x-codex-*`/`x-models-etag`/`x-codex-turn-state`） | 03 |
| `x-codex-turn-state` 作为会话亲和信号 | [02 §4.5](./02-data-model.md) 补入 |
| 连接池楔死、流式空闲超时、下游断开清理 | 03 + [12](./12-debuggability.md) 可观测项 |
| `GET /v1/models` 透传 | 03 |
| 上游按模型的协议能力探测（部分模型仅 Responses） | 03 + [05](./05-scheduling-and-operations.md) 候选过滤 |
| AxonHub `hasResponseContent` 作内容感知参考 | 03 实现注记 |

---

## 7. 重审结论

1. **存量调研没有白做**：ISSUE-002 全部有效；ISSUE-001/07 转为决策证据 + 保真目标；zhfeng1 已产出 12。
2. **最大增量来自客户端侧**：Codex CLI 的三条硬约束（事件生命周期、function_call status、响应头透传）**此前完全没有记录**，而它是主力。
3. **发现 7 项新实现风险**，全部归自研层负责，已列落点。
4. **转向决策被强化**：AxonHub 吞 reasoning item 与 Codex 要求直接冲突，该路本就走不通。

_本篇只做重审与落点登记，未改动既有文档。§6 的落点随 [11](./11-decision-full-selfbuilt.md) 的全量返工一并执行。_
