# Tachi desktop — build/run targets (Wails v3, macOS).
#
# Usage:
#   make build    → wails3 package → bin/Tachi.app (frontend + go + codesign)
#   make test     → go test ./...（这个模块自己的，根 go.work 看不见它）
#   make run      → build then `open -n bin/Tachi.app`
#   make dev      → wails3 dev (hot reload)
#   make appicon  → 由 build/appicon-source.jpg 重新生成应用图标（appicon.png + Icon Composer 资源）
#   make frontend → npm run build in frontend/
#   make tidy     → go mod tidy
#   make clean    → remove bin/ and frontend/dist
#
# Wails v3 CLI is installed at ~/go/bin/wails3.

GO        ?= go
WAILS     ?= wails3
NPM       ?= npm
APPDIR     = bin/Tachi.app

# LOCAL_SIGN_ID is the local, self-signed code-signing identity used by
# `make sign-local`. macOS ties TCC grants — notification permission included —
# to an app's code identity, and `wails3 package` leaves the bundle AD-HOC
# signed, whose identity is a content hash: it changes on every rebuild, so the
# permission the user granted is forgotten and the app can end up with
# notifications denied for good ("Notifications are not allowed for this
# application"). Re-signing with a certificate keeps the identity stable across
# rebuilds. Create it once (Keychain Access → Certificate Assistant, or a
# self-signed cert trusted for code signing) and it lives in the login keychain.
LOCAL_SIGN_ID ?= Tachi Local Code Signing

export PATH := $(HOME)/go/bin:$(PATH)

# The repo-root go.work (../go.work) does not list this module, but Go tools
# discover it from the parent directory anyway — leaving desktop "invisible"
# to `go list` and breaking wails3 bindings generation (0 Services → frontend
# TS errors). Desktop is a self-contained module (go.mod repeats the fork
# replaces it needs), so build it outside the workspace. Targets that spawn Go
# tooling (build/dev/run + their task dependencies) inherit this.
export GOWORK := off

.PHONY: all build dev appicon frontend tidy clean run sign-local stt-shim

all: build

# stt-shim 把 Swift 垫片（stt/shim.swift）编成静态库给 cgo 链。
#
# 它是 build 的前置而不是独立步骤：产物不入库（是编译结果），漏掉它的表现是链接期
# 报一句找不到 stt/libtachi_stt.a——而 cgo 会把那句话埋在一整行 clang 命令后面，
# 看着像天书。所以让构建自己去跑，别指望人记得。
#
# 为什么这里会有 Swift：macOS 26 的转写 API 只在 Swift 的模块接口里存在（ObjC 头里
# 只有老的 SFSpeechRecognizer），见 docs/2026-09-29-badge-voice-answers-design.md §7。
stt-shim:
	@stt/build_shim.sh

build: stt-shim
	$(WAILS) package

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
	open -n $(APPDIR)

clean:
	rm -rf bin frontend/dist

# sign-local re-signs the packaged app with the local certificate so its code
# identity (and therefore its notification permission) survives rebuilds.
# Run it after `make build`.
sign-local:
	test -d $(APPDIR) || (echo "no $(APPDIR) yet — run make build first"; exit 1)
	codesign --force --deep --sign "$(LOCAL_SIGN_ID)" $(APPDIR)
	codesign -dv --verbose=2 $(APPDIR) 2>&1 | grep -E "Authority|flags"
