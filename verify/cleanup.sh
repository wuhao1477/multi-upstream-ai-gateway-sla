#!/usr/bin/env bash
# 跑完清理：停容器、删卷、删网络、删镜像、删本地状态文件。
# 默认不删除 python:3.12-alpine（常用基础镜像）；加 --all 连它一起删。
set -uo pipefail
cd "$(dirname "$0")"

echo ">> 停止并移除容器 + 卷 + 网络..."
docker compose -f docker-compose.verify.yml down -v --remove-orphans

echo ">> 删除 AxonHub 验证镜像..."
docker rmi looplj/axonhub:v1.0.0-beta5 2>/dev/null || true

if [ "${1:-}" = "--all" ]; then
  echo ">> 删除 python 基础镜像..."
  docker rmi python:3.12-alpine 2>/dev/null || true
fi

echo ">> 删除本地状态文件..."
rm -f state.json

echo ">> 校验残留（应为空）..."
docker ps -a --filter "name=ah-verify" --format '{{.Names}}' || true
docker volume ls --filter "name=ah-verify" --format '{{.Name}}' || true
docker network ls --filter "name=ah-verify" --format '{{.Name}}' || true

echo ">> 清理完成。verify/ 下的脚本文件保留（属于仓库文档），如需一并删除请手动 rm -rf verify/。"
