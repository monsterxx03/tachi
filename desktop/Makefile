# Tachi desktop — build/run targets (Wails v3, macOS + Linux).
#
# Usage:
#   make build    → macOS: wails3 package → bin/Tachi.app (frontend + go + codesign)
#                   Linux: go build → bin/tachi-desktop（前端要先用 make frontend 出 dist）
#   make test     → go test ./...（这个模块自己的，根 go.work 看不见它）
#   make run      → macOS: build then `open -n bin/Tachi.app`；Linux: build then 直接跑
#   make dev      → wails3 dev (hot reload)
#   make appicon  → 由 build/appicon-source.jpg 重新生成应用图标（appicon.png + Icon Composer 资源，macOS 专用）
#   make frontend → npm run build in frontend/
#   make tidy     → go mod tidy
#   make clean    → remove bin/ and frontend/dist
#
# Wails v3 CLI is installed at ~/go/bin/wails3.
#
# 两个平台各走各的：macOS 用 wails3 package（.app bundle + 签名）；Linux 直接用 go build，
# 出包（appimage/deb/rpm/aur）交给 `wails3 package` / `task linux:package`——那套要 wails3
# CLI，而日常开发（本目标）不该被它挡住。两边的差异都收在下面 UNAME_S 的分支里。

GO        ?= go
WAILS     ?= wails3
NPM       ?= npm
APPDIR     = bin/Tachi.app

# 平台分支用得到的两个：uname 选分支；Linux 的产物名要和 .desktop / nfpm 包一致
# （build/linux/tachi-desktop.desktop 的 Exec、build/linux/nfpm/nfpm.yaml 的 contents
# 都写的是 tachi-desktop）。
UNAME_S   := $(shell uname -s)
LINUX_BIN  = bin/tachi-desktop

# LOCAL_SIGN_ID is the local, self-signed code-signing identity the build signs with (see the
# `build` target: it does this whenever the identity exists). macOS ties TCC grants — bluetooth,
# speech recognition, notifications — to an app's code identity, and `wails3 package` leaves the
# bundle AD-HOC signed, whose identity is a content hash: it changes on every rebuild, so every
# permission the user ever granted is forgotten and asked again (and notifications can end up
# denied for good: "Notifications are not allowed for this application"). A certificate makes the
# identity survive rebuilds.
#
# Create it once with ./scripts/dev-cert.sh (it generates the cert, trusts it for code signing
# and keeps an exportable identity.p12 under ~/.tachi/dev-signing/). **On another machine**, copy
# that identity.p12 over and run the two `security import` / `security add-trusted-cert` lines the
# script prints — same certificate, same app identity. (Re-creating a certificate there instead is
# fine too; the only cost is that machine asking once, because TCC's records are per-machine.)
LOCAL_SIGN_ID ?= Tachi Local Code Signing

export PATH := $(HOME)/go/bin:$(PATH)

# The repo-root go.work (../go.work) does not list this module, but Go tools
# discover it from the parent directory anyway — leaving desktop "invisible"
# to `go list` and breaking wails3 bindings generation (0 Services → frontend
# TS errors). Desktop is a self-contained module (go.mod repeats the fork
# replaces it needs), so build it outside the workspace. Targets that spawn Go
# tooling (build/dev/run + their task dependencies) inherit this.
export GOWORK := off

.PHONY: all build package dev appicon frontend tidy clean run sign-local stt-shim

all: build

# stt-shim 把 Swift 垫片（stt/shim.swift）编成静态库给 cgo 链。
#
# 它是 build 的前置而不是独立步骤：产物不入库（是编译结果），漏掉它的表现是链接期
# 报一句找不到 stt/libtachi_stt.a——而 cgo 会把那句话埋在一整行 clang 命令后面，
# 看着像天书。所以让构建自己去跑，别指望人记得。
#
# 为什么这里会有 Swift：macOS 26 的转写 API 只在 Swift 的模块接口里存在（ObjC 头里
# 只有老的 SFSpeechRecognizer），见 docs/2026-09-29-badge-voice-answers-design.md §7。
#
# 只在 macOS 上跑：Linux 的 stt 走 stt/stt_other.go（纯 Go 桩，没有 cgo 静态库要链），
# 而这个脚本本身要 swiftc/xcrun——非 darwin 上不但没必要，还会直接把构建打断。
stt-shim:
ifeq ($(UNAME_S),Darwin)
	@stt/build_shim.sh
else
	@echo "stt-shim: 跳过（$(UNAME_S)；语音转写在本平台不接）"
endif

build: stt-shim
ifeq ($(UNAME_S),Darwin)
	$(WAILS) package
	@if security find-identity -v -p codesigning 2>/dev/null | grep -qF "$(LOCAL_SIGN_ID)"; then \
		$(MAKE) --no-print-directory sign-local; \
	else \
		echo "⚠ 没有本地签名证书（$(LOCAL_SIGN_ID)）：这个包是 ad-hoc 签名的，而 ad-hoc 的身份"; \
		echo "  是内容哈希——重建一次就换一个身份，macOS 于是把蓝牙/语音/通知的权限再问一遍。"; \
		echo "  建一张：./scripts/dev-cert.sh（一次就够，之后重建多少次都还是同一个 app）。"; \
	fi
else
	@test -d frontend/dist || { echo "frontend/dist 不存在——先跑 make frontend（或 wails3 package）"; exit 1; }
	$(GO) build -o $(LINUX_BIN) .
	@echo "→ $(LINUX_BIN)（出安装包用 make package 或 task linux:package）"
endif

# package 是 Linux 的出包入口（appimage/deb/rpm/aur，走 build/linux/Taskfile.yml）。
# macOS 的 .app/dmg 打包仍在 build 里（wails3 package）。
package:
ifeq ($(UNAME_S),Darwin)
	@echo "macOS 的包由 make build 产出（bin/Tachi.app）；dmg 见 task darwin:package:dmg"
else
	$(WAILS) package
endif

dev:
	$(WAILS) dev

# appicon regenerates the macOS app icon from build/appicon-source.jpg. The
# output is a rounded square with a transparent surround (macOS does not mask
# app icons for you), then `make build` recompiles icons.icns from it.
appicon:
	swift build/make-appicon.swift

frontend:
	cd frontend && $(NPM) run build

# test 跑这个模块的测试。前置 stt-shim 是必需的，而不是顺手：desktop/stt 要链接那个
# 静态库，缺了它是**整个包编译不过**（不是个别用例失败），而报错藏在 cgo 的一长串
# 命令行里。所以走这个目标比手敲 go test 稳。
test: stt-shim
	$(GO) test ./...

tidy:
	$(GO) mod tidy

run: build
ifeq ($(UNAME_S),Darwin)
	open -n $(APPDIR)
else
	./$(LINUX_BIN)
endif

clean:
	rm -rf bin frontend/dist

# sign-local re-signs the packaged app with the local certificate so its code
# identity (and therefore its notification permission) survives rebuilds.
# Run it after `make build`.
sign-local:
	test -d $(APPDIR) || (echo "no $(APPDIR) yet — run make build first"; exit 1)
	codesign --force --deep --sign "$(LOCAL_SIGN_ID)" $(APPDIR)
	codesign -dv --verbose=2 $(APPDIR) 2>&1 | grep -E "Authority|flags"
