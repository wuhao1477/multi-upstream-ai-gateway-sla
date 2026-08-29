# verify/ 导航

`verify/` 下住着**两批互不相干**的东西。本文余下部分只讲第二批（历史 harness），
所以先给一张表，免得找不到第一批：

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

## 二、ISSUE-001 运行时 harness（历史，见下文全部章节）

> ⚠️ **2026-07-25 定位变更**：项目已转向**彻底自研上游对接层**、移除 AxonHub/ccLoad（见 [11 转向决策](../docs/dev/11-decision-full-selfbuilt.md)）。
>
> - **本 harness 的 AxonHub 部分转为历史记录**：其六假设验证结论（尤其假设 3 首字非可见内容、Responses round-trip 吞 reasoning）是**转向的直接依据**，不再作为升级准入门禁。
> - **`mock_upstream.py` 仍然有效且升值**：其场景集（role-only 首帧 / 空 SSE / 慢首帧 / 心跳 / 500 / abort）**转为自研透传层的测试夹具**，用于验证内容感知首字判定（AC-31）与取消传播（AC-32），见 [03 §10](../docs/dev/03-upstream-layer.md)。

在**你本机（装了 Docker Desktop 的 Mac）**上跑，用来把 [ISSUE-001](../docs/issues/ISSUE-001-tech-assumption-verification.md) 的 6 项假设从"源码级"收口到"运行时"。跑完用 `cleanup.sh` 清理干净。

> 说明：本 harness 已于 2026-07-23 在本机 Docker（OrbStack / AxonHub `v1.0.0-beta5` / SQLite）**实跑完成**，逐项结论回填在 [ISSUE-001](../docs/issues/ISSUE-001-tech-assumption-verification.md) 文末。脚本已据 beta5 的实际 GraphQL schema 适配（端点 `/admin/graphql`、relay guid、渠道需 `updateChannelStatus` 启用、`usageLogs`、`saveChannelModelPrices` 定价等，详见 ISSUE-001「beta5 schema 适配」表）。今后升级 AxonHub 重跑时若报字段/端点错误，按该表核对修正。

## 前置

- Docker Desktop 运行中。
- 端口 8090 空闲。

## 步骤（约 5 分钟）

```bash
cd verify

# 1) 拉起 AxonHub(SQLite) + mock 上游（无需构建，拉预构建镜像）
docker compose -f docker-compose.verify.yml up -d

# 2) 初始化系统 + 建两个渠道 + 建 Key + 把 Key 的 active profile 锁到渠道A
python3 setup.py

# 3) 跑 6 项假设的运行时探测，输出整段贴回对话
chmod +x run_tests.sh && ./run_tests.sh > result.txt 2>&1 ; cat result.txt

# 4) 清理（停容器、删卷/网络/镜像、删 state.json）
chmod +x cleanup.sh && ./cleanup.sh          # 保留 python 基础镜像
# ./cleanup.sh --all                          # 连 python:3.12-alpine 一起删
```

## 文件

| 文件 | 作用 |
| --- | --- |
| `docker-compose.verify.yml` | AxonHub(`v1.0.0-beta5`，SQLite) + Python mock 上游 |
| `mock_upstream.py` | stdlib SSE mock：role-only 首帧、空 SSE、慢首帧、心跳、500 失败 |
| `setup.py` | 初始化系统、建渠道A/B、建 Key、锁单渠道 profile，产出 `state.json` |
| `run_tests.sh` | 6 项假设的运行时探测，带时间戳的 SSE + execution 记录查询 |
| `cleanup.sh` | 全量清理 |

## 各假设对应观察点

| # | 假设 | 怎么判定 |
| --- | --- | --- |
| 1 | 单渠道隔离 | `mock-500`（渠道A）失败后不切到渠道B → 返回错误而非成功 |
| 2 | 取消对账 | `mock-slow-first` 2s 断开后，该 execution.status = `canceled` |
| 3 | 首字定义 | `mock-normal` 的 role delta 与首内容间隔 0.5s；记录的 `metricsFirstTokenLatencyMs` 若≈0 则=首事件（证实源码判断），≈500 则=首内容 |
| 4 | 账本关联 | execution 有 `externalID`、`metrics*`、usage 有 token/cost 且可关联到 request |
| 5 | 权限顺序 | 需配额场景，见 ISSUE-001；可另配 quota 渠道扩展 |
| 6 | ccLoad 备选 | 独立仓库，另起 compose，见 ISSUE-001 |

## 版本说明

镜像默认 `looplj/axonhub:v1.0.0-beta5`（最接近评估提交 `ed6119a1` 的发布）。若要严格对齐评估提交，按仓库根 `Dockerfile` 在该提交自行构建镜像并替换 compose 中的 image。源码级预验证已固定在 `ed6119a1`。
