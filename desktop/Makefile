# Tachi desktop — build/run targets (Wails v3, macOS).
#
# Usage:
#   make build    → wails3 package → bin/tachi-desktop.app (frontend + go + codesign)
#   make run      → build then `open -n bin/tachi-desktop.app`
#   make dev      → wails3 dev (hot reload)
#   make frontend → npm run build in frontend/
#   make tidy     → go mod tidy
#   make clean    → remove bin/ and frontend/dist
#
# Wails v3 CLI is installed at ~/go/bin/wails3.

GO        ?= go
WAILS     ?= wails3
NPM       ?= npm
APPDIR     = bin/tachi-desktop.app

export PATH := $(HOME)/go/bin:$(PATH)

.PHONY: all build dev frontend tidy clean run

all: build

build:
	$(WAILS) package

dev:
	$(WAILS) dev

frontend:
	cd frontend && $(NPM) run build

tidy:
	$(GO) mod tidy

run: build
	open -n $(APPDIR)

clean:
	rm -rf bin frontend/dist
