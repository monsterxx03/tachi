# Tachi desktop — build/run targets (Wails v3, macOS + Linux).
#
# Usage:
#   make build    → macOS: wails3 package → bin/tachi-desktop.app（frontend + go + codesign，内含 CLI）
#                   Linux: go build → bin/tachi-desktop（前端要先用 make frontend 出 dist）
#   make cli      → 构建根模块的 tachi（CLI/TUI）到 bin/tachi —— 就是进包/.app 的那一个
#   make package  → Linux: deb / rpm / arch 三个包（wails3，见下；内含 tachi + tachi-desktop）
#                   macOS: 一个 .pkg 安装器（app 进 /Applications，CLI 进 /usr/local/bin）
#   make test     → go test ./...（这个模块自己的，根 go.work 看不见它）
#   make run      → macOS: build then `open -n bin/tachi-desktop.app`；Linux: build then 直接跑
#   make dev      → wails3 dev (hot reload)
#   make appicon  → 由 build/appicon-source.jpg 重新生成应用图标（appicon.png + Icon Composer 资源，macOS 专用）
#   make frontend → npm run build in frontend/
#   make bar-widget → 装 omarchy bar 控件到 ~/.config/omarchy/plugins/（见 omarchy/README.md）
#   make tidy     → go mod tidy
#   make clean    → remove bin/ and frontend/dist
#
# Wails v3 CLI 装在 ~/go/bin/wails3（下面 export PATH 把它带上）。出包和 dev 都要它；日常的
# build/test 不要——那条路是纯 go build，装不装 CLI 都能跑。

GO        ?= go
WAILS     ?= wails3
NPM       ?= npm
APPDIR     = bin/tachi-desktop.app

# 平台分支用得到的两个：uname 选分支；Linux 的产物名要和 .desktop / nfpm 包一致
# （build/linux/tachi-desktop.desktop 的 Exec、build/linux/nfpm/nfpm.yaml 的 contents
# 都写的是 tachi-desktop）。
UNAME_S   := $(shell uname -s)
LINUX_BIN  = bin/tachi-desktop

# CLI_OUT is where `make cli` writes the CLI/TUI binary — the ROOT module's `tachi`, which the Linux
# package installs next to the desktop app and which macOS's bundle carries in Contents/MacOS.
# Written as $(CURDIR): the target exists to be called from other directories (the Linux Taskfile's
# build:cli), where a bare relative path would land somewhere else.
CLI_OUT   ?= $(CURDIR)/bin/tachi

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

.PHONY: all build cli headless package package-appimage dev appicon frontend bar-widget tidy clean run sign-local stt-shim

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
	@echo "→ $(LINUX_BIN)（CLI 用 make cli；出安装包用 make package）"
endif

# cli 构建根模块的 tachi（CLI/TUI）到 CLI_OUT。它是两个平台出包时都要的那个二进制：Linux 装到
# /usr/local/bin/tachi（nfpm 的 contents），macOS 进 .app 的 Contents/MacOS——两边的打包链都只是
# **调用本目标**（Taskfile 的 common:build:cli），不再各写一条 go build。
#
# 之所以走根 Makefile：版本号（git describe）、-trimpath，以及**内嵌的 web console**
# （web/frontend/dist——缺了它根模块根本编译不过，对应根 Makefile 的 web-check）都定义在那边一处。
# 本 Makefile 导出的 GOWORK=off 会被继承，根模块因此按自己的 go.mod 构建，正是我们要的。
cli:
	$(MAKE) -C .. --no-print-directory build CLI_OUT=$(CLI_OUT)
	@echo "→ $(CLI_OUT)"

# headless 构建**无窗口宿主**：会话运行时 + 手机服务端，没有窗口、托盘、菜单、通知、BLE、麦克风。
#
# 与 build 的区别是全部的：没有 wails3、没有 frontend（无头宿主没有 webview）、没有 cgo
# （CGO_ENABLED=0，所以不需要 gtk / webkit 头文件），产物是一个静态链接的二进制。这就是它能在一台
# 没有图形环境的机器上跑起来的原因（NAS、服务器、容器）——`file` 说它 statically linked，`otool -L`
# / `ldd` 说它不依赖任何东西。
#
# 目标平台跟着宿主走：在 Mac 上产出的是 Mach-O，要给 Linux 用就 GOOS=linux GOARCH=amd64（或 arm64）。
# 手机上要的那个令牌与开关在 ~/.tachi（mobile.enabled / mobile.json），不在构建里。
HEADLESS_BIN = bin/tachi-headless
headless:
	CGO_ENABLED=0 $(GO) build -tags headless -trimpath -o $(HEADLESS_BIN) .
	@echo "→ $(HEADLESS_BIN)"

# package 是出包入口：Linux 是 deb / rpm / arch 三种，走 build/linux/Taskfile.yml 的
# linux:package（nfpm 直接写这三种格式，不依赖宿主上的打包工具）；macOS 是一个 .pkg 安装器（app
# 进 /Applications、CLI 进 /usr/local/bin），走 build/darwin/Taskfile.yml 的 create:pkg
# （pkgbuild/productbuild，系统自带）。.dmg 仍是另一条：task darwin:package:dmg——它只把 app 拖进去，
# CLI 不会上 PATH。
#
# 为什么 AppImage 不在这里：linuxdeploy 的 GTK 插件假定 Debian 那套 gdk-pixbuf 布局
# （/usr/lib/<arch>/gdk-pixbuf-2.0/2.10.0），Arch 系主机上没有那个目录，于是它跑到一半就
# "Failed to run plugin: gtk"。它在能跑的主机上仍然是 `make package-appimage`。
package:
ifeq ($(UNAME_S),Darwin)
	@$(MAKE) --no-print-directory build
	@$(WAILS) task darwin:create:pkg
	@ls -1 bin/*.pkg | sed 's/^/→ /'
else
	$(WAILS) package
	@ls -1 bin/*.deb bin/*.rpm bin/*.pkg.tar.zst 2>/dev/null | sed 's/^/→ /'
endif

package-appimage:
	$(WAILS) task linux:package:appimage

dev:
	$(WAILS) dev

# appicon regenerates the macOS app icon from build/appicon-source.jpg. The
# output is a rounded square with a transparent surround (macOS does not mask
# app icons for you), then `make build` recompiles icons.icns from it.
appicon:
	swift build/make-appicon.swift

frontend:
	cd frontend && $(NPM) run build

# bar-widget 把 omarchy bar 控件（omarchy/）装到用户配置里。
#
# 母本在仓库（desktop/omarchy/），装到 ~/.config/omarchy/plugins/<id>/ 的是一份拷贝 —— 改那个
# 目录里的文件会在下次安装时被覆盖，所以改的是仓库这份，然后重跑本目标（见 omarchy/README.md）。
#
# 和 `omarchy plugin clone` 一个道理（它也是拷贝），不软链接：仓库挪走之后插件还得能用。
bar-widget:
	install -d $(HOME)/.config/omarchy/plugins/monsterxx03.tachi
	install -m644 omarchy/manifest.json omarchy/Tachi.qml $(HOME)/.config/omarchy/plugins/monsterxx03.tachi/
	@echo "→ ~/.config/omarchy/plugins/monsterxx03.tachi/（已装）"
	@echo "  改完要 omarchy restart shell 才生效（bar 控件不会热重载已挂上去的实例）；"
	@echo "  第一次还要放位置：omarchy bar put monsterxx03.tachi --after omarchy.agents"

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
