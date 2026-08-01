package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestInputThinkingCompletions verifies the /thinking option completion: when
// the input is the /thinking command (with an optional partial level), the
// completion list includes the valid level values.
func TestInputThinkingCompletions(t *testing.T) {
	cases := []struct {
		input    string
		wantCmds []string
	}{
		{
			input:    "/thinking",
			wantCmds: []string{"/thinking", "/thinking default", "/thinking high", "/thinking low", "/thinking max", "/thinking medium", "/thinking none", "/thinking xhigh"},
		},
		{input: "/thinking h", wantCmds: []string{"/thinking high"}},
		{input: "/thinking m", wantCmds: []string{"/thinking max", "/thinking medium"}},
		{input: "/thinking n", wantCmds: []string{"/thinking none"}},
		{input: "/thinking x", wantCmds: []string{"/thinking xhigh"}},
		{input: "/thinking bogus", wantCmds: nil},
		// Partial command name: only command-name completion, no levels yet.
		{input: "/think", wantCmds: []string{"/thinking"}},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			i := NewInputArea(10, "", nil)
			i.textarea.SetValue(tc.input)
			i.updateCompletions()

			var got []string
			for _, c := range i.completions {
				got = append(got, c.Name)
			}
			assert.ElementsMatch(t, tc.wantCmds, got)
		})
	}
}
