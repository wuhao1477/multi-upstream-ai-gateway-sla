# 增加 Cookie 登记、all-api-hub 导入与会话采集

## 当前决定（2026-10-08）

替代原账号密码登录方案：不接收、不存储、不提交上游登录账号密码，不代办 2FA。用户在原站自行完成登录与 2FA，之后提供 Cookie。现有令牌采集保留；新增认证能力仅使用 Cookie。

用户已恢复实施。`codex/release-v1.0.9` 已将旧密码配置替换为加密 Cookie 登记、导入、验证与受限读取；迁移 033 删除旧密码/会话表，已应用的 032 保留不改写。构建、核心测试及 1280px/375px 真 Chrome 表单检查已执行，具体结果见[验收记录](../acceptance/browser-session-collection.md)。用户手动登记的 Cookie 已通过首站身份、余额与已有 Key 读取验收，缺失用户 ID 已自动补齐，4 把 Key 的额度及采集时间已保存；真实 all-api-hub 备份认证尚未验收，当前不发布。

## 最小方案

1. 在原凭据抽屉登记、替换、启停、清除 Cookie；增加“验证 Cookie”，不提供“测试登录”。保存只保存，验证只读取身份，不发起登录。Cookie 加密存储，界面只返回存在性与状态。
2. Cookie 绑定已登记的 HTTPS 站点和本地账号；已有上游 User ID 时必须匹配，不自动改绑。缺少 ID 时，从 NewAPI 默认签名 `session` Cookie 的 Gob 内容读取候选值，再用原 Cookie 与该 ID 请求 `/api/user/self`；响应身份匹配后，才在同一事务中保存 ID 与可用状态。验证期间 Cookie 或账号变化时拒绝写入。不能识别的格式提示导入带 `account_info.id` 的完整 all-api-hub 备份，不猜测 ID。上游账号密码、验证码、2FA 密钥均不需要。
3. 原令牌请求优先；无令牌或明确认证不可用时，单次切换到 Cookie 读取。身份验证复用已有 7 个用户 ID 头候选，仅明确鉴权失败才尝试下一个；成功后本轮复用命中头名，不新增存储字段。首次登记 Key 即保存已采集元数据。Cookie 与 Authorization 不混发；按账号、精确 origin 和现有读取端点隔离，禁止携带 Cookie 跟随重定向。
4. Cookie 失效或遇到额外验证后停止自动尝试，提示用户在原站完成登录后重新导入；网络故障、429、5xx、权限不足不触发登录或凭据替换。不自动续期，不持久化响应中的 Set-Cookie。
5. 首版直接复用 Go HTTP 客户端、适配器、限流与结果存储，不引入 agent-browser、Chromium、Node 服务或新队列。以后只有真实证据证明必须使用页面环境时，才另行设计浏览器 Cookie 注入；也不加入账号密码登录。

## all-api-hub 导入

已核实公开源码提交 [949bd8e](https://github.com/qixing-jk/all-api-hub/tree/949bd8e2ec8b15d16475617e5e4e6b8b4923d441)：

- `accounts.accounts[].authType == "cookie"`。
- Cookie 位于 `accounts.accounts[].cookieAuth.sessionCookie`，是 Cookie 请求头字符串。
- `site_url` 提供站点，`account_info.id` 提供上游 User ID；顶层账号 `id` 不是上游 User ID。
- 此字段不是完整浏览器 Cookie 导出，不携带 Domain、Path、Expires；扩展动态获取的 WAF Cookie 不保证在备份中。本功能不补造这些属性、不获取或绕过 CF／Turnstile。
- 文件上传、已有 WebDAV 同步共用 `ParseHubBackup → ImportHub`。Cookie 账号不再仅凭 `access_token` 判“无凭据”；缺少 Cookie 时明确提示，不冒充导入成功。
- 新建、补齐、已有账号更新和差异预览都支持 Cookie；同内容重导不改变修订/失效状态，缺失字段不清除本地 Cookie，重复同步不重新启用已停用配置。更换 Cookie 不覆盖令牌。
- 预览只报告变化，不能注入 Cookie、采集 Key 或写库；报告、同步历史、错误与日志不得包含 Cookie 明文。

依据：[类型定义](https://github.com/qixing-jk/all-api-hub/blob/949bd8e2ec8b15d16475617e5e4e6b8b4923d441/src/types/index.ts#L115)、[认证枚举](https://github.com/qixing-jk/all-api-hub/blob/949bd8e2ec8b15d16475617e5e4e6b8b4923d441/src/types/auth.ts)、[备份结构](https://github.com/qixing-jk/all-api-hub/blob/949bd8e2ec8b15d16475617e5e4e6b8b4923d441/src/services/importExport/backupContracts.ts)、[Cookie 字符串处理](https://github.com/qixing-jk/all-api-hub/blob/949bd8e2ec8b15d16475617e5e4e6b8b4923d441/src/utils/browser/cookieString.ts)。

## 开发顺序

- [x] 删除旧密码 UI/API 和运行时存取代码；新增前向迁移移除旧密码表，建立 Cookie 专用存储，复用现有加密与修订失效逻辑。已应用的 032 迁移不重写，不迁移旧密码为 Cookie。
- [x] 修改 all-api-hub 解析、新建/补齐/更新、差异统计与 WebDAV 共用导入，支持纯 Cookie 账号与相同站点多账号。
- [x] 接入身份校验、Cookie 读取与状态展示；账号列表、凭证汇总与筛选识别 Cookie-only 账号。保留正常令牌行为，不将 Cookie 用于远端 Key 创建。
- [x] 使用用户授权的真实 Cookie 完成首站身份、余额与已有 Key 验收；用户 ID 自动补齐，首次 Key 元数据写入有真实隔离 PG 回归。

## 验收

- [x] UI/API/运行时不再接受上游账号密码；迁移后的 Cookie schema 没有密码或旧会话列。
- [x] 真数据库测试通过：手工登记与 all-api-hub 共用导入支持加密 Cookie，缺密钥或字段不足明确失败；预览不使用 Cookie。此项不等于真实 Cookie 文件/WebDAV 认证已成功。
- [x] 首站 `https://api.lyjxka.top/` 使用用户提供的真实 Cookie 通过身份校验并读取余额与 4 把已有 Key；应用不处理登录或 2FA。
- [ ] 真实 all-api-hub Cookie 备份文件或 WebDAV 同步后的端到端认证；共用导入测试不替代真实备份验收。
- [x] 核心行为测试与真 Chrome 检查覆盖多账号隔离、ID 不匹配、Cookie 失效、停用/清除/站点变更、旧任务写回、秘密不回显和 375px；已成功的采集项不重跑。
- [x] 不登录、不创建/重置远端令牌、不扩展推理请求；无 CF/Turnstile/2FA 自动化。沿用真实 PostgreSQL 和 Chrome 验证，不用假上游举证协议正确。

仍在原目录和现有版本分支实施，不使用 worktree；完成 Cookie 方案后随新版本一并上线，当前不发布。
