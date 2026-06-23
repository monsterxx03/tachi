.PHONY: build install clean dev pack icons scripts deps

# Tachi Chrome Extension Build

SRC := manifest.json background.js popup.html popup.js
DIST := dist
BUNDLE_ENTRY := content.js

build: deps icons
	@mkdir -p $(DIST)/icons
	@echo "📦 Bundling content.js with Readability..."
	@npx esbuild $(BUNDLE_ENTRY) --bundle --platform=browser --format=iife --outfile=$(DIST)/content.js
	@cp $(SRC) $(DIST)/
	@cp icons/*.png $(DIST)/icons/
	@echo "✅ Extension built in $(DIST)/"
	@echo "   Load it in chrome://extensions (Developer Mode)"

deps:
	@if [ ! -d "node_modules" ]; then \
		echo "📦 Installing npm dependencies..."; \
		npm install; \
	fi

icons:
	@python3 scripts/gen_icons.py

install: build
	@echo "📦 Loading extension..."
	@if command -v open > /dev/null; then \
		open "chrome://extensions"; \
	fi

pack: build
	@echo "📦 Packing extension..."
	@if [ -f "key.pem" ]; then \
		echo "Use Chrome to pack with key.pem"; \
	else \
		cd $(DIST) && zip -r ../tachi-chrome-extension.zip . && cd ..; \
		echo "Created tachi-chrome-extension.zip"; \
	fi

clean:
	@rm -rf $(DIST) tachi-chrome-extension.zip
	@echo "✅ Cleaned"
