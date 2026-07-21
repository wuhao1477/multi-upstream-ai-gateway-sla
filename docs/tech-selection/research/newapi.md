# NewAPI 源码评估

| 项目 | 内容 |
| --- | --- |
| 仓库 | `QuantumNous/new-api`（原 `Calcium-Ion/new-api` 已重定向） |
| 评估提交 | `e0d5156115881780328d31fe9bce7fe25aa9c6c7`（`main`） |
| 最近发布 | `v1.0.0-rc.21` |
| 许可因素 | 按本轮要求，不参与排序 |
| 结论 | 可作为成熟渠道管理与计费平台；不作为最终 SLA 执行数据面 |

## 1. 适用定位

NewAPI 的主要优势是渠道接入、协议转换、用户分组、倍率、配额和对外计费。它适合管理大量 OpenAI 兼容及其他模型渠道，也适合被本项目的 `UpstreamMetadataAdapter` 采集价格、倍率和余额。

它不是为外部 SLA 决策器设计的可编排执行面。stock NewAPI 的逐请求路由以分组、模型、优先级和权重为中心，不能由普通调用方直接下发真实 Key、候选序列和逐候选首字期限。

## 2. 已验证能力

### 渠道与路由

- Channel 包含 Key、模型、分组、余额、已用额度、优先级和权重；见 [Channel 模型](https://github.com/QuantumNous/new-api/blob/e0d5156115881780328d31fe9bce7fe25aa9c6c7/model/channel.go#L23)。
- 同一 Channel 可配置多个 Key，但只支持随机或轮询选择；见 [多 Key 选择](https://github.com/QuantumNous/new-api/blob/e0d5156115881780328d31fe9bce7fe25aa9c6c7/model/channel.go#L175)。
- 默认候选按模型和分组过滤，再按优先级层及权重选择；见 [请求分配](https://github.com/QuantumNous/new-api/blob/e0d5156115881780328d31fe9bce7fe25aa9c6c7/middleware/distributor.go#L32) 与 [候选缓存](https://github.com/QuantumNous/new-api/blob/e0d5156115881780328d31fe9bce7fe25aa9c6c7/model/channel_cache.go#L114)。
- 失败后可重新选择渠道，但规则是通用错误重试，不考虑连续会话剩余 TTFT 预算；见 [Relay 重试](https://github.com/QuantumNous/new-api/blob/e0d5156115881780328d31fe9bce7fe25aa9c6c7/controller/relay.go#L181)。

### 价格、倍率和用量

- 原生支持模型倍率、分组倍率、缓存读写倍率、图片和音频等计费项；见 [价格计算](https://github.com/QuantumNous/new-api/blob/e0d5156115881780328d31fe9bce7fe25aa9c6c7/relay/helper/price.go#L43)。
- 请求日志包含 Channel、上游请求 ID、Token、额度、总耗时、首响应时间和多 Key 索引等信息；见 [Log 模型](https://github.com/QuantumNous/new-api/blob/e0d5156115881780328d31fe9bce7fe25aa9c6c7/model/log.go#L59)。
- Channel 有余额和已用额度字段，但这些数据没有形成共享账号余额、在途费用预留和保守可路由余额语义。

### 管理与部署

- 管理路由支持 Channel CRUD、测试、余额更新和模型获取；见 [Channel 管理路由](https://github.com/QuantumNous/new-api/blob/e0d5156115881780328d31fe9bce7fe25aa9c6c7/router/channel-router.go#L19)。
- 支持 Docker 单容器和 SQLite，个人部署成本较低。

## 3. 与 SLA 需求的关键差距

### 精确资源选择

管理员 Token 可以通过 `token-channel_id` 后缀指定 Channel；普通用户会被拒绝，见 [Token 解析与权限检查](https://github.com/QuantumNous/new-api/blob/e0d5156115881780328d31fe9bce7fe25aa9c6c7/middleware/auth.go#L390) 和 [指定 Channel 分配](https://github.com/QuantumNous/new-api/blob/e0d5156115881780328d31fe9bce7fe25aa9c6c7/middleware/distributor.go#L35)。

该机制只能确定 Channel。若 Channel 内含多个 Key，NewAPI 仍会自行随机或轮询，外部层无法确定真实 Key，缓存归属、余额和费用也不能准确归因。

### 首个有效内容前接管

流式扫描器记录第一行响应并处理整流空闲超时和客户端断开，但没有独立的“首个用户可见有效内容”期限，也不会依据会话预算切换候选；见 [流式扫描](https://github.com/QuantumNous/new-api/blob/e0d5156115881780328d31fe9bce7fe25aa9c6c7/relay/helper/stream_scanner.go#L240)。

因此外部 SLA 网关必须在提交给客户端前观察 NewAPI 输出，超时后取消当前请求并重新选择资源。取消后的继续计费和重复费用也必须由外部账本核对。

### 观测和业务测活

- 主要 Usage Log 是一次最终请求记录，不能稳定表示每个失败、重试和取消尝试。
- 性能聚合以模型和分组为主，不能替代真实 Key、故障域和尝试级统计。
- Channel Test 是管理员发起的合成测试，不是符合 FR-060～068 的真实业务测活；见 [Channel Test](https://github.com/QuantumNous/new-api/blob/e0d5156115881780328d31fe9bce7fe25aa9c6c7/controller/channel-test.go#L75)。
- 未发现请求前同步 Webhook 或通用运行时插件机制。

## 4. 适配器边界

可接受的 L0 方式是：

1. 一个真实 Key 建立一个独立 Channel，禁止在该 Channel 内配置多 Key。
2. 外部 SLA 网关持有管理员专用内部 Token，通过 Channel ID 选择资源；该 Token 不暴露给客户端。
3. 外部 `ExecutionAdapter` 负责首内容预读、动态期限、取消、候选切换和尝试级账目。
4. 外部 `UpstreamMetadataAdapter` 负责渠道采购价、倍率、共享余额和可信版本，不把 NewAPI 的用户售卖价直接视为上游真实成本。

若要求普通 API 直接传入资源计划，或要求 NewAPI 内部执行动态首字接管，则至少需要 L2/L3 修改请求认证、调度、流式处理和日志链路。

## 5. 采用判断

**列入候选，但不作为最终推荐。** NewAPI 的渠道生态、计费和轻量部署很成熟，适合承担渠道管理、对外分发或作为本项目需要适配的一类上游。作为外部 SLA 决策层的数据面时，它的精确 Key 控制、首个有效内容接管和尝试级记录弱于 AxonHub。
