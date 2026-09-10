# multi-upstream-ai-gateway-sla

一个面向个人和内网环境的多上游 AI 渠道管理系统。它把不同站型的渠道、账号、Key、分组、额度和模型目录统一采集到 PostgreSQL，并提供管理 UI、手动采集和周期采集。

当前发布版本：`v1.0.0` · 当前交付阶段：**P1 上游采集与管理**

[部署指南](docs/DEPLOYMENT.md) · [Release v1.0.0](https://github.com/wuhao1477/multi-upstream-ai-gateway-sla/releases/tag/v1.0.0) · [Apache-2.0](LICENSE)

## P1 已交付什么

- 渠道列表、创建、编辑、启用和停用
- 多账号管理与账号级采集凭证
- 上游 Key 登记、编辑、停用、删除和脱敏展示
- 渠道分组、分组倍率与 Key 分组关联
- Key 额度、用量、限流字段和同步历史
- 渠道模型目录、价格信息和疑似下架提示
- NewAPI / Sub2API 站型探测与采集适配器
- 手动采集、周期采集、部分成功和 `degraded` 能力标记
- 资产总览、异常项、管理 API 和 Vue 管理 UI

## 明确不在 v1.0.0

P1 不包含请求转发、账本、候选调度、SLA 接管、容量保留、告警、压测、订阅台账或多租户。`/v1/*` 是后续 P2 的数据面入口；当前版本可通过 Caddy 访问 `/healthz`，管理面走本机回环端口。

## 5 分钟启动

```bash
git clone https://github.com/wuhao1477/multi-upstream-ai-gateway-sla.git
cd multi-upstream-ai-gateway-sla-public
cp .env.example .env
openssl rand -hex 32
```

把生成的随机值分别填入 `.env` 的 `POSTGRES_PASSWORD` 和 `ADMIN_TOKEN`。不要把 `.env`、上游 Key 或采集凭证提交到 Git。

当前 GHCR 镜像需要 `read:packages` 权限：

```bash
echo "$GHCR_TOKEN" | docker login ghcr.io -u "$GITHUB_USER" --password-stdin
```

启动并检查：

```bash
docker compose pull
docker compose up -d
docker compose ps
curl -k https://localhost/healthz
```

管理界面地址：<http://127.0.0.1:8080/admin/ui/>。在界面中输入 `.env` 的 `ADMIN_TOKEN`，然后创建渠道、账号、采集凭证和上游 Key。

## 架构

```text
管理 UI/API ──> sla-core-a :8080 ─┐
                                  ├── PostgreSQL 16
管理 UI/API ──> sla-core-b :8080 ┘

客户端 ────────> Caddy :80/:443 ──> /healthz（P1）
                              └──> /v1/*（P2）

collector ───────────────────────> PostgreSQL + 上游管理接口
```

- `sla-core-a/b` 共享 PostgreSQL；启动时自动执行迁移和配置种子。
- `collector` 使用同一镜像中的 `/collector`，负责周期采集，不发布宿主机端口。
- Caddy 只代理 `/healthz` 和未来的 `/v1/*`；`/admin/*`、`/metrics` 不经 Caddy。
- 管理端口只绑定 `127.0.0.1`，默认是 `8080`。

## 配置

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `SLA_IMAGE` | `ghcr.io/wuhao1477/multi-upstream-ai-gateway-sla-public:v1.0.0` | 发布镜像版本 |
| `POSTGRES_PASSWORD` | 无 | 必填；建议 `openssl rand -hex 32` |
| `ADMIN_TOKEN` | 无 | 必填；管理 API/UI 令牌 |
| `ADMIN_PORT` | `8080` | 本机管理端口 |
| `SITE_ADDRESS` | `localhost` | Caddy 站点名；默认使用自签证书 |
| `HTTP_PORT` | `80` | Caddy HTTP 端口 |
| `HTTPS_PORT` | `443` | Caddy HTTPS 端口 |

上游 Key 和采集凭证是运行时数据，使用管理界面登记，不写进环境变量。

## 日常运维

```bash
docker compose ps
docker compose logs --tail=200 sla-core-a sla-core-b collector
docker compose logs -f caddy
docker compose stop
docker compose start
docker compose exec -T postgres pg_dump -U sla -d sla > sla-backup.sql
```

`docker compose down -v` 会删除 PostgreSQL 数据卷，只能在明确放弃数据时使用。升级或回滚时修改 `.env` 的 `SLA_IMAGE`，然后执行 `docker compose pull && docker compose up -d`。

完整的备份恢复、安全边界和排障说明见 [部署指南](docs/DEPLOYMENT.md)。

## 本地开发

发布镜像部署使用根目录 `compose.yml`；修改源码后的本地构建使用 `deploy/docker-compose.yml`：

```bash
make check
docker compose -f deploy/docker-compose.yml up -d --build
./verify/test-compose.sh
docker compose -f deploy/docker-compose.yml down -v
```

常用入口：

- `make build`：构建前端和三个 Go 二进制
- `make test`：Go 竞态测试
- `make test-ui`：真实 Chrome 管理界面验收
- `./verify/test-migrate.sh`：临时 PostgreSQL 迁移验收
- `./verify/gate.sh`：文档一致性和 DDL 门禁
- `./verify/test-compose-release.sh`：发布 Compose 配置校验
- `./verify/test-doc-links.sh`：README/部署文档链接校验

## 发布

Release 工作流由 Tag 触发，构建 Linux amd64/arm64 二进制并推送多架构 GHCR 镜像：

```text
ghcr.io/wuhao1477/multi-upstream-ai-gateway-sla-public:v1.0.0
```

当前 Release 包含两个平台的二进制包和 `SHA256SUMS`，详见 [Release v1.0.0](https://github.com/wuhao1477/multi-upstream-ai-gateway-sla/releases/tag/v1.0.0) 与 [发布工作流](.github/workflows/release.yml)。

## 文档地图

- [部署指南](docs/DEPLOYMENT.md)：启动、配置、升级、回滚、备份和排障
- [P1 验收记录](docs/acceptance/P1-release-readiness.md)：真实验收和发布判定
- [PRD](docs/PRD.md)：产品需求与 P1/P2/P3/P4 切分
- [开发总览](docs/dev/00-overview-and-milestones.md)：里程碑和工程约束
- [架构设计](docs/dev/01-architecture.md)：组件与数据流
- [数据模型](docs/dev/02-data-model.md)：PostgreSQL 表和迁移规则
- [采集器契约](docs/dev/04-collector-adapter.md)：站型适配器和能力分级
- [管理 API](docs/dev/09-admin-api.md)：管理面接口
- [验收入口](verify/README.md)：本地门禁、Compose、迁移和 UI 验收

## 路线图

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| P1 | 上游渠道采集与管理 | 已发布 |
| P2 | 网关转发、账本、入站鉴权与配额 | 未开始 |
| P3 | 调度、SLA、告警和压测 | 未开始 |
| P4 | 订阅制、多 SLA 等级和外部告警完整渠道 | 未开始 |

## 许可证

本项目采用 [Apache License 2.0](LICENSE)。
