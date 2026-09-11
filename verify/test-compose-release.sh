#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

compose() {
  env \
    DATABASE_URL='postgres://sla_gateway:compose-test-pg@db.example:5432/sla_gateway?sslmode=require' \
    ADMIN_TOKEN='compose-test-admin' \
    ADMIN_PORT='18080' \
    SLA_IMAGE='ghcr.io/wuhao1477/multi-upstream-ai-gateway-sla:v1.0.4' \
    docker compose --env-file /dev/null -f compose.yml "$@"
}

services=$(compose config --services)
for service in sla-core collector; do
  echo "$services" | grep -qx "$service" || {
    echo "❌ 发布 Compose 缺少服务：$service"
    exit 1
  }
done

# 库是外部依赖：发布栈不得再起 postgres，也不得留数据卷 ——
# 留着会让"外部库"部署悄悄退回本地库，而两边都在跑迁移。
if echo "$services" | grep -qx postgres; then
  echo "❌ 发布 Compose 仍然自带 postgres 服务"
  exit 1
fi

config=$(compose config)
grep -q 'ghcr.io/wuhao1477/multi-upstream-ai-gateway-sla:v1.0.4' <<<"$config"
grep -q 'db.example:5432' <<<"$config"
grep -q '/collector' <<<"$config"
grep -q 'host.docker.internal' <<<"$config"

if grep -q 'pgdata' <<<"$config"; then
  echo "❌ 发布 Compose 仍然声明 pgdata 卷"
  exit 1
fi

# ── 管理面必须只绑回环 ──
# 这条是安全边界不是风格：少了 host_ip 就是发布到 0.0.0.0，局域网内任何人
# 都能直接打开 /admin/ui/，绕过前面所有反代。ADMIN_TOKEN 是认证不是网络隔离，
# 两层都要在（06 §1、09 §1 冻结）。
grep -q 'host_ip: 127.0.0.1' <<<"$config" || {
  echo "❌ 管理端口没有绑定 127.0.0.1——会发布到 0.0.0.0，管理面对局域网暴露"
  exit 1
}
grep -q 'published: "18080"' <<<"$config"

# ── healthcheck 必须真探 /healthz ──
# -version 只证明二进制能执行：进程卡死或 PG 断连时它照样退 0，
# 容器会一直报 healthy 而服务早已不可用（实测 503 时 -version 仍退 0）。
grep -q '\-healthcheck' <<<"$config" || {
  echo "❌ healthcheck 没有使用 -healthcheck 子命令"
  exit 1
}
if grep -q 'CMD.*\-version' <<<"$config"; then
  echo "❌ healthcheck 仍在用 -version，它探不出进程卡死/断库"
  exit 1
fi

# ── collector 不该持有 ADMIN_TOKEN ──
# cmd/collector 与 internal/collection 全程零引用，多一个容器持有密钥没有收益。
if compose config --format json \
  | python3 -c 'import json,sys; sys.exit(0 if "ADMIN_TOKEN" in json.load(sys.stdin)["services"]["collector"].get("environment",{}) else 1)'; then
  echo "❌ collector 仍然被注入 ADMIN_TOKEN"
  exit 1
fi

for missing in DATABASE_URL ADMIN_TOKEN; do
  if env -u "$missing" \
    DATABASE_URL='postgres://x:y@db.example:5432/z' ADMIN_TOKEN='t' \
    ADMIN_PORT='18080' SLA_IMAGE='img' \
    env -u "$missing" docker compose --env-file /dev/null -f compose.yml config \
    >/dev/null 2>&1; then
    echo "❌ 缺少必填变量 $missing 时 Compose 仍然成功"
    exit 1
  fi
done

echo "✅ 发布 Compose 配置校验通过"
