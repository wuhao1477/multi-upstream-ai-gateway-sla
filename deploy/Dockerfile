# 多阶段构建：编译产出静态二进制，运行阶段用 distroless（10 §3 "最小镜像"）。
#
# 为何 distroless 而非 alpine：镜像里没有 shell 与包管理器，
# 攻击面小；且我们不需要在容器里跑脚本 —— 迁移由二进制自带（embed.go）。

# --platform=$BUILDPLATFORM：编译阶段固定跑在**构建机自己的架构**上，
# 交叉编译目标架构，而不是让 QEMU 模拟目标架构去跑编译器。
#
# 为何这样值得：Go 交叉编译是原生能力（CGO_ENABLED=0 时无需交叉工具链），
# 而 QEMU 模拟下编译 Go 会慢 5～10 倍。amd64 runner 上出 arm64 镜像，
# 编译全速、产物仍是真 arm64。
#
# ⚠️ 没有 BUILDPLATFORM/TARGETARCH 时（未启用 BuildKit 的老 docker build），
#    这两个 ARG 会是空串 → GOARCH="" 即"取构建机架构"，退化成原来的行为，
#    不会产出架构错误的镜像。
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src

# 先只拷依赖清单，让 go mod download 能命中 Docker 层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
# CGO_ENABLED=0 才能在 distroless/static 上跑
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
        -ldflags "-X main.version=${VERSION} -s -w" \
        -o /out/sla-core ./cmd/sla-core && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
        -ldflags "-X main.version=${VERSION} -s -w" \
        -o /out/collector ./cmd/collector && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
        -ldflags "-X main.version=${VERSION} -s -w" \
        -o /out/migrate ./cmd/migrate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sla-core /sla-core
COPY --from=build /out/collector /collector
COPY --from=build /out/migrate /migrate
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/sla-core"]
