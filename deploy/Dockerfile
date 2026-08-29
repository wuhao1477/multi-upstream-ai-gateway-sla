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

# ── 前端阶段：编 web/ 下的 Vue 工程，产出静态资源 ──
#
# 也钉在 BUILDPLATFORM 上：产物是 JS/CSS 文本，与目标架构无关，
# 而 rolldown / lightningcss 是**按平台分发的原生二进制** —— 在 QEMU 里
# 跑它们既慢又容易撞上缺少对应架构 binding 的坑。构建机原生跑、产物照样通用。
FROM --platform=$BUILDPLATFORM node:24-bookworm-slim AS web
WORKDIR /src/web
# 先只拷清单：依赖没变时 npm ci 这一层直接命中缓存
COPY web/package.json web/package-lock.json ./
# npm ci 而非 npm install：严格按 lockfile 装，构建结果可复现。
# lockfileVersion 3 记录了各平台的原生 binding 变体，跨平台 ci 是安全的。
RUN npm ci
COPY web/ ./
# build 脚本里带 vue-tsc：类型错会让 docker build 直接失败，
# 而不是产出一个"能跑但类型是错的"镜像。
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src

# 先只拷依赖清单，让 go mod download 能命中 Docker 层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# 前端产物拷进 Go 包内，交给 //go:embed all:webdist 打进二进制。
# 必须在 go build 之前：embed 是编译期读文件，晚一步就编不进去。
# （.dockerignore 已排除本机的 webdist 内容，这里拿到的一定是上一阶段刚编的。）
COPY --from=web /src/internal/admin/webdist/ ./internal/admin/webdist/
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
