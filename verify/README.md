# verify/ 导航

## 一、P1 验收（现役，日常用这些）

发布镜像部署的配置校验：

```bash
./test-compose-release.sh
```

这条命令只运行 `docker compose config`，检查根目录 `compose.yml` 的服务、镜像、管理端口、collector entrypoint 和必填密钥约束；它不会启动容器，也不会访问真实数据库或上游渠道。

公开仓库安全基线：

```bash
./test-public-safety.sh
```

它扫描当前 Git 内容中的 JWT、带密码 DSN、私钥、长 Bearer 令牌、内网数据库标识和已知真实验收站点，并检查本地备份/架构图文件不会被 Git 跟踪。

| 入口 | 跑什么 | 需要 |
| --- | --- | --- |
| `ui-stack.sh` | 真 Chrome 点管理界面：SPA 17 项 + 功能 58 项 + Key 明文不进日志 1 项 | Chrome、node、PG；**`HUB_FILE`**（缺了就降级只跑 SPA 17 项并打印跳过了什么） |
| `ui-stack.sh --keep` | 同一套栈，起完停在前台供**人工**点验，打印真上游地址与凭证 | 同上但不需要 Chrome；`HUB_FILE` 必填（手点的意义就是对真站点点） |
| `test-arm-cloud.sh` | 把云端 arm64 镜像拉回本地真机验收 | gh、docker、`HUB_FILE` |
| `remote-stack.sh` | 连内网真库的只读验收 32 项（起栈/拆栈/防打在旧进程上） | Chrome、node、`DATABASE_URL` 到得了 <internal-db-host> |
| `pick-upstream.mjs` | **探活选真上游**，被上面三个共用 | `HUB_FILE` |
| `test-compose.sh` / `test-migrate.sh` / `test-config-api.sh` / `gate.sh` | 全栈冒烟 / 迁移 / 配置 API / 文档门禁 | PG、docker |

上游一律**真站点**，由 `pick-upstream.mjs` 从 all-api-hub 导出里现场探活挑选，
不写死 URL（[CLAUDE.md §1](../CLAUDE.md)）。真上游令牌**不进 GitHub secrets**，
所以要凭证的 58 项与真库 35 项**只在本地跑**，CI 只跑免密部分。

`remote-stack.sh` 是 2026-08-31 补的包装。此前这套验收手起 `./bin/sla-core &`、
手跑 node、手 kill —— 评估分支时发现 8-30 22:47 起的那个 core 一直挂着占 18390。
泄漏是小事，**它让这套验收能跑在旧二进制上并全绿**才是问题：脚本只连 `BASE`，
从不校验答话的是谁。包装的第 1 步与第 4 步就是这两条断言（端口被占则拒绝并点名
占用者；起栈后核对 `LISTEN` 的 pid 就是自己起的），两条都逐条验过红。

`ui-stack.sh` 由原 `test-ui.sh` 与 `dev-ui.sh` 合并（2026-08-30）。
**人工点验不是自动验收的冗余**：P1-evidence §4 第 15 项（先点采集再登记凭证，
本地失败也起算 60s 限流窗口）就是手点翻出来的，当时脚本化验收 33 项全绿。
合并的理由不是省那 47 行重复，而是消掉**分叉** —— 两份脚本解析
`pick-upstream.mjs` 输出的写法已经岔开（一边 7 行复制粘贴的 `node -e`，
一边抽了 3 行 `rd()`），这类分叉会继续长。两种模式的端口、容器名、数据目录
全部错开，可同时跑。

## 二、造流/造观测点的两个夹具

这两个文件造的是**流与字节**，或**一台记录用的服务端**，不是**站点**。
判据见 [CLAUDE.md §1](../CLAUDE.md) 的例外表：真依赖能不能按需产出这个输入。

| 文件 | 造什么 | 为什么真依赖给不了 |
| --- | --- | --- |
| `sse_stream_fixture.py` | 病态 SSE 流：role-only 首帧 / 空 SSE / 慢首帧 / 心跳 / 500 / abort | 真站点不会按你的要求在指定时刻断流。用于 AC-31 首字判定与 AC-32 取消传播，场景表见 [14 §1bis](../docs/dev/14-acceptance-matrix.md) |
| `probe_codex_wire.py` | 一台只记录不干活的服务端 | 被测对象是**真实 Codex CLI 发出的字节**。真上游收得下请求，但不会把原始请求给你看 |

```bash
python3 sse_stream_fixture.py 8091     # 独立起，不需要 compose
python3 probe_codex_wire.py            # 起在 :8899，把 Codex 指过来
```

`sse_stream_fixture.py` 原名 `mock_upstream.py`，2026-08-29 更名 —— 旧名让人
读成"假上游"，于是每处引用都得跟一句"这个不算 mock"。名字本身是误读的来源。

## 三、`probe_real_sites.py`（**已删除**，被 `coverage_report.py` 取代）

2026-08-29 删。它只打公开端点做 Detect 探测，而 `coverage_report.py`
对库中全部渠道跑**六项能力的全量 sync** —— 探测只是其中第一步，
两者重叠的部分由后者全覆盖，且后者的报告才是 P1 退出标准 ② 要的那份。

它当时测出、且现在只在这里留档的一件事：**NewAPI 系的 `quota_per_unit`
必须逐站读取，不可写死** —— 真实站点上这个值不止一种，而它是余额换算的分母，
写死会让部分站的余额差几个数量级。同批探测里另有若干站开着 turnstile 人机验证，
服务端采集对它们不可行（[04 §6](../docs/dev/04-collector-adapter.md)），
按 FR-011 转人工录入。

## 四、ISSUE-001 的 AxonHub harness（**已删除**）

`docker-compose.verify.yml`、`setup.py`、`run_tests.sh`、`cleanup.sh`
已于 2026-08-29 删除。它们拉起 AxonHub `v1.0.0-beta5` 并**把渠道指向假上游**，
而 AxonHub 已在 [11 转向决策](../docs/dev/11-decision-full-selfbuilt.md)
里被移出架构 —— 留着是一套指向已删依赖的死脚本，且是仓库里唯一还在
"造站点"的东西。

六假设的**结论**保留在 [ISSUE-001](../docs/issues/ISSUE-001-tech-assumption-verification.md)，
它们是转向的直接依据；要看当时怎么跑的，`git log -- verify/setup.py`。
