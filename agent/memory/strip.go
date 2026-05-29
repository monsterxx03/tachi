package memory

import "strings"

// noiseTags defines XML-like block tags that should be stripped from messages
// before storage. These are system-injected blocks (e.g. <system-reminder>)
// prepended to user messages that are not meaningful for memory recall.
var noiseTags = []string{
	"<local-command-caveat>",
	"<local-command-stdout>",
	"<command-name>",
	"<command-message>",
	"<task-notification>",
	"<system-reminder>",
	"<available-skills>",
	"<available-deferred-tools>",
	"<relevant-memories>",
}

// stripNoiseTags removes noise block tags and their content from s.
// Each tag is expected to appear as a paired block (<tag>...</tag>).
// The closing tag is derived by inserting "/" after the leading "<".
func stripNoiseTags(s string) string {
	for _, tag := range noiseTags {
		endTag := tag[:1] + "/" + tag[1:]
		for {
			start := strings.Index(s, tag)
			if start == -1 {
				break
			}
			end := strings.Index(s[start:], endTag)
			if end == -1 {
				// Unmatched opening tag, remove from start to end
				s = strings.TrimSpace(s[:start])
				break
			}
			s = strings.TrimSpace(s[:start] + s[start+end+len(endTag):])
			// Collapse consecutive newlines from block removal
			for strings.Contains(s, "\n\n") {
				s = strings.ReplaceAll(s, "\n\n", "\n")
			}
		}
	}
	return s
}
