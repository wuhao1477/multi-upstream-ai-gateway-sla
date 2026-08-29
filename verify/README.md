# verify/ 导航

## 一、P1 验收（现役，日常用这些）

| 入口 | 跑什么 | 需要 |
| --- | --- | --- |
| `test-ui.sh` | 真 Chrome 点管理界面：SPA 14 项 + 功能 48 项 | Chrome、node、PG；**`HUB_FILE`**（缺了就降级只跑 SPA 14 项并打印跳过了什么） |
| `dev-ui.sh` | 起常驻栈供**人工**点验，打印真上游地址与凭证 | 同上，`HUB_FILE` 必填 |
| `test-arm-cloud.sh` | 把云端 arm64 镜像拉回本地真机验收 | gh、docker、`HUB_FILE` |
| `ui/verify-remote.mjs` | 连内网真库的只读验收 31 项 | 到得了 <internal-db-host> |
| `pick-upstream.mjs` | **探活选真上游**，被上面三个共用 | `HUB_FILE` |
| `test-compose.sh` / `test-migrate.sh` / `test-config-api.sh` / `gate.sh` | 全栈冒烟 / 迁移 / 配置 API / 文档门禁 | PG、docker |

上游一律**真站点**，由 `pick-upstream.mjs` 从 all-api-hub 导出里现场探活挑选，
不写死 URL（[CLAUDE.md §1](../CLAUDE.md)）。真上游令牌**不进 GitHub secrets**，
所以要凭证的 48 项与真库 31 项**只在本地跑**，CI 只跑免密部分。

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

## 三、ISSUE-001 的 AxonHub harness（**已删除**）

`docker-compose.verify.yml`、`setup.py`、`run_tests.sh`、`cleanup.sh`
已于 2026-08-29 删除。它们拉起 AxonHub `v1.0.0-beta5` 并**把渠道指向假上游**，
而 AxonHub 已在 [11 转向决策](../docs/dev/11-decision-full-selfbuilt.md)
里被移出架构 —— 留着是一套指向已删依赖的死脚本，且是仓库里唯一还在
"造站点"的东西。

六假设的**结论**保留在 [ISSUE-001](../docs/issues/ISSUE-001-tech-assumption-verification.md)，
它们是转向的直接依据；要看当时怎么跑的，`git log -- verify/setup.py`。
