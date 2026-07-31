package tools

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// ToolArgsSummary extracts the key argument from a tool call's JSON args
// and returns a human-readable string. No truncation — callers apply
// their own length limits as needed.
//
// Returns the raw args JSON string if parsing fails.
func ToolArgsSummary(name, argsJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return argsJSON
	}

	switch name {
	case ToolNameRead:
		if p, ok := args["path"].(string); ok {
			offset, hasOffset := toInt(args["offset"])
			limit, hasLimit := toInt(args["limit"])
			if hasOffset && offset > 0 {
				if hasLimit && limit > 0 {
					return fmt.Sprintf("%s L%d+%d", p, offset, limit)
				}
				return fmt.Sprintf("%s L%d", p, offset)
			}
			if hasLimit && limit > 0 {
				return fmt.Sprintf("%s +%d", p, limit)
			}
			return p
		}
	case ToolNameWrite, ToolNameEdit:
		if p, ok := args["path"].(string); ok {
			return p
		}
	case ToolNameGlob, ToolNameGrep:
		if p, ok := args["pattern"].(string); ok {
			return p
		}
	case ToolNameBash:
		if cmd, ok := args["command"].(string); ok {
			if bg, ok := args["background"].(bool); ok && bg {
				if name, ok := args["bg_name"].(string); ok && name != "" {
					return fmt.Sprintf("[bg:%s] %s", name, cmd)
				}
				return "[bg] " + cmd
			}
			return cmd
		}
	case ToolNameWebSearch:
		if q, ok := args["query"].(string); ok {
			return q
		}
	case ToolNameWebFetch:
		if u, ok := args["url"].(string); ok {
			return u
		}
	case ToolNameLSP:
		if op, ok := args["operation"].(string); ok {
			if p, ok := args["path"].(string); ok {
				return fmt.Sprintf("%s %s", op, p)
			}
			return op
		}
	case ToolNameSubAgent:
		if prompt, ok := args["prompt"].(string); ok {
			if branch, ok := args["worktree_branch"].(string); ok && branch != "" {
				return fmt.Sprintf("[%s] %s", branch, prompt)
			}
			return prompt
		}
	case ToolNameSkill:
		if op, ok := args["operation"].(string); ok {
			if n, ok := args["name"].(string); ok {
				return op + " " + n
			}
			return op
		}
	case ToolNameMCPSearchTools:
		if q, ok := args["query"].(string); ok {
			return q
		}
	case ToolNameSendFile:
		if p, ok := args["path"].(string); ok {
			return p
		}
	}
	// Fallback: format all top-level arguments for unknown tools.
	return formatFullArgs(args)
}

// ToolArgsTitle builds a concise title for tool call display (e.g. Zed's ACP
// tool card). Uses base filename for paths and truncates long strings.
func ToolArgsTitle(name, argsJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return name
	}

	switch name {
	case ToolNameRead, ToolNameWrite, ToolNameEdit:
		if p, ok := args["path"].(string); ok {
			return path.Base(p)
		}
	case ToolNameGlob, ToolNameGrep:
		if p, ok := args["pattern"].(string); ok {
			return TruncateForTitle(p, 50)
		}
	case ToolNameBash:
		if cmd, ok := args["command"].(string); ok {
			if bg, ok := args["background"].(bool); ok && bg {
				if name, ok := args["bg_name"].(string); ok && name != "" {
					return TruncateForTitle(fmt.Sprintf("[bg:%s] %s", name, cmd), 60)
				}
				return TruncateForTitle("[bg] "+cmd, 60)
			}
			return TruncateForTitle(cmd, 60)
		}
	case ToolNameWebSearch:
		if q, ok := args["query"].(string); ok {
			return TruncateForTitle(q, 50)
		}
	case ToolNameWebFetch:
		if u, ok := args["url"].(string); ok {
			return TruncateForTitle(u, 50)
		}
	case ToolNameLSP:
		if op, ok := args["operation"].(string); ok {
			if p, ok := args["path"].(string); ok {
				return fmt.Sprintf("%s %s", op, path.Base(p))
			}
			return op
		}
	case ToolNameSubAgent:
		if prompt, ok := args["prompt"].(string); ok {
			return TruncateForTitle(prompt, 50)
		}
	case ToolNameSkill:
		if op, ok := args["operation"].(string); ok {
			if n, ok := args["name"].(string); ok {
				return fmt.Sprintf("%s %s", op, n)
			}
			return op
		}
	case ToolNameMCPSearchTools:
		if q, ok := args["query"].(string); ok {
			return TruncateForTitle(q, 50)
		}
	case ToolNameSendFile:
		if p, ok := args["path"].(string); ok {
			return path.Base(p)
		}
	}
	// Fallback: use the first key=value pair from the args.
	return formatFullArgsTitle(args)
}

// TruncateForTitle truncates s to maxLen runes, taking only the first line
// and appending "…" if truncated.
func TruncateForTitle(s string, maxLen int) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	runes := []rune(s)
	if len(runes) > maxLen {
		return string(runes[:maxLen-1]) + "…"
	}
	return s
}

// toInt converts an any value to int. Accepts float64 (JSON number),
// int, and int64.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// formatFullArgs formats all top-level args as "key1=value1, key2=value2".
// Values are converted to strings and truncated. Returns empty string if no args.
func formatFullArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	var parts []string
	for k, v := range args {
		s := fmt.Sprint(v)
		runes := []rune(s)
		if len(runes) > 40 {
			s = string(runes[:37]) + "..."
		}
		parts = append(parts, k+"="+s)
	}
	// Sort for deterministic output.
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// formatFullArgsTitle returns a short title from the first arg key-value pair.
func formatFullArgsTitle(args map[string]any) string {
	for k, v := range args {
		s := fmt.Sprint(v)
		return TruncateForTitle(k+": "+s, 50)
	}
	return ""
}
