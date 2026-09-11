package main

import (
	"strings"
	"testing"
)

// The dispatch contract: the frontend sends whatever the user typed after "/",
// and gets either "" (started) or a human-readable refusal it shows as a notice.
// These are the refusals that must not turn into a pretend turn.
func TestRunCommandRefusals(t *testing.T) {
	svc := &AgentService{desk: newDesktopApp()}

	tests := []struct {
		name string
		in   string
		want string // substring
	}{
		{"empty", "/", "空命令"},
		{"whitespace only", "   ", "空命令"},
		{"unknown name", "/nope", "未知命令：/nope"},
		{"unknown keeps first field", "/nope with args", "未知命令：/nope"},
		{"known but not implemented here", "/usage", "desktop 暂不支持 /usage"},
		{"no active session", "/compact", "没有活跃会话"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := svc.RunCommand(tt.in)
			if !strings.Contains(got, tt.want) {
				t.Errorf("RunCommand(%q) = %q, want it to contain %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestListCommandsOffersOnlyImplementedCommands(t *testing.T) {
	svc := &AgentService{desk: newDesktopApp()}
	list := svc.ListCommands()
	if len(list) == 0 {
		t.Fatal("ListCommands() is empty — the composer's palette would have nothing to show")
	}
	seen := map[string]bool{}
	for _, c := range list {
		if _, ok := desktopCommandHandlers[c.Name]; !ok {
			t.Errorf("command %q is advertised without a desktop handler", c.Name)
		}
		if c.Description == "" {
			t.Errorf("command %q has no description (the palette shows it)", c.Name)
		}
		seen[c.Name] = true
	}
	for _, want := range []string{"compact", "review", "commit"} {
		if !seen[want] {
			t.Errorf("/%s is missing from ListCommands()", want)
		}
	}
}
