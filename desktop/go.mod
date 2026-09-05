module github.com/monsterxx03/tachi/desktop

go 1.26.5

require (
	github.com/monsterxx03/tachi v0.0.0
	github.com/wailsapp/wails/v3 v3.0.0-beta.16
)

require (
	github.com/JohannesKaufmann/dom v0.2.0 // indirect
	github.com/JohannesKaufmann/html-to-markdown/v2 v2.5.0 // indirect
	github.com/adrg/xdg v0.5.3 // indirect
	github.com/anthropics/anthropic-sdk-go v1.37.0 // indirect
	github.com/coder/acp-go-sdk v0.13.5 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/creasty/defaults v1.8.0 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jchv/go-winloader v0.0.0-20250406163304-c1995be93bd1 // indirect
	github.com/mark3labs/mcp-go v0.57.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.21 // indirect
	github.com/openai/openai-go/v3 v3.49.0 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/sashabaranov/go-openai v1.41.2 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/tidwall/gjson v1.19.0 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	mvdan.cc/sh/v3 v3.13.1 // indirect
)

replace github.com/monsterxx03/tachi => ../

// These replaces are required because the tachi source (imported under the
// replace above) relies on patched forks that are NOT inherited by a downstream
// module. They must be repeated here for the desktop build to compile.
replace github.com/coder/acp-go-sdk => github.com/monsterxx03/acp-go-sdk v0.13.6-0.20260808163713-6be58aef93e8

replace github.com/sashabaranov/go-openai => github.com/monsterxx03/go-openai v1.41.2-extrabody
