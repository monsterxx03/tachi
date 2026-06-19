.PHONY: build install clean dev pack icons scripts

# Tachi Chrome Extension Build

SRC := manifest.json background.js content.js toast.css popup.html popup.js sidepanel.html
DIST := dist

build: icons
	@mkdir -p $(DIST)/icons
	@cp $(SRC) $(DIST)/
	@cp icons/*.png $(DIST)/icons/
	@echo "✅ Extension built in $(DIST)/"
	@echo "   Load it in chrome://extensions (Developer Mode)"

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
