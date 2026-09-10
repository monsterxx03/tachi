# Tachi desktop — build/run targets (Wails v3, macOS).
#
# Usage:
#   make build    → wails3 package → bin/Tachi.app (frontend + go + codesign)
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

export PATH := $(HOME)/go/bin:$(PATH)

# The repo-root go.work (../go.work) does not list this module, but Go tools
# discover it from the parent directory anyway — leaving desktop "invisible"
# to `go list` and breaking wails3 bindings generation (0 Services → frontend
# TS errors). Desktop is a self-contained module (go.mod repeats the fork
# replaces it needs), so build it outside the workspace. Targets that spawn Go
# tooling (build/dev/run + their task dependencies) inherit this.
export GOWORK := off

.PHONY: all build dev appicon frontend tidy clean run

all: build

build:
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

tidy:
	$(GO) mod tidy

run: build
	open -n $(APPDIR)

clean:
	rm -rf bin frontend/dist
