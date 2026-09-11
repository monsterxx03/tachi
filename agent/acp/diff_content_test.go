package acp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/agent/tools"
)

// TestBuildDiffFromArgs covers the derivation boundary: only a file-changing call
// with usable text produces a block.
func TestBuildDiffFromArgs(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		args     string
		wantSome bool
	}{
		{"edit", tools.ToolNameEdit, `{"path":"/a.go","old_string":"a","new_string":"b"}`, true},
		{"write", tools.ToolNameWrite, `{"path":"/a.go","content":"x"}`, true},
		{"read is not a change", tools.ToolNameRead, `{"path":"/a.go"}`, false},
		{"bash is not a change", tools.ToolNameBash, `{"command":"sed -i s/a/b/ /a.go"}`, false},
		{"empty args", tools.ToolNameEdit, "", false},
		{"unparsable args", tools.ToolNameEdit, `{"path":`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildDiffFromArgs(tt.tool, tt.args)
			if (got != nil) != tt.wantSome {
				t.Errorf("buildDiffFromArgs(%s) = %v, wantSome %v", tt.tool, got, tt.wantSome)
			}
		})
	}
}

// TestDiffContentPreservesTheWireFormat is the regression this refactor needed: the
// tool-call stream used to call ToolDiffContent directly, passing the old text ONLY
// for edits. Routing everything through fileChangeContent must keep that — a create
// has to send NO oldText at all, because the SDK reads a nil OldText as "new file"
// ("The original content (None for new files)", types_gen.go). Sending "" instead
// would tell every ACP client that the file used to be empty.
//
// The golden strings are the pre-refactor wire format, byte for byte.
func TestDiffContentPreservesTheWireFormat(t *testing.T) {
	t.Run("edit carries both sides", func(t *testing.T) {
		got := buildDiffFromArgs(tools.ToolNameEdit, `{"path":"/a.go","old_string":"old\n","new_string":"new\n"}`)
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		const want = `{"newText":"new\n","oldText":"old\n","path":"/a.go","type":"diff"}`
		if string(raw) != want {
			t.Errorf("wire format changed\n got %s\nwant %s", raw, want)
		}
	})

	t.Run("create sends no oldText at all", func(t *testing.T) {
		got := buildDiffFromArgs(tools.ToolNameWrite, `{"path":"/new.go","content":"x\n"}`)
		if got == nil || got.Diff == nil {
			t.Fatal("expected a diff block for a WriteFile")
		}
		if got.Diff.OldText != nil {
			t.Errorf("OldText = %q, want nil (a create has no old side)", *got.Diff.OldText)
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		const want = `{"newText":"x\n","path":"/new.go","type":"diff"}`
		if string(raw) != want {
			t.Errorf("wire format changed\n got %s\nwant %s", raw, want)
		}
		if strings.Contains(string(raw), "oldText") {
			t.Error(`the wire form must omit "oldText", not send it empty`)
		}
	})

	t.Run("an edit with an empty old side also sends no oldText", func(t *testing.T) {
		// The ONE place this refactor is not bit-identical to what came before, and it
		// is deliberate: the old code passed args.OldString unconditionally, so an edit
		// naming an empty old side put `"oldText": ""` on the wire. Such an edit is
		// unreachable twice over — the tool rejects an empty old_string (it matches
		// nothing), and only confirmation-requiring tools reach the permission preview —
		// and "" is the wrong encoding anyway: the field means "the content that was
		// there", and there was none. One rule (empty old text ⇒ no oldText) beats a
		// distinction that exists only for a call that always fails.
		got := buildDiffFromArgs(tools.ToolNameEdit, `{"path":"/a.go","old_string":"","new_string":"added\n"}`)
		if got == nil || got.Diff == nil {
			t.Fatal("expected a diff block")
		}
		if got.Diff.OldText != nil {
			t.Errorf("OldText = %q, want nil", *got.Diff.OldText)
		}
	})
}

// TestPermissionPreviewSharesTheDerivation: the permission dialog and the tool-call
// stream must show the same thing for the same call. They do by construction — both
// go through tools.FileChangeForTool → fileChangeContent — so this pins the shared
// half: the neutral change the permission path feeds in is what the stream sends.
func TestPermissionPreviewSharesTheDerivation(t *testing.T) {
	args := `{"path":"/a.go","old_string":"old\n","new_string":"new\n"}`

	fromStream, err := json.Marshal(buildDiffFromArgs(tools.ToolNameEdit, args))
	if err != nil {
		t.Fatal(err)
	}
	fc, ok := tools.FileChangeForTool(tools.ToolNameEdit, args) // what permission.go feeds in
	if !ok {
		t.Fatal("expected a change for an edit")
	}
	fromPermission, err := json.Marshal(fileChangeContent(fc))
	if err != nil {
		t.Fatal(err)
	}
	if string(fromStream) != string(fromPermission) {
		t.Errorf("the two ACP producers disagree:\n stream     %s\n permission %s", fromStream, fromPermission)
	}
}
