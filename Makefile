.PHONY: build build-debug build-linux build-linux-arm64 test test-cover test-cover-html lint lint-fix web-build web-check itest itest-mockllm itest-run itest-tui itest-acp itest-headless desktop-smoke headless headless-linux headless-linux-arm64

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# Release build: -s -w strips symbol table + DWARF (~30% smaller binary),
# -trimpath keeps build reproducible. For Delve debugging use `make build-debug`.
LDFLAGS := -s -w -X main.Version=$(VERSION) -X github.com/monsterxx03/tachi/llm.Version=$(VERSION)
NPM := npm --prefix web/frontend

# CLI_OUT is where `make build` writes the CLI/TUI binary. It is a variable because
# the DESKTOP package ships that same binary (see desktop/Makefile's `cli`) and
# builds it from here: `make -C .. build CLI_OUT=…` — so the version stamp,
# -trimpath and the embedded web console stay defined in ONE place.
CLI_OUT ?= tachi

# third_party_channels.go (local-only, gitignored) blank-imports channel plugins that
# live OUTSIDE this module and are resolved through the repo's go.work. With the
# workspace off that import cannot resolve at all, so the file carries a
# `//go:build !no_workspace` constraint and we tag it out here. GOWORK=off arrives from
# desktop/Makefile (its module is deliberately not in the workspace) through the
# environment of `make -C ..`: the desktop package builds the CLI it ships that way,
# and that build must not depend on a local plugin checkout existing.
TAGS := $(if $(filter off,$(GOWORK)),-tags no_workspace,)

build: web-check
	go build -trimpath $(TAGS) -ldflags="$(LDFLAGS)" -o $(CLI_OUT) .

# Build the frontend into web/frontend/dist so it's embedded into the Go binary.
# `npm ci`, not `install`: it installs strictly from package-lock.json, so it can
# never rewrite a tracked file. `install` does — under node 20's npm it drops the
# `libc` fields a newer npm wrote into the lock, dirtying the tree. CI's
# .github/actions/web-build does the same.
web-build:
	$(NPM) ci && $(NPM) run build

# The module embeds web/frontend/dist (web/frontend/embed.go), so any Go
# compile needs it to exist first. Check instead of unconditionally building:
# dist is only missing on a fresh checkout, and npm build is expensive.
web-check:
	@if [ ! -f web/frontend/dist/index.html ]; then \
		echo "web/frontend/dist is missing — building the frontend first"; \
		$(MAKE) web-build; \
	fi

build-debug: web-check
	go build $(TAGS) -ldflags="-X main.Version=$(VERSION) -X github.com/monsterxx03/tachi/llm.Version=$(VERSION)" -o tachi .

build-linux: web-check
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath $(TAGS) -ldflags="$(LDFLAGS)" -o tachi-linux-amd64 .

build-linux-arm64: web-check
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath $(TAGS) -ldflags="$(LDFLAGS)" -o tachi-linux-arm64 .

# headless 构建**无窗口宿主**：会话运行时 + 手机服务端 +（配置开着的）IM channel，没有窗口、托盘、
# 通知、BLE、麦克风。给一台没有图形环境的机器用（NAS、服务器、容器）——手机连它、机器人也住在它里面，
# 不需要桌面开着，也不需要第二个进程。
#
# 构建本身住在 desktop/Makefile（那边是独立 module，构建标签与 CGO_ENABLED=0 只定义一处）；
# 这里只转发，并把产物搬到仓库根，和 tachi / tachi-linux-amd64 一样是「随手可拿」的位置：
# 这东西的用途就是被 scp 到另一台机器上。
#
# 完整的一条：`make headless-linux`（amd64）或 `make headless-linux-arm64`，产物直接落在仓库根，
# 名字带平台。与 `build-linux` / `build-linux-arm64` 同一套命名：前缀说的是「编什么」，
# 后缀说的是「给哪个平台」。
#
# GOOS/GOARCH 必须显式转发给子 make —— 命令前的环境变量赋值只进得去父 make，不会随 MAKEFLAGS
# 一起下去，子 make 于是按**宿主**平台构建，产物名字却是交叉平台的名字。所以 headless 目标把
# 它们当作 make 变量读（$(or $(GOOS),…)：命令行变量优先，其次环境），再逐一转给子 make。
#
# HEADLESS_OUT 这个名字**不能**和 desktop/Makefile 里的变量同名：命令行变量会经 MAKEFLAGS
# 传给子 make，于是子 make 的 `-o` 也指到根目录，cp 复制的还是上一次的旧文件 —— 名字对、
# 平台错，正是最难发现的那种。那边叫 HEADLESS_BIN。
HEADLESS_GOOS   ?= $(or $(GOOS),$(shell go env GOOS))
HEADLESS_GOARCH ?= $(or $(GOARCH),$(shell go env GOARCH))
HEADLESS_OUT    ?= tachi-headless-$(HEADLESS_GOOS)-$(HEADLESS_GOARCH)

# headless-linux 是上面那条的常用形态：Linux 上没有桌面版可跑（那个要 gtk，属于 desktop/
# 自己的构建），所以「给 Linux 编一个」就是「编无头宿主」。
headless-linux:
	$(MAKE) --no-print-directory headless GOOS=linux GOARCH=amd64

headless-linux-arm64:
	$(MAKE) --no-print-directory headless GOOS=linux GOARCH=arm64

headless:
	$(MAKE) -C desktop --no-print-directory headless \
		$(if $(GOOS),GOOS=$(GOOS)) $(if $(GOARCH),GOARCH=$(GOARCH))
	cp desktop/bin/tachi-headless $(HEADLESS_OUT)
	@echo "→ $(HEADLESS_OUT)"

test: web-check
	go test ./...

test-cover: web-check
	@echo "=== Running tests with coverage ==="
	@go test -coverprofile=coverage.out ./...
	@echo ""
	@echo "=== Per-package coverage ==="
	@go tool cover -func=coverage.out | sort -k 3 -r
	@echo ""
	@echo "=== Total coverage: $$(go tool cover -func=coverage.out | tail -1 | awk '{print $$NF}') ==="
	@rm -f coverage.out

test-cover-html: web-check
	go test -coverprofile=coverage.out ./... && \
	go tool cover -html=coverage.out && \
	rm -f coverage.out

# The web console's eslint. It has its own definition because CI calls it too (the
# `test` job, right after web-build has installed the deps) — one command, two callers.
# Skipped with a loud line when the deps are not installed: `make lint` used to work on a
# tree with no node_modules, and that must stay true. CI always has them, so CI never skips.
web-lint:
	@if [ -d web/frontend/node_modules ]; then \
		echo "=== Running eslint (web/frontend) ==="; \
		cd web/frontend && npm run lint; \
	else \
		echo "=== web/frontend 没有 node_modules：跳过 eslint（先 make web-build 装一次） ==="; \
	fi

lint: web-check web-lint
	@echo "=== Running golangci-lint ==="
	golangci-lint run ./...

lint-fix: web-check
	@echo "=== Running go fix ==="
	go fix ./...
	@echo "=== Running golangci-lint (with --fix) ==="
	golangci-lint run --fix ./...

# Integration tests (docs/2026-07-31-tui-integration-test.md): isolated from
# unit tests via the integration build tag; `go test ./...` stays unchanged.
# M0 = -p pipe mode (real binary). mockllm's own unit + contract tests run
# with the regular `test` target (no build tag).
#
# Parallelism: every spec owns an isolated --home (t.TempDir()) + its own
# mockllm server (random port) + (run/acp) a real tachi subprocess or (tui)
# an in-process tea.Program, so specs are safe to run concurrently.
#   - `itest` runs all four suites with `make -j4` — package parallelism
#     (mockllm contract tests) AND ginkgo -p spec parallelism inside
#     run/acp/tui; total wall time ≈ mockllm + the slowest suite.
#   - `itest-run` / `itest-acp` / `itest-tui` additionally parallelize the
#     suite's specs via ginkgo -p (ITEST_PROCS processes, default 4; override
#     with `make ITEST_PROCS=8 itest-acp`).
#   - Each suite depends on `itest-mockllm`: the contract tests lock the
#     mock's wire format against the real SDK clients, so a wire-format
#     regression fails fast before the suites run.
GINKGO := go run github.com/onsi/ginkgo/v2/ginkgo
ITEST_PROCS ?= 4

itest: web-check
	$(MAKE) -j5 itest-mockllm itest-run itest-tui itest-acp itest-headless

# Contract + unit tests for the mock LLM server (no integration build tag
# needed — the contract tests run under plain `go test` too).
itest-mockllm:
	go test ./itest/mockllm

itest-run: itest-mockllm
	$(GINKGO) -p --procs=$(ITEST_PROCS) -tags=integration ./itest/run

itest-tui: itest-mockllm
	$(GINKGO) -p --procs=$(ITEST_PROCS) -tags=integration ./itest/tui

itest-acp: itest-mockllm
	$(GINKGO) -p --procs=$(ITEST_PROCS) -tags=integration ./itest/acp

# itest-headless 驱动无头宿主的**真二进制**（`-tags headless`，CGO_ENABLED=0，不需要 GUI）：
# 起一个进程、HTTP 建会话、WS 收一轮、再重订阅核对记录。go test 而不是 ginkgo ——
# 三个用例不需要 spec 并行，而编译一次二进制比调度并发更贵。
# -count=1：这条路最怕的是缓存把「这次没跑」说成绿的。
itest-headless: itest-mockllm
	go test -tags integration -count=1 ./itest/headless

# desktop-smoke drives the REAL desktop app (see docs/agents/desktop.md): a throwaway
# bundle, scripted model, and a driver that asserts what the UI shows. Not part of
# `itest`: it needs the GUI (a `make -C desktop build` bundle and a window server).
# Pass a subset with `make desktop-smoke ARGS="-run send-now -v"`.
# GOWORK=off on the desktop build: desktop/ is a self-contained module and is NOT listed
# in the repo-root go.work, so a `go build` inside it under workspace mode fails with
# "main module (github.com/monsterxx03/tachi) does not contain package .../desktop".
desktop-smoke:
	cd desktop && $(MAKE) frontend && GOWORK=off go build -o bin/tachi-desktop .
	go run ./itest/desktop $(ARGS)

