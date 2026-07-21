# ccLoad `master` 源码评估

| 项目 | 内容 |
| --- | --- |
| 仓库 | `caidaoli/ccLoad` |
| 研究分支 | `master`；仓库不存在 `main` |
| 固定提交 | `665fec14f5eed2c00fa0aaf54c9a4643c04d0dab` |
| 提交时间 | 2026-07-21 14:19:55（Asia/Shanghai） |
| 标签 | `v3.6.1` |
| 验证范围 | `go test ./internal/app ./internal/cooldown ./internal/util` 通过 |
| 总结 | 三个新增项目中最清晰的 L0 单资源执行绑定；不替代外部 SLA 核心 |

评估期间 `master` 从 `0363945a...` 前进到上述提交。新增提交识别 OpenAI Responses 的 `response.failed` SSE，并把嵌套限流错误映射到 429；本报告已使用更新后的源码和行号。

## 1. 请求链和原生能力

请求链为路由注册 → `HandleProxyRequest` → 模型/协议候选 → Token 渠道过滤 → Channel → Key → URL → `forwardAttempt` → 协议转换/流式写出。已验证能力包括：

- OpenAI、Anthropic、Gemini、Codex 请求入口及协议转换。
- Channel 优先级和加权轮询；Key 顺序/轮询；URL 延迟选择；Key、模型、Channel、URL 多层冷却。
- RPM、并发、Channel 日成本、Token 成本上限和活跃请求管理。
- 流式延迟提交、首流内容超时、取消传播、错误透传和多层串行回退。
- Token、缓存读写、推理、TTFB、标准成本和渠道倍率后成本日志。

关键源码：

- [请求候选和 Token 过滤](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/app/proxy_handler.go#L295)
- [Channel/Key/URL 尝试链](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/app/proxy_forward.go#L2082)
- [Channel 配置](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/model/config.go#L105)
- [请求日志字段](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/model/log.go#L64)

## 2. L0 精确资源绑定

旧评估遗漏了 API Token 的 Channel 白名单。`allowed_channel_ids` 支持 allow/deny 语义，候选生成后、Channel 尝试前执行过滤，并可通过管理 API 热更新：

- [Token Channel 限制模型](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/model/auth_token.go#L60)
- [过滤实现](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/app/auth_service.go#L821)
- [进入尝试循环前过滤](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/app/proxy_handler.go#L321)
- [Token 管理 API](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/app/admin_auth_tokens.go#L152)

可行映射是：一个真实 Provider/Endpoint/Key 建一个单 Key、单 URL Channel；再创建只允许该 Channel 的内部 Token。外部 SLA 网关按请求选择 Token，即可在 stock ccLoad 中把请求限制到一个真实资源。对约二十个上游，这个对象数量可管理，且比依赖全局权重明确。

限制必须写入适配器契约：

- 没有请求内 `channel_id/key_index/base_url` 或候选序列；必须预建 Channel 和 Token。
- 目标 Channel 已被冷却时，过滤发生在候选生成之后，可能返回 403 或无可用上游，不会忽略健康状态强制执行。
- Channel 必须只有一个 Key 和 URL，否则实际资源仍不确定。
- 没有逐请求 `no_retry`；Codex 400 body rewrite 可能在同一 Key 内隐藏重试。[隐藏重试](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/app/proxy_forward.go#L1549)

## 3. 首内容、超时和取消

ccLoad 每个流式 attempt 从请求开始创建首内容定时器，并通过关闭 response body 中断阻塞读取。全局和按上游协议均可配置：[超时配置](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/storage/migrate.go#L358) [请求定时器](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/app/request_context.go#L31)。

`deferredResponseWriter` 在确认流输出前不提交客户端响应；错误或超时可在提交前换 Channel。客户端取消会传播到上游，尝试循环也会停止：[延迟提交](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/app/proxy_stream.go#L146) [提交条件](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/app/proxy_forward.go#L752) [取消读取](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/app/proxy_stream.go#L133)。

但 `HasStreamOutput` 的语义仍比本项目要求宽：除 heartbeat 和 error 外，解析器在检查事件类型前就标记已有输出，`message_start`、`content_block_start`、`response.created` 等元数据可能触发提交。[SSE 输出判定](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/app/proxy_sse_parser.go#L458)。`v3.6.1` 修复了 `response.failed` 假成功，但没有把判定提升为“用户可见文字、推理或工具参数”。

因此内置超时可作为基础保护，连续会话剩余 TTFT 和严格首有效内容仍由外部层计算。外部层应一次只调用一个单资源 Token，过滤元数据事件，在动态期限到达时取消整条请求。

## 4. 价格、余额、缓存和测活

ccLoad 不只支持静态价格：模型目录默认每六小时从 models.dev 拉取，保存来源、ETag 和获取时间；成本引擎覆盖输入/输出、缓存读写、长上下文、service tier、工具及按次项目。Channel `cost_multiplier` 会作为日志快照保存。[目录同步与周期](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/app/model_catalog_sync.go#L22) [ETag 拉取与安装](https://github.com/caidaoli/ccLoad/blob/665fec14f5eed2c00fa0aaf54c9a4643c04d0dab/internal/app/model_catalog_sync.go#L146)

这些能力仍不能替代采购价格账本：日志不保存当次价格版本、币种和生效区间；未知模型可能按 0 成本处理。项目也没有上游 Account、共享余额、Key 独立余额、在途费用预留、保守下限或账单核对。Channel 日限额和客户端 Token 限额都是完成后记账，存在并发超额窗口。

定时 Channel 检查允许配置模型，但请求内容是统一合成内容，且不从合资格真实业务流量抽样；没有探索预算、测活资格、失败后再次试错历史和独立测活账目。缓存方面只有事后 cache token 和 Codex prompt hint，没有通用会话到资源亲和及切换损失预测。

## 5. Attempt 记录和可审计性

每个最终 `forwardAttempt` 日志包含 Channel、Key hash、Base URL、状态、TTFB、Token、缓存、成本和倍率。多 Key/URL/Channel 失败通常会留下多条日志，但：

- 没有统一的外部 request ID、attempt ID、序号、上级请求和候选计划，无法可靠组合为一次 SLA 决策。
- Codex body rewrite 在一个 `forwardAttempt` 内覆盖前一次结果，只留下最终日志和 retry strategy，首次调用没有独立费用/usage。
- 取消或失败流只有已解析到 usage 时才可能记录费用；不能证明取消后是否继续计费。

外部 SLA 网关必须为每次调用生成自己的 Attempt 记录，并把 ccLoad Token、Channel 和日志 ID 作为辅助引用。

## 6. 扩展与维护判断

管理 API 覆盖 Channel、Key、Token、模型、冷却、测试、日志和指标；Channel/Key/Token 可以热更新。全局 settings 更新会触发进程重启，不适合高频策略。项目没有服务端 Webhook、运行时 Plugin 或策略回调，协议 Registry 是编译期注册。

| 使用方式 | 侵入等级 | 判断 |
| --- | --- | --- |
| 单 Key/URL Channel + 单 Channel Token + 外部 SLA 网关 | L0 | 可用，三项新增候选中边界最清晰 |
| 增加受认证的 request-scoped Channel 选择 | L2 | 可集中在认证与候选边界 |
| 加入动态候选期限、严格首内容、余额、故障域、真实测活和 Attempt 账本 | L3 | 不应写入私有 fork |

部署方面，ccLoad 是单 Go 二进制、嵌入 Web、默认 SQLite，也支持 MySQL/PostgreSQL，明显比 OmniRoute 轻。多实例时仍需审查内存冷却、统计缓存和 settings 重启行为。

**结论：上调为 L0 轻量执行备选，但不作为最终基础。** 它适合让外部 SLA 网关精确调用一个资源；不具备成为价格、余额、会话和测活权威系统的条件。
