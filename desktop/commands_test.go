package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
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
	for _, want := range []string{"compact", "review", "commit", "sh"} {
		if !seen[want] {
			t.Errorf("/%s is missing from ListCommands()", want)
		}
	}
}

func TestShellEcho(t *testing.T) {
	tests := []struct {
		name string
		out  string
		note string
		want string
	}{
		{"plain output", "total 12\ndrwxr-xr-x  3 will", "", "total 12\ndrwxr-xr-x  3 will"},
		{"failure keeps the output and the code", "error: pathspec", "(exit 1)", "error: pathspec\n(exit 1)"},
		{"note without output", "", "(exit 127)", "(exit 127)"},
		{"silent success says so", "", "", "(no output)"},
		{"whitespace-only output counts as silent", "  \n ", "", "(no output)"},
		{"timeout note", "", "⏱️ 超时，进程已终止", "⏱️ 超时，进程已终止"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellEcho(tt.out, tt.note); got != tt.want {
				t.Errorf("shellEcho(%q, %q) = %q, want %q", tt.out, tt.note, got, tt.want)
			}
		})
	}
}

// An empty /sh is a usage hint, not a failure: the command still has to close its
// turn normally, otherwise the transcript keeps a running placeholder forever.
func TestRunShellCommandEmptyIsUsageNotError(t *testing.T) {
	events := make(chan agent.AgentEvent, 1)
	if err := runShellCommand(&commandRun{args: "   ", ech: events}); err != nil {
		t.Fatalf("runShellCommand(empty) = %v, want nil", err)
	}
	ev := <-events
	if ev.Type != agent.AgentEventTextDelta || !strings.Contains(ev.TextDelta, "用法") {
		t.Errorf("empty /sh emitted %+v, want a usage text delta", ev)
	}
}

// A /sh run must mark its synthesized tool call as user-requested, or the card
// would fold its output away — hiding the very thing the command was run for.
func TestRunShellCommandMarksCardForExpansion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events := make(chan agent.AgentEvent, 4)
	desk := newDesktopApp()

	go func() {
		defer close(events)
		if err := runShellCommand(&commandRun{desk: desk, ctx: ctx, args: "echo tachi-sh", ech: events}); err != nil {
			t.Errorf("runShellCommand() = %v", err)
		}
	}()

	var start, result *agent.AgentEvent
	for ev := range events {
		switch ev.Type {
		case agent.AgentEventToolCallStart:
			e := ev
			start = &e
		case agent.AgentEventToolResult:
			e := ev
			result = &e
		}
	}
	if start == nil || !start.ToolAutoExpand {
		t.Fatalf("tool call start = %+v, want ToolAutoExpand", start)
	}
	if start.ToolName != tools.ToolNameBash {
		t.Errorf("tool name = %q, want the Bash card shape", start.ToolName)
	}
	if result == nil || !strings.Contains(result.ToolResult, "tachi-sh") {
		t.Fatalf("tool result = %+v, want the command's output", result)
	}
	if result.ToolIsError {
		t.Errorf("a successful command was reported as an error: %+v", result)
	}
}
