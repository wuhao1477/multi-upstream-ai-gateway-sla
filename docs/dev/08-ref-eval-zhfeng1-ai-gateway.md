# 08 参考项目评估：zhfeng1/ai-gateway（2026-07-23）

| 项目 | 内容 |
| --- | --- |
| 状态 | ✅ 研究完成，**结论待评审**。**本篇未修改任何现有设计文档**，候选改动见 §5，由项目负责人定夺 |
| 日期 | 2026-07-23 |
| 评估对象 | [zhfeng1/ai-gateway](https://github.com/zhfeng1/ai-gateway) |
| 评估提交 | `3bf7431665669d5dc059e105e66f8806ecb13e3a`（`v0.1.13-11-g3bf7431`）—— **与[选型期评估](../tech-selection/research/zhfeng1-ai-gateway.md)同一提交，代码无变化** |
| 评估目的 | 选型期已排除它作为**候选网关**；本轮改问：它的实现对**我们自研核心的设计**有无**启发/参考**价值 |
| 方法 | 浅克隆读源码（单文件 `app/main.py` 2441 行）+ README；结论均给出代码行号证据 |
| 一句话结论 | **不改选型结论（仍排除）；但提供 1 条决定性佐证 + 2 条可选的小方法论**，其余一律不吸收 |

> **务实边界**：本篇刻意只挑"小、具体、能直接落到我们现有设计"的点。它的架构、调试控制台、抓包模型一概不吸收——见 §6 反面清单。

---

## 1. 它到底是什么（事实基线）

| 项 | 事实 |
| --- | --- |
| 定位 | **任意上游的透明调试代理 + 请求查看器**（上游 URL 嵌在网关路径里），不是多上游调度网关 |
| 规模/活跃 | 4 stars、0 forks、25 commits、单作者；**代码自选型评估以来未变** |
| 栈 | Python（FastAPI + httpx）+ **SQLite**；WebSocket 推送控制台；另打包 macOS/Windows 桌面版 |
| **许可证** | ⚠️ **仓库无 LICENSE 文件** —— 法律上**不可复用其代码**，本篇只借鉴思路，不复制实现 |
| 有的能力 | 全量请求/响应抓取、SSE 多视图（JSON/文本/原始事件流）、指标（网关耗时 / 上游耗时 / 差值 / TTFT / TPS / reasoning tokens）、路径隔离命名空间 |
| 没有的能力 | 多渠道调度、负载均衡、故障转移、成本/账本、余额、价格、会话、SLA、配置模型 |

**对选型结论的影响：无。** 代码未变、能力未变，[选型期"立即排除"](../tech-selection/research/zhfeng1-ai-gateway.md)（补齐目标能力等同重写、L3 侵入）继续成立，无需重新评估。

---

## 2. 决定性发现：它的 TTFT 也是错的，而且最朴素

核心代码（`app/main.py:2404-2411`）：

```python
async def stream_response() -> AsyncIterator[bytes]:
    first_byte_at = None
    async for chunk in upstream_response.aiter_bytes():
        if first_byte_at is None:
            first_byte_at = time.perf_counter()   # ← 任意第一个字节即打点
```

`httpx.aiter_bytes()` 产出的是**原始 HTTP 字节块**，因此 `first_byte_ms` 会把以下全部算作"首字"：SSE 注释心跳（`: heartbeat`）、role-only delta、空 SSE、乃至分帧字节。**它连 SSE 事件层都没解析**就打点。

### 与已实测的两个候选并列

| 实现 | 首字打点位置 | 排除心跳注释? | 排除 role-only? | 等于用户可见内容? |
| --- | --- | --- | --- | --- |
| AxonHub `metricsFirstTokenLatencyMs` | 首个 **JSON 流事件** | ✅ 是 | ❌ 否 | ❌ |
| ccLoad `first_byte_time` | 首个**已提交流事件** | ✅ 是 | ❌ 否 | ❌ |
| **zhfeng1 `first_byte_ms`** | 首个**原始字节** | ❌ 否 | ❌ 否 | ❌ **最朴素** |

**含义（本篇最有价值的一条）**：三个**互相独立**的实现，全部把"首字节/首事件"当 TTFT，无一做内容感知。这说明**这不是某个项目的疏忽，而是该品类的系统性通病**。

→ **对我们的作用是「加固」而非「改动」**：[AC-31](../PRD.md)（TTFT 自算、不采信网关字段）与 [ISSUE-001 假设 3/6](../issues/ISSUE-001-tech-assumption-verification.md) 的结论再获一个独立佐证。**现有设计无需任何修改**，只是把"必须自算"从"两个候选都踩坑"升级为"品类通病，换谁都得自算"。

---

## 3. 两条值得考虑的小方法论

### 3.1 网关自身开销的度量法（`gateway_overhead_ms`）

`app/main.py:171-173, 492-493`：

```python
upstream_duration_ms = int((finished_at - upstream_started_at) * 1000)   # 上游耗时
# ...
"gateway_overhead_ms": row["duration_ms"] - row["upstream_duration_ms"]  # 总耗时 - 上游耗时 = 网关自身开销
```

即在**发往上游前**多打一个时间戳，用 `总耗时 − 上游耗时` 分离出网关自身开销。

**与我们的关系**：[FR-110](../PRD.md) 要求"决策过程给每个请求增加的时延 P99 ≤50 毫秒"，[06 §6](./06-deployment-and-operations.md) 也写了"自监控 P99 决策开销 ≤50ms"，**但没写怎么测**。这个"总减上游"的分解就是最简单正确的测法，可直接用于我们的自监控。

**候选改动（小）**：在 06 §6 可观测性里明确该测法；在 [02](./02-data-model.md) `attempts` 可考虑加一列 `gateway_overhead_ms`（或仅作运行时指标不落库）。

### 3.2 TTFT 与生成速度两段式分解（`tps_from_values`）

`app/main.py:466-474`：

```python
generation_ms = duration_ms - first_byte_ms      # 剔除首字等待
return round(output_tokens / (generation_ms / 1000), 2)
```

TPS **不用总耗时**，而用"总耗时 − 首字耗时"，即只算**生成阶段**的速度。

**与我们的关系**：[FR-040](../PRD.md) 要输出速度、[02 `attempts.output_tokens_per_s`](./02-data-model.md) 已有该列，但未规定分母口径。用总耗时会把首字等待混进生成速度，慢首字的渠道会被误判为"生成慢"。**这个两段分解是正确口径**。

**候选改动（很小）**：在 02 给 `output_tokens_per_s` 加一句口径说明（分母 = 完整延迟 − 内容感知 TTFT），或在 05 健康指标里写明。

### 3.3 （更次要）多形态 usage 提取

`app/main.py:253-292` 对多种响应结构逐路径探测 usage：`usage.output_tokens_details.reasoning_tokens`、`usage.completion_tokens_details.reasoning_tokens`、`response.usage.*`、`message.usage.*` —— 覆盖 OpenAI Chat Completions / Responses / Anthropic 三种形态；另有 `parse_completed_response_from_sse` 逆序扫 SSE 找 `response.completed` 还原最终响应。

**与我们的关系**：主路径我们从 AxonHub `usageLogs` 拿用量，用不上。但 **ccLoad 退路无渠道级定价、账本需自算成本**（[03 §6.1](./03-gateway-adapter.md)），届时要从原始上游响应里抠 usage，这种"多路径探测 + 从 SSE 还原完成态"的写法可作参考。**优先级低，M1 之后再说。**

---

## 4. 它的反面价值：正好印证我们 FR-112 的取舍

它默认 `MAX_CAPTURE_BYTES=0`（**不限制**）、全量保存请求/响应正文与请求头（含 `Authorization`）、日志接口无鉴权、目标 URL 未防 SSRF。

我们 [FR-112](../PRD.md) 明确"一期**不存**请求正文/上下文/请求头/请求体"、[02 §9.2](./02-data-model.md) 还加了 CI schema 断言禁止出现 `body/headers` 列。**它正是我们决定不做的那种设计的活样本**——一个调试工具这么做合理，一个长期在线的 SLA 网关这么做就是存储膨胀 + 凭证泄露面。这条**加固我们既有决策，不需要改任何东西**。

---

## 5. 候选改动清单（**待你定夺，我未改任何现有文档**）

| # | 候选改动 | 影响文档 | 规模 | 我的建议 |
| --- | --- | --- | --- | --- |
| 1 | 明确"网关/决策自身开销"的度量法 = 总耗时 − 上游耗时 | 06 §6（+ 02 可选加列） | 很小 | **建议采纳**：FR-110 的 50ms 约束目前只有目标没有测法 |
| 2 | 明确 `output_tokens_per_s` 分母 = 完整延迟 − 内容感知 TTFT | 02（+ 05 健康指标） | 极小 | **建议采纳**：口径不定会误判慢首字渠道 |
| 3 | 把"TTFT 品类通病"作为第三方佐证补进 ISSUE-001 或 AC-31 说明 | ISSUE-001 / 03 | 极小 | 可选：**不改结论，只加一句佐证**，不加也不影响正确性 |
| 4 | 多形态 usage 提取思路记入 ccLoad 退路的账本自算 | 03 §6 | 小 | **暂不动**：M1 之后真要用 ccLoad 退路时再说 |

---

## 6. 明确不吸收（防过度设计）

| 不吸收 | 理由 |
| --- | --- |
| 它的整体架构（上游 URL 嵌路径、单文件、桌面打包） | 与我们"自研核心 + GatewayAdapter + stock 网关"的分层完全不同 |
| 调试控制台 / WebSocket 实时列表 / SSE 多视图 UI | 调试工具形态，非生产 SLA 网关所需；做了就是过度设计 |
| 全量抓包（正文+请求头） | **直接违反 FR-112**，见 §4 |
| 它的任何代码 | **无 LICENSE，法律上不可复用**；只借鉴思路 |
| 重新考虑它作为候选网关 | 代码未变、能力未变，选型排除结论继续成立 |

---

## 7. 结论

1. **选型结论不变**：仍排除，无需重新评估（代码自评估以来未动）。
2. **最大价值是一条佐证**：三个独立实现的 TTFT 全是"首字节/首事件"口径，**证明这是品类通病**，我们"自研核心必须自算内容感知 TTFT"的硬约束（AC-31）由此更稳固——**但不需要改任何设计**。
3. **可选的两条小改进**（网关开销测法、TPS 分母口径）是真实且低成本的缺口补全，建议采纳，但**等你确认后我再动文档**。
4. 其余一律不吸收，避免过度设计。

_研究资料留档；本篇不修改任何现有设计文档，§5 的候选改动待项目负责人拍板。_
