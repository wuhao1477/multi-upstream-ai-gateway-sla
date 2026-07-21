# OmniRoute `main` 源码评估

| 项目 | 内容 |
| --- | --- |
| 仓库 | `diegosouzapw/OmniRoute` |
| 研究分支 | `main`，不是 GitHub 默认分支；仓库不存在 `master` |
| 固定提交 | `698b6eb00d0a3f589c9ae8828edc7b23002270e0` |
| 提交时间 | 2026-07-19 22:15:24（Asia/Shanghai） |
| 最近可达标签 | `v3.8.48`；该提交本身无标签 |
| 验证范围 | 固定源码、CodeGraph 调用链和链接复核；未安装依赖或启动服务 |
| 总结 | 可作受约束的 L0 单资源执行适配器；不作为最终基础 |

本报告只使用上述 `main` 提交的源码作功能判断。GitHub 默认分支仍为 `release/v3.8.49`；其当前头与 `main` 已分别独有大量提交，不能用相同的 `package.json` 版本号把两者视为同一能力集。

## 1. 请求链和正式扩展能力

主链为 `POST /v1/chat/completions` → `handleChat` → 模型/Combo 解析 → Connection 选择 → `handleChatCore` → Provider executor → 流式转换与用量记录。项目不是单纯桌面客户端，可以独立运行后端，并提供：

- Provider、Connection、API Key、Combo、配额、并发和熔断管理。
- 请求前 Middleware、服务端 Plugin `onRequest/onResponse/onError` 及异步 Webhook。
- OpenAI、Anthropic、Responses 等协议转换，工具调用、推理和缓存 Token 归一化。
- Connection/模型级额度预检、会话亲和、Combo 回退、调用日志和用量历史。

源码证据：

- [Chat 路由](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/src/app/api/v1/chat/completions/route.ts#L115)
- [请求前 Middleware](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/src/sse/handlers/chat.ts#L512)
- [Plugin onRequest](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/open-sse/handlers/chatCore.ts#L447)
- [Plugin 阻断和修改语义](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/src/lib/plugins/hooks.ts#L171)

Middleware 可以在模型/Combo 选择前修改 body 或 model，但其沙箱不适合作外部同步决策客户端；Plugin `onRequest` 执行时 Connection 已选定，只能修改 body 或阻断。Webhook 是请求后的通知。三者都不能直接接收本项目的动态 `RoutePlan`。

## 2. 外部精确资源选择

`main` 支持请求头 `x-omniroute-connection`，单模型路径把它传为 `forcedConnectionId`。Connection 候选先受调用 API Key 的 `allowedConnections` 限制，再按 forced ID 过滤：

- [读取指定 Connection](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/src/sse/handlers/chat.ts#L424)
- [传入单模型执行](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/src/sse/handlers/chat.ts#L936)
- [白名单与 forced ID 过滤](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/src/sse/services/auth.ts#L1073)
- [选中 Connection 响应头辅助函数](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/src/sse/handlers/chatHelpers.ts#L857)

可行的 stock L0 绑定是：一个真实 Key 建一个 Connection；为它创建只允许该 Connection 的内部 API Key；外部适配器选择该凭证并同时发送指定 Connection 头。必须显式配置白名单，因为新 API Key 的 `allowedConnections=[]` 表示允许全部 Connection：[默认 API Key](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/src/lib/db/apiKeys.ts#L585)。Connection 也不得配置 `extraApiKeys`，否则无法证明实际 Key。实际身份应由单资源白名单保证，不能假设所有成功路径都会提供选中 Connection 响应头。

只依赖请求头而复用宽权限 API Key 不安全：活动会话 pin 可以覆盖 forced Connection，只要旧 Connection 仍在允许池中：[亲和覆盖规则](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/src/sse/services/sessionAffinityPin.ts#L221)。单 Connection 白名单可消除此歧义。

## 3. 首内容、超时和取消

OmniRoute 有内部流式预读，但不能把 HTTP 200、首个 SSE 事件或内置 TTFT 当作本项目的“首个有效内容”：

1. `/v1/chat/completions` 等待 2 秒，已知慢 Provider 等待 15 秒；处理仍未完成时，会先提交 HTTP 200 并发送合法但空的 `delta:{}` keepalive。[Keepalive 阈值](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/open-sse/utils/keepaliveThreshold.ts#L20) [空事件和提前提交](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/open-sse/utils/earlyStreamKeepalive.ts#L21)
2. `ensureStreamReadiness` 只要求非 ping 的非空结构化事件；role-only、`response.created` 等生命周期事件也会放行，不保证已有文字、推理或工具参数。[Readiness 判定](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/open-sse/utils/streamReadiness.ts#L84) [预读循环](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/open-sse/utils/streamReadiness.ts#L252)
3. 流完成回调类型有 `ttft`，但主要完成和错误路径没有赋值；写入 `usage_history.ttft_ms` 的值不能作为可靠基线。[完成回调](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/open-sse/utils/stream.ts#L2387) [TTFT 写入](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/open-sse/handlers/chatCore/streamingUsageStats.ts#L33)

真实客户端断开可以沿请求 signal 传播。但 Combo 的单目标超时虽然创建 `modelAbortSignal`，Chat 回调没有把该字段传入 `handleSingleModelChat`，因此超时后旧上游可能继续运行：[目标超时控制器](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/open-sse/services/combo/targetTimeoutRunner.ts#L36) [Combo 回调字段](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/src/sse/handlers/chat.ts#L795)。本项目不能用内置 Combo 承担严格接管；外部层应一次只请求一个 Connection，忽略 keepalive，在动态期限到达时取消整条 HTTP 请求，再开始下一个 Attempt。

## 4. 账目、价格、余额和订阅

成功终态能记录 Provider、Connection、模型、输入/输出/推理、缓存读写和成本；Combo target 也有 step/execution key。缺口是：

- `call_logs` 和 `usage_history` 都没有独立的 attempt ID、序号、上级请求、候选计划和每次实际费用。
- 一个 `handleChatCore` 内多次 `persistAttemptLogs` 复用同一 `pendingRequestId`；`call_logs.id` 是主键且使用普通 INSERT，后续重复写入失败会被吞掉。[Attempt 日志](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/open-sse/handlers/chatCore/attemptLogging.ts#L77) [日志 INSERT](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/src/lib/usage/callLogs.ts#L643)
- 内部 Key 轮换只保存初始/最终 Connection；失败、取消、没有终态 usage 的消耗无法作为财务级 Attempt 证据。
- 额度与余额只覆盖部分 Provider，不具备二十个异构 NewAPI/Sub2API 站点的统一余额、共享账号关系、费用预留和账单核对语义。

智能路由配置包含 Provider 额度、`budgetCap`、成本/延迟/SLA 限制和 `resetWindowAffinity`，可以按额度窗口结束时间提供调度倾向；见 [Intelligent Routing](https://github.com/diegosouzapw/OmniRoute/blob/698b6eb00d0a3f589c9ae8828edc7b23002270e0/src/lib/combos/intelligentRouting.ts#L8)。但该能力默认权重为 0，且没有通用固定费用、采购订阅有效期、跨 Connection 共享订阅池、预计到期未用额度和订阅引流上限。它只能作为外部订阅决策的输入或局部执行策略，不能成为 FR-033～039 的权威账本。

价格、缓存和成功用量可作为外部账本的辅助证据，不能成为唯一权威来源。

## 5. 适配和维护判断

| 使用方式 | 侵入等级 | 判断 |
| --- | --- | --- |
| 单 Connection 白名单 API Key + 单 Key Connection + 外部逐次取消 | L0 | 可作为受约束执行适配器 |
| 增加受认证 RoutePlan、逐请求期限、严格 Connection/Key 回执和 Attempt ID | L2 | 涉及少量稳定边界，但仍需上游接受 |
| 在 OmniRoute 内实现会话预算、通用订阅、价格/余额、真实测活、故障域和完整账本 | L3 | 不接受长期私有分支 |

项目把 Next.js、Dashboard、SQLite、执行器、路由、日志、压缩、Memory、MCP/A2A 等放在同一产品中，升级和安全审查范围显著大于专用执行网关。其插件能力很强，但不能消除本项目最关键的外部 SLA 状态和首有效内容责任。

**结论：不作为最终基础。** 在必须复用其广泛 Provider 能力时，只采用受限 L0 单资源执行模式；不依赖 Combo、内置 TTFT 或 Webhook 实现 SLA。
