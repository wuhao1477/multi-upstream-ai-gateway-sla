# 项目规则

## 1. 验收只对真实依赖,禁止 mock

**不允许为验收构造假上游、假数据库、假响应。** 有什么功能就验什么功能;
验不了的功能,如实说验不了,不要用一个假的把它填绿。

这条不是风格偏好。**mock 是照实现者对协议的理解写出来的,所以它永远不会
推翻那个理解。** 曾经的 `verify/mock_newapi.py` 返回什么形态,取决于我读
`docs/dev/04` §3.1 读出了什么;真站点返回别的,79 项全绿也发现不了。
`docs/dev/P1-evidence.md` §3 里"ASXS 真实响应形态从未确认"就是这个坑的
现成例子 —— 它一直挂在那儿,而验收一直是绿的。

具体约束:

- **上游站点**:用 all-api-hub 导出里的真站点与真凭证(`HUB_FILE`)。
  站点挂了就让验收红,并打印是哪个站、什么状态码 —— 那是真实信息,
  不是噪声。**不要退回假上游,一次都不要。**
- **数据库**:用真 postgres(容器或内网库),不用 sqlmock。
  `internal/store` 的单测覆盖率低是正常的:它的代码几乎全是 SQL,
  由 `test-integration` / `test-api` / `test-ui` 打真库覆盖。
  给它加 sqlmock 只会得到"测试 mock 是否返回了我让它返回的东西"。
- **浏览器**:用真 Chrome(puppeteer-core 连本机 Chrome),不用 jsdom。

**唯一例外**是那些"被测对象本身就是假的"的场景 —— 这时假的那一端**就是**
被测输入,不是被测依赖。判据只有一条:**真依赖能不能按需产出这个输入。**
不能,才轮到造。写这种用例要在注释里说明为什么它不违反本条。

已认定属于例外的三处(改动前先读这里,别再重新论证一遍):

| 位置 | 造的东西 | 为什么真依赖给不了 |
|---|---|---|
| `verify/ui/verify-ui.mjs` 的 `SECRET` | 一把假 Key 明文 | 要验的是"明文不回显"。拿真 Key 试等于把真凭证写进 DOM 快照与 CI 日志,失败时漏得更彻底 |
| `verify/mock_upstream.py` | 病态 SSE 流(role-only 首帧 / 空 SSE / 慢首帧 / 心跳 / 500 / abort) | 真站点不会按你的要求在指定时刻断流。见 [`verify/README.md`](verify/README.md) 与 [03 §10](docs/dev/03-upstream-layer.md),用于 AC-31 首字判定与 AC-32 取消传播 |
| 畸形上游响应的韧性用例 | 坏字段 / 坏 JSON | 同理:真站点不肯按需返回坏数据 |

注意第二、三处造的都是**流与字节**,不是**站点**。"站点 A 存在且 quota_per_unit
是 500000"这类事实,永远只能由真站点提供 —— 这是 `mock_newapi.py` 被删的原因,
也是它与 `mock_upstream.py` 的分界。

### 后果:哪些验收在 CI 里跑不了

真依赖意味着 CI 受真依赖的可达性限制,这是这条规则的代价,不要用 mock 抹平:

| 验收 | CI 能跑 | 原因 |
|---|---|---|
| detect / pricing / model_catalog | ✅ 免密 | `/api/status` 与 `/api/pricing` 公开 |
| account / keys / groups | ⚠️ 需 secret | 要 `Authorization` + 用户 ID 头 |
| 真库只读验收(31 项) | ❌ | GitHub runner 到不了内网 <internal-db-host> |

到不了的那些,在本地跑并把结论写进 PR/提交说明。**不要因为 CI 跑不了就
换成 mock 让它在 CI 里绿。**

## 2. 前端

技术栈与钉版理由见 `web/package.json` 与 `web/pnpm-workspace.yaml` 的注释。
包管理器统一 **pnpm**,`packageManager` 字段说了算。

改前端用 `cd web && pnpm dev`(:5173 热更新,`/admin/*` 代理到
`SLA_DEV_BACKEND`);`verify/dev-ui.sh` 起的是静态产物,改一行要重跑。

**`make build` 依赖 `make web`。** 跳过前端不会有编译期报错(`webdist/` 里
有 `go:embed` 占位文件),失败点会推迟到运行时 `/admin/ui` 返回 500。
