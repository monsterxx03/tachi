package systemreminder

import (
	"fmt"
	"sort"
	"strings"
)

// LSPStatusProvider provides LSP server status information for the reminder.
type LSPStatusProvider interface {
	IsConfigured() bool
	ServerInfos() []LSPServerInfo
}

// LSPServerInfo is a minimal representation of an LSP server for the reminder.
type LSPServerInfo struct {
	Name       string
	Ready      bool
	Extensions []string
}

// LSPStatusReminder injects the status of configured LSP servers into the
// <system-reminder> block. Only fires on the first user message of a
// conversation.
type LSPStatusReminder struct {
	Provider LSPStatusProvider
}

func (r *LSPStatusReminder) Generate(ctx Context) []string {
	if r.Provider == nil || !r.Provider.IsConfigured() {
		return nil
	}
	if !ctx.IsFirstMessage {
		return nil
	}

	servers := r.Provider.ServerInfos()
	if len(servers) == 0 {
		return nil
	}

	sort.Slice(servers, func(i, j int) bool {
		return servers[i].Name < servers[j].Name
	})

	var lines []string
	lines = append(lines, "Available LSP servers:")

	for _, srv := range servers {
		exts := strings.Join(srv.Extensions, ", ")
		status := "⏳ starting"
		if srv.Ready {
			status = "✓ ready"
		}
		lines = append(lines, fmt.Sprintf("  %s: %s (%s)", srv.Name, status, exts))
	}

	lines = append(lines, "")
	lines = append(lines, "Use the LSP tool for code intelligence (goToDefinition, findReferences, hover, etc.).")

	return lines
}
