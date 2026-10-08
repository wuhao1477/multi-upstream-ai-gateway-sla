# Cookie 登记、导入与读取验收记录

日期：2026-10-08。Issue：[#26](https://github.com/wuhao1477/multi-upstream-ai-gateway-sla/issues/26)。分支：`codex/release-v1.0.9`，在原目录开发，未创建 worktree。

## 当前状态（2026-10-08 Cookie 实施）

账号密码登录方案已撤回并从运行时代码移除。现已实现加密 Cookie 登记、all-api-hub
共用导入、身份验证与受限读取；保留正常令牌路径，不添加浏览器运行时。需求见
[Issue #26](../issues/ISSUE-browser-session-collection.md)。

没有获得真实上游 Cookie，**首站身份、余额和已有 Key 仍未验收**。下列数据库/行为
测试与管理表单检查不能证明真实 Cookie 有效；不把旧密码阶段的结果计入 Cookie 验收。

## Cookie 方案已执行验证

| 范围 | 结果 |
| --- | --- |
| 真实 PostgreSQL 16 | 独立测试库应用 033；覆盖 AES-GCM、账号/origin 绑定、空值保留、替换、停用/清除、身份和站点变更、修订冲突及旧任务写回。旧密码/会话表已移除 |
| 管理 API / 导入 | 真实库覆盖保存、验证、删除、缺密钥/额外字段拒绝、秘密不回显，以及 all-api-hub 新建/补齐/更新/预览、同站多账号、事务回滚、同值失效状态不重置、缺失不删除和不覆盖令牌 |
| 读取行为 | 覆盖令牌优先、单次 Cookie 切换、Authorization 互斥、身份不匹配、失效停止、状态分类、读取白名单、重定向/私网目标拒绝、修订重查及渠道锁。行为输入不作为真实站点协议证据 |
| 构建 | `make build VERSION=v1.0.9-dev-cookie` 退出 0，包含前端 lint/typecheck/Vite build 和三个 Go 二进制 |
| Go 测试 | 带真实 `SLA_TEST_DSN` 执行一次 `make test`，首次两项失败；其余包通过。修正测试隔离与 DSN 格式后，仅增量补跑两项失败测试，`-race -count=1 -v` 均通过、无跳过。未声称首次全量命令退出 0 |
| 前端 | `cd web && pnpm test` 退出 0 |
| 真 Chrome | 1280px 完整流程通过；375px 首次在关闭/重开处失败，诊断确认点击被 `#toast` 截获，脚本等待提示隐藏后通过。随后新增 Cookie-only 账号汇总/筛选/列表显示检查，先观察到失败；修正相关判断并重新构建后，1280px/375px 均通过 |
| 静态检查 / 部署配置 | `make vet`（含 rows.Err 检查）与两份 Compose 的 `config --quiet` 通过。core/collector 使用同一 `SLA_COOKIE_SECRET_KEY`，未改变默认发布镜像版本 |

首次全量 Go 测试的两项失败及修正：

- `TestCookieCredentialsInvalidatedWithAccountOrSite` 使用固定变更后地址，旧清理只按原地址删除，重复运行遇到残留渠道。新测试改用含渠道 ID 的地址并注册对应清理；只移除了本任务已确认的遗留测试记录。
- 既有 `TestPoolMaxConnsFitsConcurrentCollectionWorkers` 要求 URL DSN，之前传入 keyword DSN。未改生产代码或既有测试，改用无密码 URL DSN，并通过 `PGPASSWORD` 提供测试库密码。

Cookie 表单检查入口（只允许可写的本机隔离测试实例）：

```sh
cd verify/ui
pnpm exec node verify-cookie-credentials.mjs
# 已有宽屏结果时，仅补移动端：
pnpm exec node verify-cookie-credentials.mjs --mobile-only
```

需 `BASE`、`ADMIN_TOKEN` 和 `SLA_TEST_DSN`。脚本使用本机真 Chrome、真实管理 API
与独立 PG，输入是没有上游授权意义的测试字符串。验证按钮访问真实 `.invalid`
地址并如实报告网络失败，不模拟认证成功；不读取真实备份或浏览器 Cookie。

### 首站公开路径证据

2026-10-08，无 Cookie/Authorization 的公开请求：

- `GET https://api.lyjxka.top/api/token?p=1&size=100` 返回 301，跳转到带尾斜杠地址。
- `GET https://api.lyjxka.top/api/token/?p=1&size=100` 返回 401，无跳转。

据此将 Cookie Key 列表读取直接规范为 `/api/token/`，仍禁止跟随重定向；新增
`TestCookieUsesCanonicalTokenListPath` 保护此行为。上述响应不构成认证成功证据。
all-api-hub 格式依据固定提交 `949bd8e2ec8b15d16475617e5e4e6b8b4923d441`，见 Issue。

### 当前本地运行（2026-10-08，Asia/Shanghai）

- `v1.0.9-dev-cookie` 在 `http://127.0.0.1:18391/admin/ui/accounts` 运行，`/healthz` 与账号页面检查通过。
- 本地使用库 `sla_browser_local` 经真实启动路径从 032 升至 033；迁移前旧密码表为 **0 行**，没有删除已存上游凭据。最终 Cookie 表仅含 `account_id,enabled,cookie_ciphertext,state,updated_at`。
- 测试库 `sla_browser_test` 与本地库分开；PG 容器 `sla-browser-26-local-pg` 继续运行，仅映射本机 `18439`。数据库卷和仓库外密钥保留。
- 周期采集未启用（`-collector=false`），WebDAV 同步未启用；没有自动访问首站账号。测试 core `18392` 和测试 Chrome 已停止。
- 功能保留在 `codex/release-v1.0.9`，未推送、未创建标签或 Release。

### 尚未验收

1. 用户完成登录/2FA 后，真实 Cookie 的首站身份、余额、已有 Key 及其他读取结果。Cookie 只通过本地管理页或导入提供，不发送到聊天。
2. 使用真实 Cookie 备份文件或真实 WebDAV 同步的端到端认证；共用导入的真 PG 测试不替代这项。
3. 完整 `verify/test-migrate.sh` 及 Python 文档/schema 检查：Conda 不可用，未运行默认 Python。直接真实迁移、Go 测试和已纳入脚本的新测试不等于整个脚本已执行。

## 以下为旧密码方案历史

以下记录只供追溯。用户需自行在原站完成登录和 2FA，再登记或导入 Cookie；Cookie
失效后重新导入，不执行账号密码登录。

## 旧方案阶段结论（历史）

任务 1 的凭据配置已实现并完成下述验证；完整迁移脚本尚未运行。任务 2–4 的真实登录、采集接入及浏览器部署未完成，**不得据此宣称首站登录成功或版本可发布**。

新增独立凭据表与 AES-256-GCM 存储、管理 PUT/DELETE、账号列表状态及现有抽屉控件。每次更新撤销会话；账号停启或用户 ID 变化使旧任务无法写回，站点地址/家族变化删除原站凭据。原令牌表、采集请求及远端 Key 创建路径未改动。

## 旧方案已执行验证（历史）

| 范围 | 结果 |
| --- | --- |
| 隔离 PostgreSQL 16 | 成功应用 001–032；4 个浏览器存储测试通过，覆盖加密保存/恢复、留空保留密码、更换用户名、清除、旧会话修订、账号停启、站点变化与 AAD/篡改拒绝 |
| 管理接口 | 3 个新增测试通过：认证、缺密钥 503、真实数据库的配置生命周期；拒绝多余/尾随 JSON、只读写入，秘密不回显 |
| 构建 | `make build VERSION=v1.0.9-dev` 退出 0，包含前端 lint/typecheck/build 和三个 Go 二进制 |
| Go 竞态测试 | 首次 `make test` 与构建并行，admin/core 因嵌入的旧前端文件被替换而编译失败；其他包通过。构建结束后仅补跑 `go test -race ./cmd/sla-core ./internal/admin`，通过。新增存储测试及最后简化的密码保存逻辑分别增量验证通过 |
| 前端 | `cd web && pnpm test` 退出 0；真实 Chrome 配置检查在 1280px/375px 均通过，覆盖保存、待验证状态、关闭清空密码、重开及清除 |
| Compose | 两份配置解析通过，新可选密钥传给 core；根目录 Compose 的 collector 不接收此阶段不使用的密钥。未修改默认发布镜像版本 |

真实数据库测试使用仅本任务创建的本机隔离实例，通过 `SLA_TEST_DSN` 显式指定；没有读写业务数据库。Chrome 检查输入为本项目管理配置的测试数据，没有模拟或访问上游。未使用真实站点密码，也未记录秘密截图或请求体。

旧方案检查入口（仅供追溯，不作为 Cookie 验收；环境变量须指向可写的隔离测试实例）：

```sh
go test -race ./internal/store ./internal/admin -run '^TestBrowser' -count=1 -v
node verify/ui/verify-browser-credentials.mjs
```

Go 测试需 `SLA_TEST_DSN`；Chrome 检查需 `BASE`、`ADMIN_TOKEN`、`SLA_TEST_DSN`，默认使用本机 Google Chrome。后者在指定实例创建配置测试记录，**不得对业务部署执行**。

## 旧方案未验证项目（不再推进）

1. `./verify/test-migrate.sh` 和 Python 文档/schema 校验未执行：当前 Conda 不可用，遵守 Python 隔离要求，未运行默认 Python。新增测试已纳入既有迁移脚本；直接真实迁移和 Go 测试不替代完整脚本。
2. `https://api.lyjxka.top/` 只检查过公开登录页面，未提交真实登录。原定的账号密码配置与登录验证已取消；后续使用用户完成 2FA 后提供的 Cookie 验证，不能把公开页面可访问视为认证成功。
3. 旧方案未实现测试登录按钮、自动登录/读取切换、会话过期重登或 Chromium 镜像；这些工作已取消。真实余额、已有 Key、分组、价格和模型目录仍未验证。CF/Turnstile 等验证不在实现范围。

旧方案不再推进或发布；完成新的 Cookie 方案与真实验收后随下一版本一并上线。

## 本地运行复验（2026-10-08 13:27，历史记录）

- `70e248bf` 构建为 `v1.0.9-dev+70e248bf`，`make build` 通过。服务在 `http://127.0.0.1:18391/admin/ui/accounts` 运行，健康检查通过，周期采集未启用。
- PostgreSQL 16 仅映射本机 `18439`，本地使用库 `sla_browser_local` 与自动测试库 `sla_browser_test` 分开；两库均经真实启动路径应用 32 个迁移。密码及密钥在仓库外保存，目录权限 700、秘密文件权限 600；本地库使用独立持久卷。
- `go test -race ./internal/store ./internal/admin -run '^TestBrowser' -count=1 -v`：4 个存储测试、3 个管理接口测试通过，没有跳过。
- `verify-browser-credentials.mjs` 对独立测试服务执行，1280px 和 375px 均通过。测试服务 `18392` 已停止，本地使用服务与打开的 Chrome 保留运行。
- 本地已登记首站 `api.lyjxka.top` 并打开空白凭据表单，未提交上游登录、未读取真实余额或 Key。自动登录/采集未接入；Python/Conda 相关检查仍未执行，本轮结果不改变上述未验证边界。
