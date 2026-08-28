# 多阶段构建：编译产出静态二进制，运行阶段用 distroless（10 §3 "最小镜像"）。
#
# 为何 distroless 而非 alpine：镜像里没有 shell 与包管理器，
# 攻击面小；且我们不需要在容器里跑脚本 —— 迁移由二进制自带（embed.go）。

FROM golang:1.25 AS build
WORKDIR /src

# 先只拷依赖清单，让 go mod download 能命中 Docker 层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
# CGO_ENABLED=0 才能在 distroless/static 上跑
RUN CGO_ENABLED=0 GOOS=linux go build \
        -ldflags "-X main.version=${VERSION} -s -w" \
        -o /out/sla-core ./cmd/sla-core && \
    CGO_ENABLED=0 GOOS=linux go build \
        -ldflags "-X main.version=${VERSION} -s -w" \
        -o /out/collector ./cmd/collector && \
    CGO_ENABLED=0 GOOS=linux go build \
        -ldflags "-X main.version=${VERSION} -s -w" \
        -o /out/migrate ./cmd/migrate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sla-core /sla-core
COPY --from=build /out/collector /collector
COPY --from=build /out/migrate /migrate
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/sla-core"]
