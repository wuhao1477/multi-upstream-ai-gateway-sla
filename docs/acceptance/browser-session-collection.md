# 账号密码与浏览器会话采集：阶段验收

日期：2026-10-08。Issue：[#26](https://github.com/wuhao1477/multi-upstream-ai-gateway-sla/issues/26)。分支：`codex/release-v1.0.9`，在原目录开发，未创建 worktree。

## 当前结论

任务 1 的凭据配置已实现并完成下述验证；完整迁移脚本尚未运行。任务 2–4 的真实登录、采集接入及浏览器部署未完成，**不得据此宣称首站登录成功或版本可发布**。

新增独立凭据表与 AES-256-GCM 存储、管理 PUT/DELETE、账号列表状态及现有抽屉控件。每次更新撤销会话；账号停启或用户 ID 变化使旧任务无法写回，站点地址/家族变化删除原站凭据。原令牌表、采集请求及远端 Key 创建路径未改动。

## 已执行验证

| 范围 | 结果 |
| --- | --- |
| 隔离 PostgreSQL 16 | 成功应用 001–032；4 个浏览器存储测试通过，覆盖加密保存/恢复、留空保留密码、更换用户名、清除、旧会话修订、账号停启、站点变化与 AAD/篡改拒绝 |
| 管理接口 | 3 个新增测试通过：认证、缺密钥 503、真实数据库的配置生命周期；拒绝多余/尾随 JSON、只读写入，秘密不回显 |
| 构建 | `make build VERSION=v1.0.9-dev` 退出 0，包含前端 lint/typecheck/build 和三个 Go 二进制 |
| Go 竞态测试 | 首次 `make test` 与构建并行，admin/core 因嵌入的旧前端文件被替换而编译失败；其他包通过。构建结束后仅补跑 `go test -race ./cmd/sla-core ./internal/admin`，通过。新增存储测试及最后简化的密码保存逻辑分别增量验证通过 |
| 前端 | `cd web && pnpm test` 退出 0；真实 Chrome 配置检查在 1280px/375px 均通过，覆盖保存、待验证状态、关闭清空密码、重开及清除 |
| Compose | 两份配置解析通过，新可选密钥传给 core；根目录 Compose 的 collector 不接收此阶段不使用的密钥。未修改默认发布镜像版本 |

真实数据库测试使用仅本任务创建的本机隔离实例，通过 `SLA_TEST_DSN` 显式指定；没有读写业务数据库。Chrome 检查输入为本项目管理配置的测试数据，没有模拟或访问上游。未使用真实站点密码，也未记录秘密截图或请求体。

本阶段检查入口（环境变量须指向可写的隔离测试实例）：

```sh
go test -race ./internal/store ./internal/admin -run '^TestBrowser' -count=1 -v
node verify/ui/verify-browser-credentials.mjs
```

Go 测试需 `SLA_TEST_DSN`；Chrome 检查需 `BASE`、`ADMIN_TOKEN`、`SLA_TEST_DSN`，默认使用本机 Google Chrome。后者在指定实例创建配置测试记录，**不得对业务部署执行**。

## 未验证及下一步

1. `./verify/test-migrate.sh` 和 Python 文档/schema 校验未执行：当前 Conda 不可用，遵守 Python 隔离要求，未运行默认 Python。新增测试已纳入既有迁移脚本；直接真实迁移和 Go 测试不替代完整脚本。
2. `https://api.lyjxka.top/` 只检查过公开登录页面，未提交真实登录。需操作者在运行本分支的本机管理页配置自有账号密码，再验证普通 HTTP 会话是否足够；只有实测依赖浏览器环境时才引入 chromedp。
3. 尚无测试登录按钮、自动登录/读取切换、会话过期重登或 Chromium 镜像；未验证真实余额、已有 Key、分组、价格和模型目录。CF/Turnstile 等验证不在实现范围。

本阶段不单独发布；完成其余任务与真实验收后随下一版本一并上线。
