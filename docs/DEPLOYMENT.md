# 部署指南

本文面向首次部署的开源用户。当前发布版本是 P1：上游渠道、账号、Key、分组、模型目录和采集管理。请求转发、账本、P3 调度/SLA/告警/压测和 P4 订阅尚未进入本版本。

## 1. 推荐部署：发布镜像 + Compose

### 前置条件

- Docker Engine 24+ 与 Docker Compose v2。
- 一台能访问 GHCR 的 Linux/macOS 主机。
- 管理端口（默认 18081）没有被其他程序占用。本栈不监听 80/443。
- **一个外部 PostgreSQL 16+**：库已创建，账号有建表权限（首次启动会执行迁移），且容器网络可达。本栈不自带数据库。

公开仓库和 GHCR 镜像无需登录即可访问。如果所在环境要求认证，再使用具备 `read:packages` 权限的令牌登录；不要把 Token 写入仓库或 `.env`。

### 首次启动

```bash
git clone https://github.com/wuhao1477/multi-upstream-ai-gateway-sla.git
cd multi-upstream-ai-gateway-sla
cp .env.example .env
```

编辑 `.env`，至少填写两个值：

```dotenv
DATABASE_URL=postgres://sla:<密码>@db.internal:5432/sla?sslmode=require
ADMIN_TOKEN=<管理面随机令牌>
```

数据库跑在宿主机上时，容器里的 `localhost` 指向容器自己，要写宿主机地址：macOS/Windows 用 `host.docker.internal`，Linux 写内网 IP 或给服务加 `extra_hosts`。

建议生成 URL-safe 随机值：

```bash
openssl rand -hex 32
```

启动发布镜像：

```bash
docker compose pull
docker compose up -d
docker compose ps
```

检查实例是否就绪：

```bash
curl http://127.0.0.1:18081/healthz
```

预期返回 HTTP 200，并包含 `status=ok`、`db=ok` 和 `config_snapshot=loaded`。容器自己也在探同一个端点（`/sla-core -healthcheck`），`docker compose ps` 显示 `healthy` 才算真就绪。

管理界面只绑定到本机回环地址：

```text
http://127.0.0.1:18081/admin/ui/
```

在界面中填写 `.env` 里的 `ADMIN_TOKEN`。整个栈不监听任何对外端口 —— 要从别的机器访问，请在前面放一层反向代理，并**只放行 `/healthz`**，`/admin/*` 与 `/metrics` 一律不代理（[09 §1](dev/09-admin-api.md) 冻结：网络边界 + 令牌两层都要）。

## 2. 服务拓扑

```text
浏览器/运维 ──> 127.0.0.1:18081 ──> sla-core :8080 ─┐
                                                     ├── 外部 PostgreSQL 16+
collector ──────────────────────────────────────────┘      （DATABASE_URL）
                   └──> 上游站点管理接口
```

| 服务 | 作用 | 宿主机端口 |
| --- | --- | --- |
| `sla-core` | 管理 API、管理 UI、`/healthz` | `127.0.0.1:18081` |
| `collector` | 周期采集上游账号、Key、分组和模型目录 | 不发布 |

数据库不在本栈内：备份、版本升级和高可用由数据库那一侧负责，栈里没有任何数据卷，`docker compose down` 不会删除业务数据。

数据库迁移和 P1 配置种子由 `sla-core` 启动时自动执行。`sla-core` 与 `collector` 都会跑 bootstrap，但 PostgreSQL 咨询锁保证只有一个真正执行迁移，所以不需要 `depends_on` 编排顺序。

### 这个拓扑放弃了什么

原 [06 §1](dev/06-deployment-and-operations.md) 拓扑是 Caddy + 2× sla-core + PG + collector。现在少了三样，每样的代价都是明确的：

- **没有第二个 `sla-core`，也就没有 AC-27 的进程级冗余** —— 重启、崩溃、升级都会中断服务。原来两个实例买到的就是这一条（宿主机、库仍是单点，从来没被冗余覆盖过）。要它就得加回第二个实例**和**一层带主动健康探测的 LB，缺一不可。
- **没有 Caddy，路径白名单就没有落点**。P1 阶段对外本来就只有 `/healthz`，所以现在不亏；但 P2 的 `/v1/*` 上线后，管理面与数据面共用同一个 `:8080` 监听器，届时**必须**二选一：把管理面拆成独立 listener（绑回环），或者把反代加回来。否则 `/admin/*` 会随数据面一起暴露，只剩 `ADMIN_TOKEN` 一层（[09 §1](dev/09-admin-api.md) 冻结：两层都要）。
- **没有主动健康探测**。Caddy 原来每 2 秒探一次 `/healthz` 并摘除故障实例。现在只剩容器自己的 healthcheck（`/sla-core -healthcheck`，同样打 `/healthz`），它能让 Docker 按 `restart` 策略重启容器，但**不能把流量切走** —— 单实例本来也无处可切。

### 合并成一个容器（可选）

`collector` 与 `sla-core` 是同一个镜像的两个入口，采集也可以直接跑在 `sla-core` 进程里 —— 给它加 `SLA_COLLECTOR=1` 然后删掉 `collector` 服务即可。多实例同时开也安全：每个渠道由 PG 咨询锁排他。

保持两个容器的唯一理由是日志分离（采集日志量大，混在一起不好看）。两种都行，按习惯选。

> ⚠️ `SLA_COLLECTOR` 与 `-healthcheck` 自 **v1.0.2** 起才有，用旧镜像时这两项都不可用 —— `SLA_COLLECTOR` 会被忽略（采集不会跑），`-healthcheck` 会让容器一直 unhealthy。旧镜像请保留 `collector` 服务，并把 healthcheck 退回 `["CMD", "/sla-core", "-version"]`。

## 3. 配置

根目录 `.env.example` 是发布镜像部署入口；`deploy/.env.example` 用于源码构建验收。

| 变量 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `SLA_IMAGE` | 否 | `ghcr.io/wuhao1477/multi-upstream-ai-gateway-sla:v1.0.4` | 要运行的发布镜像，可用于升级/回滚 |
| `DATABASE_URL` | 是 | 无 | 外部 PostgreSQL 16+ 连接串；库需已存在且账号可建表 |
| `ADMIN_TOKEN` | 是 | 无 | 管理 API 令牌；不会写入 `config_params`。`collector` 不需要它，别顺手也注进去 |
| `ADMIN_PORT` | 否 | `18081` | 本机管理 UI/API 端口，**只绑 `127.0.0.1`** |
| `SLA_COLLECTOR` | 否 | 空（关） | 非空时 `sla-core` 进程内跑周期采集；开了它就可以删掉 `collector` 服务 |

上游 Key 和采集凭证是运行时数据，从管理 UI/API 登记，不放在 `.env`。

## 4. P1 首次使用

1. 打开 `/admin/ui/` 并输入 `ADMIN_TOKEN`。
2. 创建渠道，填写上游 Base URL 和站型。
3. 创建账号并登记采集凭证。
4. 登记上游 Key；列表和日志只显示脱敏前缀。
5. 点击“立即采集”，检查账号、Key、分组、倍率、用量和模型目录。
6. 在渠道详情查看资产总览和异常项。

支持的站型以管理界面的站型注册表和实际探测结果为准。上游没有提供某项能力时，采集结果会标记为 `degraded`，不会静默伪装成完整成功。

## 5. 日常运维

```bash
docker compose ps
docker compose logs --tail=200 sla-core collector
```

`docker compose ps` 的 `healthy` 来自 `/sla-core -healthcheck`，它真打 `/healthz` —— 断库或进程卡死时会转 `unhealthy`，不像 `-version` 那样恒绿。

停止和重新启动：

```bash
docker compose stop
docker compose start
```

删除容器：

```bash
docker compose down
```

数据库在栈外，`down` 与 `down -v` 都不会动它；本栈没有声明任何卷，`-v` 也无东西可删。

## 6. 升级与回滚

修改 `.env` 中的 `SLA_IMAGE`，然后拉取并重建容器：

```bash
SLA_IMAGE=ghcr.io/wuhao1477/multi-upstream-ai-gateway-sla:v1.0.4
docker compose pull
docker compose up -d
curl http://127.0.0.1:18081/healthz
```

回滚就是把 `SLA_IMAGE` 改回上一个已验证 Tag，再执行同样的三条命令。应用启动时会自动执行尚未应用的迁移；已应用迁移文件不可修改，只能新增迁移。

## 7. 备份与恢复

库在栈外，备份直接打 `DATABASE_URL`，不经过 compose：

```bash
pg_dump "$DATABASE_URL" > sla-$(date +%Y%m%d-%H%M%S).sql
```

恢复前先停止写入服务：

```bash
docker compose stop sla-core collector
psql "$DATABASE_URL" < sla-backup.sql
docker compose start sla-core collector
curl http://127.0.0.1:18081/healthz
```

备份文件包含一期明文采集凭证，必须按生产密钥处理并限制访问权限。

## 8. 安全边界

- 管理 UI/API 只发布到 `127.0.0.1:${ADMIN_PORT}`。**端口映射漏掉 `127.0.0.1:` 就等于发布到 `0.0.0.0`**，局域网内任何人都能打开 `/admin/ui/` 并绕过前面所有反代；`verify/test-compose-release.sh` 会守住这条。
- 前面若有反向代理（NGINX 等），**只放行 `/healthz`**；`/admin/*` 与 `/metrics` 一律不代理。
- 外部数据库不要开到公网；跨主机连接用 `sslmode=require` 及以上。
- 不要把 `ADMIN_TOKEN`、`DATABASE_URL`、上游 Key 或采集凭证提交到 Git。
- P1 明文保存采集凭证是已确认的内网取舍；对外提供服务前必须重新评估加密、轮换和权限模型。

## 9. 源码构建与验收

发布镜像不适合修改源码后的快速迭代。源码构建使用现有验收 Compose：

```bash
make check
docker compose -f deploy/docker-compose.yml up -d --build
./verify/test-compose.sh
docker compose -f deploy/docker-compose.yml down -v
```

前端和真实上游验收入口见 [verify/README.md](../verify/README.md)。

## 10. 发布信息

- Release：[v1.0.4](https://github.com/wuhao1477/multi-upstream-ai-gateway-sla/releases/tag/v1.0.4)
- 镜像：`ghcr.io/wuhao1477/multi-upstream-ai-gateway-sla:v1.0.4`
- 发布工作流：[`.github/workflows/release.yml`](../.github/workflows/release.yml)
- P1 验收记录：[P1 release readiness](acceptance/P1-release-readiness.md)

## 11. 当前路线图

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| P1 | 上游渠道采集与管理 | 已发布 |
| P2 | 网关转发、账本、入站鉴权与配额 | 未开始 |
| P3 | 调度、SLA、告警和压测 | 未开始 |
| P4 | 订阅制、多 SLA 等级和外部告警完整渠道 | 未开始 |
