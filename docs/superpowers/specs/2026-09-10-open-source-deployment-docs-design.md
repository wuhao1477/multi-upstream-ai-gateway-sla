# 开源部署与项目文档设计规格

## 目标

让首次接触仓库的开源用户可以用已发布镜像启动 P1，理解管理面、数据存储、采集器和 Caddy 的边界，并能完成升级、回滚、备份与基本故障排查。

## 范围

- 新增根目录 `compose.yml`，作为发布镜像部署入口。
- 新增根目录 `.env.example`，描述部署必填项与可选项。
- 保留 `deploy/docker-compose.yml` 作为本地源码构建和验收入口，并在文档中明确区别。
- 新增 `docs/DEPLOYMENT.md`，覆盖启动、初始化、管理 UI、升级、回滚、备份恢复、健康检查、日志和安全边界。
- 重写 `README.md`，面向开源用户说明项目定位、P1 范围、快速启动、架构、配置、开发、发布和路线图。
- 新增 Apache-2.0 `LICENSE`。
- 增加 Compose 配置校验与文档链接校验，不改变 P1 业务代码。

## 部署模型

根目录 `compose.yml` 使用 `SLA_IMAGE` 指定的镜像，默认值为 `ghcr.io/wuhao1477/multi-upstream-ai-gateway-sla:v1.0.0`，启动以下服务：

- `postgres`：PostgreSQL 16，仅加入 Compose 内部网络。
- `sla-core-a`、`sla-core-b`：同一版本的 `sla-core`，共享 PostgreSQL；仅 A 将管理端口绑定到宿主机回环地址。
- `collector`：同一版本的 `collector` 子命令，不发布宿主机端口。
- `caddy`：对外提供 80/443，只转发 `/healthz` 和未来的 `/v1/*`；不转发 `/admin/*` 与 `/metrics`。

数据库迁移和种子由 `sla-core` 启动时的 bootstrap 完成；不增加额外 migration 容器。P1 没有请求转发，因此 `/v1/*` 保持未实现状态，管理面通过本机端口访问。

## 配置约定

- `POSTGRES_PASSWORD` 与 `ADMIN_TOKEN` 必填且不得使用空值；Compose 不提供生产默认密码。
- `SLA_IMAGE` 可切换到其他已发布版本，用于升级和回滚。
- `ADMIN_PORT` 默认绑定 `127.0.0.1:8080`。
- `SITE_ADDRESS` 控制 Caddy 的站点名；默认使用 `localhost` 自签证书。
- 上游 Key 与采集凭证属于运行时数据，经管理界面登记，不写入 `.env`。

## 文档结构

README 只保留开源用户需要的入口信息；深入的部署操作放在 `docs/DEPLOYMENT.md`，需求和设计历史继续放在 `docs/PRD.md`、`docs/dev/` 与 `docs/issues/`。

## 非目标

- 不新增网关请求转发、账本、P3 调度/SLA/告警/压测或 P4 订阅功能。
- 不新增 Prometheus/Grafana 等面板服务。
- 不复制第二套业务 Compose 拓扑。
- 不改变数据库密码轮换策略；当前部署假设为内网使用。

## 验收标准

1. `docker compose --env-file .env -f compose.yml config` 成功解析，缺少必填密钥时明确失败。
2. 发布镜像部署路径可启动 PostgreSQL、双 core、collector 和 Caddy，`/healthz` 返回 200。
3. 管理 UI/API 仅从 `ADMIN_PORT` 回环入口访问，Caddy 不暴露 `/admin/*` 与 `/metrics`。
4. README 的复制、启动、查看日志、停止、升级和回滚命令可直接执行。
5. Release、GHCR 镜像、源码构建路径和 P1/P2/P3/P4 边界在文档中保持一致。
6. `LICENSE` 明确为 Apache License 2.0，未引入与现有依赖冲突的许可声明。
