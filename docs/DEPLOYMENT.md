# 部署指南

本文面向首次部署的开源用户。当前发布版本是 P1：上游渠道、账号、Key、分组、模型目录和采集管理。请求转发、账本、P3 调度/SLA/告警/压测和 P4 订阅尚未进入本版本。

## 1. 推荐部署：发布镜像 + Compose

### 前置条件

- Docker Engine 24+ 与 Docker Compose v2。
- 一台能访问 GHCR 的 Linux/macOS 主机。
- 80、443 和管理端口（默认 8080）没有被其他程序占用。

仓库当前为私有仓库，GHCR 镜像默认需要登录：

```bash
echo "$GHCR_TOKEN" | docker login ghcr.io -u "$GITHUB_USER" --password-stdin
```

Token 需要 `read:packages` 权限。不要把 Token 写入仓库或 `.env`。

### 首次启动

```bash
git clone https://github.com/wuhao1477/multi-upstream-ai-gateway-sla-public.git
cd multi-upstream-ai-gateway-sla-public
cp .env.example .env
```

编辑 `.env`，至少填写两个值：

```dotenv
POSTGRES_PASSWORD=<URL-safe 随机密码>
ADMIN_TOKEN=<管理面随机令牌>
```

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
curl -k https://localhost/healthz
```

预期返回 HTTP 200，并包含 `status=ok`、`db=ok` 和 `config_snapshot=loaded`。

管理界面只绑定到本机回环地址：

```text
http://127.0.0.1:8080/admin/ui/
```

在界面中填写 `.env` 里的 `ADMIN_TOKEN`。Caddy 不代理 `/admin/*` 与 `/metrics`；这两个路径不会从 80/443 对外开放。

## 2. 服务拓扑

```text
浏览器/运维 ──> 127.0.0.1:8080 ──> sla-core-a ─┐
                                                ├── PostgreSQL 16
                                          sla-core-b ┘

客户端 ──> Caddy :80/:443 ──> /healthz（P1 可用）
                         └──> /v1/*（P2 开始实现）

collector ───────────────────────────────> PostgreSQL + 上游管理接口
```

| 服务 | 作用 | 宿主机端口 |
| --- | --- | --- |
| `postgres` | 数据库、迁移、配置和采集数据 | 不发布 |
| `sla-core-a/b` | 管理 API、管理 UI、`/healthz` | A 的 8080 仅回环 |
| `collector` | 周期采集上游账号、Key、分组和模型目录 | 不发布 |
| `caddy` | TLS、健康探测和未来的数据面入口 | 80、443 |

数据库迁移和 P1 配置种子由 `sla-core` 启动时自动执行。双实例通过 PostgreSQL 咨询锁保证只有一个实例真正执行迁移。

## 3. 配置

根目录 `.env.example` 是发布镜像部署入口；`deploy/.env.example` 用于源码构建验收。

| 变量 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `SLA_IMAGE` | 否 | `ghcr.io/wuhao1477/multi-upstream-ai-gateway-sla-public:v1.0.0` | 要运行的发布镜像，可用于升级/回滚 |
| `POSTGRES_PASSWORD` | 是 | 无 | PostgreSQL 密码；不要使用默认值 |
| `ADMIN_TOKEN` | 是 | 无 | 管理 API 令牌；不会写入 `config_params` |
| `ADMIN_PORT` | 否 | `8080` | 本机管理 UI/API 端口 |
| `SITE_ADDRESS` | 否 | `localhost` | Caddy 站点名；默认使用自签证书 |
| `HTTP_PORT` | 否 | `80` | Caddy HTTP 端口 |
| `HTTPS_PORT` | 否 | `443` | Caddy HTTPS 端口 |

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
docker compose logs --tail=200 sla-core-a sla-core-b collector
docker compose logs -f caddy
```

停止和重新启动：

```bash
docker compose stop
docker compose start
```

删除容器但保留数据库卷：

```bash
docker compose down
```

`docker compose down -v` 会删除 PostgreSQL 和 Caddy 数据卷，只能在确认不需要数据时执行。

## 6. 升级与回滚

修改 `.env` 中的 `SLA_IMAGE`，然后拉取并重建容器：

```bash
SLA_IMAGE=ghcr.io/wuhao1477/multi-upstream-ai-gateway-sla-public:v1.0.0
docker compose pull
docker compose up -d
curl -k https://localhost/healthz
```

回滚就是把 `SLA_IMAGE` 改回上一个已验证 Tag，再执行同样的三条命令。应用启动时会自动执行尚未应用的迁移；已应用迁移文件不可修改，只能新增迁移。

## 7. 备份与恢复

导出 PostgreSQL：

```bash
docker compose exec -T postgres pg_dump -U sla -d sla > sla-$(date +%Y%m%d-%H%M%S).sql
```

恢复前先停止写入服务：

```bash
docker compose stop sla-core-a sla-core-b collector
cat sla-backup.sql | docker compose exec -T postgres psql -U sla -d sla
docker compose start sla-core-a sla-core-b collector
curl -k https://localhost/healthz
```

备份文件包含一期明文采集凭证，必须按生产密钥处理并限制访问权限。

## 8. 安全边界

- 管理 UI/API 只发布到 `127.0.0.1:${ADMIN_PORT}`。
- Caddy 只代理 `/healthz` 和未来的 `/v1/*`。
- 不要把 PostgreSQL 端口映射到宿主机公网地址。
- 不要把 `ADMIN_TOKEN`、`POSTGRES_PASSWORD`、上游 Key 或采集凭证提交到 Git。
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

- Release：[v1.0.0](https://github.com/wuhao1477/multi-upstream-ai-gateway-sla-public/releases/tag/v1.0.0)
- 镜像：`ghcr.io/wuhao1477/multi-upstream-ai-gateway-sla-public:v1.0.0`
- 发布工作流：[`.github/workflows/release.yml`](../.github/workflows/release.yml)
- P1 验收记录：[P1 release readiness](acceptance/P1-release-readiness.md)

## 11. 当前路线图

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| P1 | 上游渠道采集与管理 | 已发布 |
| P2 | 网关转发、账本、入站鉴权与配额 | 未开始 |
| P3 | 调度、SLA、告警和压测 | 未开始 |
| P4 | 订阅制、多 SLA 等级和外部告警完整渠道 | 未开始 |
