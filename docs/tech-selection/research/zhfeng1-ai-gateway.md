# zhfeng1/ai-gateway 源码评估

| 项目 | 内容 |
| --- | --- |
| 仓库 | `zhfeng1/ai-gateway` |
| 评估提交 | `3bf7431665669d5dc059e105e66f8806ecb13e3a`（`main`） |
| 最近标签 | `v0.1.13`；无 GitHub Release |
| 许可证 | 仓库未提供许可证 |
| 结论 | 立即排除 |

## 1. 项目定位

该项目是任意目标 URL 的透明代理和请求查看器，不是多上游调度网关。请求链只有 `scoped_proxy` → `proxy` → `proxy_to_target` → `httpx.AsyncClient.send` → `stream_response`。

固定提交源码：[app/main.py](https://github.com/zhfeng1/ai-gateway/blob/3bf7431665669d5dc059e105e66f8806ecb13e3a/app/main.py#L2330)。

它没有渠道、模型、账号、Key 池、价格、余额、权重、重试、故障域、会话状态、SLA 或配置管理模型。除原始首字节、总时长和状态码外，PRD 需求基本没有可复用基础。

## 2. 独立阻碍

- 仓库没有 `LICENSE`、`COPYING` 或源码授权条款，GitHub 许可证字段为空；因此没有明确授予复制、修改和分发权利。
- 目标 URL 校验只检查 HTTP scheme 和 netloc，未限制内网目标，存在 SSRF 风险：[URL Validation](https://github.com/zhfeng1/ai-gateway/blob/3bf7431665669d5dc059e105e66f8806ecb13e3a/app/main.py#L450)。
- 请求头只移除 hop-by-hop 字段，Authorization 会被保存和转发。
- 默认 `MAX_CAPTURE_BYTES=0` 表示不限制保存的请求和响应正文大小：[Capture Limit](https://github.com/zhfeng1/ai-gateway/blob/3bf7431665669d5dc059e105e66f8806ecb13e3a/app/main.py#L16)。
- 日志查询、详情和 WebSocket 没有管理认证；路径中的 `access_key` 只是日志命名空间。
- 上游失败直接返回 502，没有候选重试。

## 3. 不推荐原因

补齐目标能力等同于重新实现一个网关，侵入等级为 L3；无许可证本身也足以阻止采用。

**结论：不进入任何后续验证。**
