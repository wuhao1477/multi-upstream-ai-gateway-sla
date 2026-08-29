# 项目规则

## 1. 验收只对真实依赖,禁止 mock

**不允许为验收构造假上游、假数据库、假响应。** 有什么功能就验什么功能;
验不了的功能,如实说验不了,不要用一个假的把它填绿。

这条不是风格偏好。**mock 是照实现者对协议的理解写出来的,所以它永远不会
推翻那个理解。** 曾经的 `verify/mock_newapi.py` 返回什么形态,取决于我读
`docs/dev/04` §3.1 读出了什么;真站点返回别的,全套验收(现为 14+48+31 = 93 项)
全绿也发现不了。
[`docs/acceptance/P1-evidence.md`](docs/acceptance/P1-evidence.md) §3 里
"ASXS 真实响应形态从未确认"就是这个坑的
现成例子 —— 它一直挂在那儿,而验收一直是绿的。

**这条规则不是我发明的,是本仓库自己撞出来的**,写规则之前已有两处独立结论:

- [07 §3 → §3bis](docs/dev/07-axonhub-runtime-probes.md)(2026-07-25):§3 用 mock
  推断出"标准字段够用、无需自研转换",§3bis 换真上游一测,AxonHub 丢弃 **80%
  顶层字段**。§4 写明了原因 —— **mock 响应本身就不含这些字段,所以 mock 永远
  暴露不了字段丢失**。这是本规则的原始证据,比规则早一个月。
- [ISSUE-003](docs/issues/ISSUE-003-runtime-feedback-requirement-candidates.md):
  "mock 伪造不了订阅面板协议,需真实渠道"。

所以这两篇里的 mock 提及**一律保留**:它们是"当时用了什么方法"的史实,
改写会让翻案读不通,而那段翻案正是本条规则的论据。

具体约束:

- **上游站点**:用 all-api-hub 导出里的真站点与真凭证(`HUB_FILE`)。
  站点挂了就让验收红,并打印是哪个站、什么状态码 —— 那是真实信息,
  不是噪声。**不要退回假上游,一次都不要。**
- **数据库**:用真 postgres(容器或内网库),不用 sqlmock。
  `internal/store` 的单测覆盖率低是正常的:它的代码几乎全是 SQL,
  由 `test-integration` / `test-api` / `test-ui` 打真库覆盖。
  给它加 sqlmock 只会得到"测试 mock 是否返回了我让它返回的东西"。
- **浏览器**:用真 Chrome(puppeteer-core 连本机 Chrome),不用 jsdom。
- **Go 单测的 `httptest` 假上游**:本条同样管它们(全仓 14 处,集中在
  `internal/collector/adapters_test.go`、`detect_test.go`、`newapi_pricing_test.go`)。
  单测要快要离线,所以 `httptest` 本身不禁 —— 禁的是**用它举证"协议长什么样"**。
  两类要分清:
  - **行为类**(占多数,属例外):余额取不到、数字以字符串返回、缺
    `quota_per_unit`、401 可区分、token 轮换、限流器计数……这些输入真站点
    不肯按需产出,造是对的。
  - **形态类**(不属例外):`len(keys)==2`、`GroupRef=="vip"` 这种断言只能证明
    夹具与解析器互相自洽。形态的举证责任归 `verify/pick-upstream.mjs` 的真站点
    校验;夹具里的形态**必须标注实测来源**,不能照文档或照想象写。

  写新夹具前先问:**这个形态我实测过吗?** 没实测过就先去打真站点。这坑本仓库
  踩过两次:`adapters_test.go` 的 `/api/pricing` 夹具原先照 `04 §3.1`(已过时)
  的 dict 形态写,单测全绿而真站点一个价格都采不到(注释还在,2026-08 已修);
  我写 picker 时照想象只认 `data.records`,真站点给的是 `data.items`,于是在
  明明有 token 的站上报"没有任何 token"。后者补了
  `TestNewAPIFetchKeysPagedEnvelope`(2026-08-29),形态取自实测。

**唯一例外**是那些"被测对象本身就是假的"的场景 —— 这时假的那一端**就是**
被测输入,不是被测依赖。判据只有一条:**真依赖能不能按需产出这个输入。**
不能,才轮到造。写这种用例要在注释里说明为什么它不违反本条。

已认定属于例外的三处(改动前先读这里,别再重新论证一遍):

| 位置 | 造的东西 | 为什么真依赖给不了 |
|---|---|---|
| `verify/ui/verify-ui.mjs` 的 `SECRET` | 一把假 Key 明文 | 要验的是"明文不回显"。拿真 Key 试等于把真凭证写进 DOM 快照与 CI 日志,失败时漏得更彻底 |
| `verify/mock_upstream.py` | 病态 SSE 流(role-only 首帧 / 空 SSE / 慢首帧 / 心跳 / 500 / abort) | 真站点不会按你的要求在指定时刻断流。见 [`verify/README.md`](verify/README.md) 与 [03 §10](docs/dev/03-upstream-layer.md),用于 AC-31 首字判定与 AC-32 取消传播 |
| 畸形上游响应的韧性用例 | 坏字段 / 坏 JSON | 同理:真站点不肯按需返回坏数据 |
| AC-36 压测的流发生器 | 1000 QPS × 60s 的流 | 6 万次请求打别人家站点等于攻击,会被封号且真花钱。且被测对象是**自研核心的吞吐**,上游只是流的来源。见 [PRD AC-36](docs/PRD.md) |

注意第二、三处造的都是**流与字节**,不是**站点**。"站点 A 存在且 quota_per_unit
是 500000"这类事实,永远只能由真站点提供 —— 这是 `mock_newapi.py` 被删的原因,
也是它与 `mock_upstream.py` 的分界。

### 后果:哪些验收在 CI 里跑不了

真依赖意味着 CI 受真依赖的可达性限制,这是这条规则的代价,不要用 mock 抹平:

| 验收 | CI 能跑 | 原因 |
|---|---|---|
| detect / pricing / model_catalog | ✅ 免密 | `/api/status` 与 `/api/pricing` 公开 |
| account / keys / groups | ❌ 只在本地 | 要 `Authorization` + 用户 ID 头,而真上游令牌**不进 GitHub secrets**(2026-08-29 决定) |
| 真库只读验收(31 项) | ❌ | GitHub runner 到不了内网 <internal-db-host> |

**真上游令牌不进 secrets 是明确决定,不是待办。** 那是别人家站点的真凭证,
放进 CI 就等于把它摊给每个能看 workflow 日志的人,以及每个能往仓库推分支的人
(`pull_request` 事件下 fork 也能触发)。泄露的后果由站主承担,而收益只是让
三项验收在云上也绿 —— 不值。

于是 `test-ui.sh` 的 62 项与真库 31 项都是**本地验收**,结论写进 PR/提交说明。
CI 只把免密那部分当回归网。**不要因为 CI 跑不了就换成 mock 让它在 CI 里绿** ——
那样得到的绿是假的,而真正的覆盖仍然为零。

## 2. 前端

技术栈与钉版理由见 `web/package.json` 与 `web/pnpm-workspace.yaml` 的注释。
包管理器统一 **pnpm**,`packageManager` 字段说了算。

改前端用 `cd web && pnpm dev`(:5173 热更新,`/admin/*` 代理到
`SLA_DEV_BACKEND`);`verify/dev-ui.sh` 起的是静态产物,改一行要重跑。

**`make build` 依赖 `make web`。** 跳过前端不会有编译期报错(`webdist/` 里
有 `go:embed` 占位文件),失败点会推迟到运行时 `/admin/ui` 返回 500。
