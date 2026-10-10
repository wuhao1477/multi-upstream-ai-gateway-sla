# P1 发版就绪评估

> **历史验收快照。** 本文记录 2026-09-02、代码基线 `49d979ed2ed06da1f7b0e9e8689a9da543c35d1b` 的 P1 发布前验收。PR 状态和运行结果均为当时证据，不代表当前 HEAD 已重新验证。

| 项目 | 历史事实 |
| --- | --- |
| 评估对象 | 分支 `feat/p1-upstream-inventory`，代码基线 `49d979ed2ed06da1f7b0e9e8689a9da543c35d1b` |
| 评估日期 | 2026-09-02 |
| 一期范围 | 上游渠道采集与管理：渠道商列表与 CRUD、上游账号管理、Key 管理、渠道分组与倍率、模型目录、手动采集与资产总览 |
| 明确不含 | 网关请求转发、账本、P3 调度/SLA/告警/压测、P4 订阅 |
| 已确认取舍 | 保留 `ThemeSwitch.vue`；不轮换内网数据库密码；不纳入备份 JSON 与 `docs/superpowers/` |

## 当时结论

**记录时本地 P1 验收与门禁已通过；PR #14 当时满足合并条件。**

2026-09-02 已在两个显式授权的真 Sub2API 站点运行 `verify/ac38-sub2api.sh`。两次均完成建渠道、账号与凭证登记、真 Key 登记、手动采集和二次限流断言。最新一次结果为：账号 1 行、Key 3 行、分组 24 行、`quota_synced_at` 前进；模型目录与分组模型因上游未返回模型字段而为 0 行，响应均显式标记 `degraded` 并给出说明；第二次同步返回 429。

## 退出标准

| 标准 | 当前状态 | 证据 |
| --- | --- | --- |
| AC-37/38/39/40 全通过 | ✅ | `make test-ui` 当前工作树真实 Chrome `62/62`；两个真 Sub2API 站点的 AC-38 均通过 |
| 全部真实渠道完成一次全量 sync 并留存报告 | ✅ 已完成纳管范围的报告 | [`coverage/`](coverage/) 与 [`P1-manual-channels.md`](P1-manual-channels.md)；20 个不可采集站已停用并记录原因 |
| Key 明文不出现在响应、日志、抓包 | ✅ | 当前 UI 验收检查 DOM；核心日志检查通过；`secret_render_test.go` 负责 CI 源码守卫 |
| 每个已注册家族的 Capabilities 与实际返回一致 | ✅ | 注册表一致性测试、真 NewAPI 采集与两次真 Sub2API 端到端采集 |

## 已通过验证

| 验证 | 结果 |
| --- | --- |
| `make test-ui` | ✅ 真实 Chrome `62/62`；真实 NewAPI 站点；双账号、账号编辑/停用/启用、账号级凭证、四把 Key、Key 编辑/删除、分组倍率、模型目录、重复采集限流均通过 |
| `go test ./internal/admin ./internal/store` | ✅ |
| `pnpm --dir web test` | ✅ |
| `pnpm --dir web run build` | ✅ |
| `./verify/test-migrate.sh` | ✅ 迁移、P1 表结构、账号凭证唯一性、事务一致性和 Key 分组字段检查通过 |
| `node --check verify/ui/verify-ui.mjs` | ✅ |
| `git diff --check` | ✅ |

本轮另外执行并通过：`make check`、`make gen-check`、`make mig-check`、`./verify/gate.sh`、`./verify/test-verify-scripts.sh`。

## P1 内的非阻塞事项

1. Sub2API 真站点的模型目录当前可能是 0 行；适配器会标记为 `degraded` 并附说明，不静默报告为成功。该行为属于上游能力差异，不影响账号、Key、分组和额度采集。
2. 完整 SSRF 网段、DNS 重绑定和重定向限制仍绑定管理面安全边界；当前管理面为单一内网 admin token。引入 RBAC 或公网暴露管理面时，应升级为阻塞项。
3. 远端真库验收因本环境没有 `DATABASE_URL` 未重跑；不能把历史结果冒充当前 HEAD 证据。

### 2026-10-09 决策修订（Issue #30）

以上为历史决定。本次仅将**禁止自动重定向**提前实施：采集、探测与 WebDAV 默认
客户端拒绝跟随 301/302/303/307/308，令牌路径仍允许私网与回环直连，保留环境代理。
Cookie 的公网 HTTPS、逐 IP、同 origin 与只读路径限制不变。不新增私网白名单配置。
完整网段限制与 DNS 重绑定防护仍未实施，引入 RBAC 或公网管理面时仍升级为阻塞项。

### 2026-10-10 决策修订（Issue #33）

用户决定不再等待 RBAC/公网管理面这一触发条件，提前实施完整出站策略：采集、探测与
WebDAV 连接前校验全部解析地址、直接拨已校验的 IP，默认只连公网，不再使用环境代理；
内网目标由部署环境 `SLA_OUTBOUND_PRIVATE_TARGETS` 按主机名授权。上面第 2 条与
2026-10-09 修订中"仍允许私网与回环直连、保留环境代理、不新增私网白名单配置"的表述
自此失效。本修订不表示重新运行过历史 35 项验收。

历史回环夹具渠道已停用；`verify-remote.mjs` 的三项 Key 断言只读取已有数据，
不依赖已删除的假上游服务。本修订不表示重新运行过历史 35 项验收。

非 Cookie 凭证的加密存储（D1）已于 2026-10-10 由 #34 实施，见 [安全债登记 D1](security-debt.md)；
生产库的迁移仍需按 06 的升级顺序在维护窗口执行。

## 合并门禁

本轮已执行并通过：

```bash
make fmt
make gen-check
make mig-check
make check
./verify/test-migrate.sh
pnpm --dir web test
pnpm --dir web run build
git diff --check
```

全部命令成功，PR 远端 `gate`、`build`、`arm64` 均为绿色；P1 当时满足合并条件。当时不需要轮换数据库密码。

记录时 PR #14 的远端 head 与评估分支最新提交一致，状态为 `OPEN`、非 Draft、`MERGEABLE`、`CLEAN`。
