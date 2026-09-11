package tools

import "testing"

func TestFileChangeForTool(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		args     string
		want     FileChange
		wantOK   bool
		wantText string // for failure messages
	}{
		{
			name: "edit carries both fragments",
			tool: ToolNameEdit,
			args: `{"path":"/repo/a.go","old_string":"old\n","new_string":"new\n"}`,
			want: FileChange{Path: "/repo/a.go", OldText: "old\n", NewText: "new\n"},
			wantOK: true,
		},
		{
			name: "replace_all is recorded for the UI",
			tool: ToolNameEdit,
			args: `{"path":"/repo/a.go","old_string":"x","new_string":"y","replace_all":true}`,
			want: FileChange{Path: "/repo/a.go", OldText: "x", NewText: "y", ReplaceAll: true},
			wantOK: true,
		},
		{
			// An insertion: the old side is empty on purpose, and that is still a change.
			name: "edit that only inserts",
			tool: ToolNameEdit,
			args: `{"path":"/repo/a.go","old_string":"","new_string":"added\n"}`,
			want: FileChange{Path: "/repo/a.go", NewText: "added\n"},
			wantOK: true,
		},
		{
			name: "write is a create with no old text",
			tool: ToolNameWrite,
			args: `{"path":"/repo/new.go","content":"package main\n"}`,
			want: FileChange{Path: "/repo/new.go", NewText: "package main\n"},
			wantOK: true,
		},
		{
			name:   "write without content has nothing to show",
			tool:   ToolNameWrite,
			args:   `{"path":"/repo/empty.go","content":""}`,
			wantOK: false,
		},
		{
			name:   "edit that says nothing about either side is not a change",
			tool:   ToolNameEdit,
			args:   `{"path":"/repo/a.go","old_string":"","new_string":""}`,
			wantOK: false,
		},
		{
			name:   "path is required",
			tool:   ToolNameEdit,
			args:   `{"old_string":"a","new_string":"b"}`,
			wantOK: false,
		},
		{
			name:   "non-file tools do not change text",
			tool:   ToolNameRead,
			args:   `{"path":"/repo/a.go"}`,
			wantOK: false,
		},
		{
			name:   "an unknown tool is not a change",
			tool:   "SomethingElse",
			args:   `{"path":"/repo/a.go","content":"x"}`,
			wantOK: false,
		},
		{
			name:   "empty args",
			tool:   ToolNameEdit,
			args:   "",
			wantOK: false,
		},
		{
			name:   "unparsable args",
			tool:   ToolNameEdit,
			args:   `{"path":`,
			wantOK: false,
		},
		{
			name:   "args of the wrong shape",
			tool:   ToolNameWrite,
			args:   `{"path":"/repo/a.go","content":{"nested":true}}`,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := FileChangeForTool(tt.tool, tt.args)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (%+v)", ok, tt.wantOK, got)
			}
			if !tt.wantOK {
				return
			}
			if got != tt.want {
				t.Errorf("FileChangeForTool() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
