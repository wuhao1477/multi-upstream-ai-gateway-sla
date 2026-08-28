# 构建与门禁入口（10 §3）。CI 调同样的目标，本地与 CI 不分叉。
.DEFAULT_GOAL := help

GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
BINDIR  := bin

.PHONY: help
help: ## 列出可用目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## 编译 sla-core、collector、migrate
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINDIR)/sla-core ./cmd/sla-core
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINDIR)/collector ./cmd/collector
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINDIR)/migrate ./cmd/migrate

.PHONY: test
test: ## 跑全部单测
	$(GO) test ./...

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## golangci-lint（未安装则退回 go vet 并提示）
	@if command -v golangci-lint >/dev/null 2>&1; then \
	  golangci-lint run; \
	else \
	  echo "golangci-lint 未安装，退回 go vet；CI 会用真 linter"; \
	  $(GO) vet ./...; \
	fi

.PHONY: fmt
fmt: ## gofmt 全库
	gofmt -l -w ./cmd ./internal

.PHONY: fmt-check
fmt-check: ## 校验格式（CI 用，不改文件）
	@out=$$(gofmt -l ./cmd ./internal); \
	if [ -n "$$out" ]; then echo "以下文件未格式化："; echo "$$out"; exit 1; fi
	@echo "✅ 格式干净"

.PHONY: gen-params
gen-params: ## 从 09 §4bis 重新生成 config 键清单
	python3 internal/config/gen_params.py

.PHONY: gen-check
gen-check: gen-params ## 校验生成物与文档同步（CI 用）
	@if ! git diff --quiet -- internal/config/params_gen.go; then \
	  echo "❌ params_gen.go 与 docs/dev/09-admin-api.md §4bis 不一致，请提交重新生成的文件："; \
	  git --no-pager diff -- internal/config/params_gen.go; exit 1; \
	fi
	@echo "✅ 配置键清单与文档同步"

.PHONY: migrate
migrate: ## 应用迁移与配置种子（需 DATABASE_URL 或 -dsn）
	$(GO) run ./cmd/migrate

.PHONY: mig-check
mig-check: ## 校验 migrations/ 与 02 的对象集合一致
	python3 verify/check_migrations.py

.PHONY: test-integration
test-integration: ## 起临时 PG 跑迁移与种子的集成测试（需 Docker）
	./verify/test-migrate.sh

.PHONY: docs-gate
docs-gate: ## 文档一致性 12 类 + DDL 真跑（需 Docker）
	./verify/gate.sh

.PHONY: check
check: fmt-check vet test gen-check mig-check ## 提交前自检（不含需 Docker 的项）

.PHONY: clean
clean: ## 清理产物
	rm -rf $(BINDIR)
