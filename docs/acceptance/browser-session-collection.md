# Cookie 登记、导入与读取验收记录

日期：2026-10-08。Issue：[#26](https://github.com/wuhao1477/multi-upstream-ai-gateway-sla/issues/26)。分支：`codex/release-v1.0.9`，在原目录开发，未创建 worktree。

## 当前状态（2026-10-08 Cookie 实施）

账号密码登录方案已撤回并从运行时代码移除。现已实现加密 Cookie 登记、all-api-hub
共用导入、身份验证与受限读取；保留正常令牌路径，不添加浏览器运行时。需求见
[Issue #26](../issues/ISSUE-browser-session-collection.md)。

用户已在本地管理页手动登记 Cookie。2026-10-08 通过应用验证接口完成首站身份
验证，缺失的上游用户 ID 已自动补齐，账号 1 的 Cookie 状态由 `needs_action`
变为 `ready`。随后修复换算基数和首次 Key 元数据保存：账号 1 保留 4 把 Key，
本轮重新同步后额度与采集时间均完整，失败 0。首站余额、Key、分组、价格和模型
目录读取已实测；分组仍报告 `available_models` 缺失，真实 all-api-hub 备份导入
尚未验收。不把旧密码阶段的结果计入 Cookie 验收。

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

- `v1.0.9-dev-cookie-sync` 在 `http://127.0.0.1:18391/admin/ui/keys` 运行，`/healthz` 检查通过；管理页沿用上一轮已通过浏览器检查的前端产物，本轮只改 Go 采集流程。
- 本地使用库 `sla_browser_local` 经真实启动路径从 032 升至 033；迁移前旧密码表为 **0 行**，没有删除已存上游凭据。最终 Cookie 表仅含 `account_id,enabled,cookie_ciphertext,state,updated_at`。
- 测试库 `sla_browser_test` 与本地库分开；PG 容器 `sla-browser-26-local-pg` 继续运行，仅映射本机 `18439`。数据库卷和仓库外密钥保留。
- 周期采集未启用（`-collector=false`），WebDAV 同步未启用；已通过应用完成首站已存 Cookie 的身份验证、账号 1 的已有 Key 同步及渠道采集。测试 core `18392` 和测试 Chrome 已停止。
- 功能保留在 `codex/release-v1.0.9`，未推送、未创建标签或 Release。

### 尚未验收

1. 首站身份、余额、已有 Key、分组、价格和目录读取已通过；分组 `available_models` 仍缺失并报告 degraded，26 个目录模型尚未登记为可路由模型。其他二开站的自定义用户 ID 头只有行为回归，没有真实站点认证验收。
2. 使用真实 Cookie 备份文件或真实 WebDAV 同步的端到端认证；共用导入的真 PG 测试不替代这项。
3. 完整 `verify/test-migrate.sh` 及 Python 文档/schema 检查：Conda 不可用，未运行默认 Python。直接真实迁移、Go 测试和已纳入脚本的新测试不等于整个脚本已执行。

### 验证按钮反馈修正（2026-10-08）

- 实测验证接口快速返回 422，但原全局提示位于抽屉下方，被固定底部遮挡；用户页面也仍加载旧脚本，刷新后恢复最新凭证显示。
- 仅修改凭证面板：显示「验证中…」、请求及列表刷新期间禁止重复提交、将结果保留在固定底部；缺用户 ID 时直接给出编辑说明，不发送请求。修改 Cookie 或切换账号后清除旧结果。不改变后端认证、存储或采集规则。
- 先观察到「结果未保留在面板」及「矮窗口下结果不可见」两条回归断言失败，再修正。隔离实例 `18392` 的真实浏览器检查通过：等待状态、真实 `.invalid` 网络失败 502、结果保留、编辑清除旧结果、缺 ID 时验证请求数为 0。最终在本地账号页面确认固定底部提示未被遮挡，未读取或改写用户 Cookie。
- `make build VERSION=v1.0.9-dev-cookie` 通过；最后调整提示位置后，增量前端构建及 `sla-core` 构建通过。更新了 `verify-cookie-credentials.mjs` 的对应断言并检查语法；本次没有重新运行完整旧验收套件或全量 Go 测试。
- 本地 `18391` 已加载修正后的界面；测试服务 `18392` 已停止，周期采集仍关闭。修改保留在 `codex/release-v1.0.9`，未推送或发布。

### Cookie 用户 ID 自动识别修正（2026-10-08）

- 根因是本地两个前置条件：适配器和 Cookie 请求都要求已有 `external_user_id`，前端随后也增加了同样的阻断。缺少 ID 不代表 Cookie 无效，旧文案“否则必然 401”不是此次原站返回结果。
- 首站公开 `/api/status` 报告 `v1.0.0-rc.21`。[该版本会话初始化](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.21/main.go#L191)使用仅签名的 CookieStore，[登录会话](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.21/controller/user.go#L138)写入用户 ID。解析只作为候选；必须用原 Cookie 请求 `/api/user/self`，身份一致后才保存。
- 验证和采集共用身份发现路径。已登记 ID 不改绑；新 ID 与可用状态在事务中写入，并采用身份变更触发器产生的新修订。Cookie/账号变化、401、身份不匹配均不写入推测 ID。没有新增依赖、浏览器运行时或迁移。
- 新增回归测试先复现失败，再通过。使用独立真实 PG 执行 `go test -race ./internal/collection ./internal/collector ./internal/store ./internal/admin -run Cookie -count=1 -v`，四个包均通过、无跳过；最后标准库简化只增量验证解析与鉴权用例。相关包 `go vet` 通过。
- `make build VERSION=v1.0.9-dev-cookie-id` 通过；最后 Go 简化后重新编译 core/collector，通过。现有真 Chrome 脚本在独立实例 `18392` 的 1280px、375px 均通过；空 ID 可以发起验证，不可识别的测试 Cookie 得到明确 422 原因，且账号 ID 保持为空。
- 本地 `18391` 通过应用验证接口复测账号 1 已保存的 Cookie：验证前无用户 ID、状态 `needs_action`；接口返回 `200 {"state":"ready"}`，随后确认用户 ID 已存在、状态 `ready`。未在工具输出中读取、显示 Cookie 或原站响应正文；未执行余额与 Key 采集。测试服务已停止，周期采集保持关闭。

### Key 同步额度换算参数修正（2026-10-08 20:15，Asia/Shanghai）

- 根因：渠道 1 没有 `__detect__` 快照，Key 列表读取成功后因 `quota_per_unit=0` 无法换算额度；不是 Cookie 鉴权失败。公开 `GET https://api.lyjxka.top/api/status` 返回 `200`、`success=true`、`quota_per_unit=500000`。
- 共用凭证加载流程在 NewAPI 缺少换算基数时复用既有探测与快照保存逻辑；Key 同步入口改用同一加载流程。已有值直接复用，获取失败或非正值明确报错，不设置默认基数。为保持文件不超过 500 行，将 `NewRunner` 原样移至同包 `runner_credentials.go`，没有新增抽象、依赖或迁移。
- 新增两项回归先复现失败，再修复通过，覆盖自动获取、持久化复用、Cookie 账号信息保留，以及字段缺失、零值和公开接口失败。独立真 PG 下 `go test -race ./internal/collection ./cmd/sla-core -count=1 -v` 退出 0，collection 无跳过；core 无测试文件。相关包 `go vet` 通过。同包移动后增量构建 core/collector 通过，未重复运行此前完整验收。
- 本地服务更新为 `v1.0.9-dev-cookie-quota`，只通过 `POST /admin/keys/import` 指定 `account_ids:[1]` 实测：`found=4, imported=4, failed=0, deferred=0`，账号状态 `ok`；真库确认登记 4 把 Key，并保存换算基数 `500000`。未创建远端 Key，未重新填写 Cookie，未在工具输出中显示 Cookie、Key 或原站响应正文。余额及其他采集能力仍未验收。

### 首次 Key 元数据与 Cookie 头名修正（2026-10-08 21:13，Asia/Shanghai）

- 新 Key 创建后复用已有 `updateImportedKey`，立即保存额度、无限额标志、有效期、限流和采集时间；写入失败计入失败，不报完整导入。真实隔离 PG 回归先复现“两把均无元数据”和“写入失败仍报成功”，修复后通过；没有删除用户 Key 来重现首次路径。
- Cookie 身份读取共用既有 7 个用户 ID 头候选，优先已有头名且不重复；仅明确鉴权失败继续尝试，全部失败后才标记失效。身份匹配后在本轮会话复用头名，不新增数据库列。每次仍限速、重读配置，尝试期间替换 Cookie 会中止旧任务；429、5xx、403、未知业务拒绝及身份不匹配不继续尝试。
- 新回归先观察到预期失败。`go test -race ./internal/collection ./internal/collector ./cmd/sla-core -count=1 -json` 退出 0：collection 75 项、collector 151 项通过（含子测试），无测试跳过；core 无测试文件。相关包 `go vet`、core/collector 构建及差异格式检查通过。本轮未改前端，不重复已有浏览器验收。
- 本地更新为 `v1.0.9-dev-cookie-sync`：Cookie 验证返回 `200/ready`；账号 1 同步返回 `found=4, imported=0, skipped=4, failed=0, deferred=0`，其中 skipped 表示已有行已更新、未重复登记。数据库确认 4/4 把 Key 的额度及采集时间已保存；首次插入由上述独立测试验证。
- `POST /admin/channels/1/sync` 返回 200：account 1、groups 6、keys 4、pricing 26、model_catalog 26，均为 `ok`。真库确认 1 条余额信号、1 条账号快照、6 个分组、26 个目录模型；Cookie 保持 `ready`，账号没有令牌凭据。分组仍有 `available_models` 缺失提示，价格只写快照，26 个模型未登记为可路由模型，不把这些限制抹成完整能力。
- 全程仅应用读取已保存 Cookie，未在工具输出显示 Cookie、Key 或原站响应正文；没有远端创建、账号密码登录、浏览器运行时或新迁移。真实 all-api-hub 文件/WebDAV 认证与其他二开站仍待提供相应真实输入后验收；未提交、推送或发布。

### PR 提交前检查（2026-10-08 21:51，Asia/Shanghai）

- 增量审查 Cookie 身份验证、凭据变更、导入及 Key 同步路径，未发现新的阻断问题；没有新增生产代码修改。
- `cd web && pnpm test`、`make build VERSION=v1.0.9-dev-cookie-sync` 通过；构建包含前端 lint/typecheck/build 与三个 Go 二进制。
- 构建完成后，使用独立真实 PG 执行 `go test -race ./... -count=1 -json`，退出 0。admin 132、collection 75、collector 151、config 8、health 5、store 69 项通过，共 440 项（含子测试），无测试跳过。
- `make fmt-check vet` 通过。前端未再修改，不重复已经通过的真实浏览器验收；本地服务继续运行，未重新采集。
- Conda 与 golangci-lint 在当前环境不可用，Python 文档/schema、完整迁移脚本及 CI lint 仍待 CI 验证；真实 all-api-hub 文件/WebDAV 认证等未验收边界保持不变。此次结论支持发起 PR 审查，不表示已可发布。

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
