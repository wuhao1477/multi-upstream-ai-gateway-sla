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

.PHONY: web
web: ## 编前端（web/ → internal/admin/webdist，供 go:embed）
	@[ -d web/node_modules ] || (cd web && pnpm install --frozen-lockfile)
	cd web && pnpm run build

.PHONY: build
# 依赖 web：管理界面由 go:embed 打进二进制，跳过前端只会编出一个
# /admin/ui 返回 500「前端产物缺失」的二进制 —— 而 go build 本身照旧成功
# （webdist/ 里有占位文件，模式匹配得到），失败点被推迟到运行时。
build: web ## 编译 sla-core、collector、migrate（含前端）
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINDIR)/sla-core ./cmd/sla-core
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINDIR)/collector ./cmd/collector
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINDIR)/migrate ./cmd/migrate

.PHONY: test
test: ## 跑全部单测（带竞态检测）
# -race：批量导入是 importConcurrency 个 worker 扇出写同一个 detectStash
# （internal/admin/import_api.go），主进程还有关停协程。竞态是那种"压测不出、
# 生产偶发"的错，而全部单测 2 秒、开了 -race 约 4 秒，没有不开的理由。
	$(GO) test -race ./...

.PHONY: vet
vet: ## go vet + rows.Err() 漏检查
	$(GO) vet ./...
# 每个 rows.Next() 循环后面都必须查一次 rows.Err()：遍历中途出错时
# rows.Next() 只返回 false，与"正常读完"无从区分，漏查就会静默返回截断的结果集。
# go vet 不管这个。13 处里曾经漏过 1 处（listCredentials），加个 grep 别让它回来。
# 先剔掉注释行再数 —— 否则解释这条规则的注释本身就会被算成一次调用。
	@bad=$$(for f in $$(grep -rl 'rows.Next()' --include='*.go' ./cmd ./internal); do \
	  code=$$(grep -v '^[[:space:]]*//' $$f); \
	  n=$$(echo "$$code" | grep -c 'rows.Next()'); \
	  e=$$(echo "$$code" | grep -c 'rows.Err()'); \
	  [ "$$n" != "$$e" ] && echo "  $$f (Next=$$n Err=$$e)"; \
	done); \
	if [ -n "$$bad" ]; then echo "以下文件的 rows.Next() 循环缺 rows.Err()："; echo "$$bad"; exit 1; fi
	@echo "✅ rows.Err() 无遗漏"

.PHONY: lint
lint: ## golangci-lint（未安装则退回 go vet 并提示）
	@if command -v golangci-lint >/dev/null 2>&1; then \
	  golangci-lint run; \
	else \
	  echo "golangci-lint 未安装，退回 go vet。装它（v2 起才支持 go1.25，注意路径含 /v2）："; \
	  echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2"; \
	  echo "（CI 用 golangci-lint-action 跑同一版本，本地跳过不代表 CI 会放过）"; \
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

.PHONY: test-api
test-api: ## /admin/config 端到端测试（需 Docker）
	./verify/test-config-api.sh

.PHONY: test-compose
test-compose: ## compose 全栈冒烟：健康/选主/AC-27/边界（需 Docker）
	./verify/test-compose.sh

.PHONY: docs-gate
docs-gate: ## 文档一致性 12 类 + DDL 真跑（需 Docker）
	./verify/gate.sh

.PHONY: check
check: fmt-check vet test gen-check mig-check ## 提交前自检（不含需 Docker 的项）

.PHONY: clean
clean: ## 清理产物
	rm -rf $(BINDIR)

.PHONY: test-ui
test-ui: ## 真 Chrome 验收管理界面（需 Chrome + node + PG）
	./verify/ui-stack.sh

.PHONY: dev-ui
dev-ui: ## 起常驻本地栈供人工点验管理界面（Ctrl-C 拆除）
	./verify/ui-stack.sh --keep

.PHONY: test-remote
test-remote: ## 内网真库只读界面验收 32 项（需 DATABASE_URL + Chrome）
	./verify/remote-stack.sh
