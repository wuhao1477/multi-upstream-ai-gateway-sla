#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

compose() {
  env \
    POSTGRES_PASSWORD='compose-test-pg' \
    ADMIN_TOKEN='compose-test-admin' \
    ADMIN_PORT='18080' \
    SITE_ADDRESS='localhost' \
    SLA_IMAGE='ghcr.io/wuhao1477/multi-upstream-ai-gateway-sla:v1.0.0' \
    docker compose --env-file /dev/null -f compose.yml "$@"
}

services=$(compose config --services)
for service in postgres sla-core-a sla-core-b collector caddy; do
  echo "$services" | grep -qx "$service" || {
    echo "❌ 发布 Compose 缺少服务：$service"
    exit 1
  }
done

config=$(compose config)
grep -q 'ghcr.io/wuhao1477/multi-upstream-ai-gateway-sla:v1.0.0' <<<"$config"
grep -q 'host_ip: 127.0.0.1' <<<"$config"
grep -q 'published: "18080"' <<<"$config"
grep -q 'entrypoint:' <<<"$config"
grep -q '/collector' <<<"$config"

if env -u POSTGRES_PASSWORD -u ADMIN_TOKEN docker compose \
  --env-file /dev/null -f compose.yml config >/dev/null 2>&1; then
  echo "❌ 缺少必填密钥时 Compose 仍然成功"
  exit 1
fi

echo "✅ 发布 Compose 配置校验通过"
