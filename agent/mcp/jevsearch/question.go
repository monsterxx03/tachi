// Package jevsearch holds the QUESTION the MCP tool-search gate asks, and
// nothing else — no pool, no client, no threshold.
//
// It is a package of its own because that text is shared by two callers that
// must never disagree: the gate (`agent/mcp/jev_search.go`) that sends it, and
// the measurement (`pkg/jev/tool_search_cases_test.go`) that swept the
// probability cut-off against it. A threshold belongs to the prompt it was
// swept on (docs/agents/jev.md), so the two build the same bytes from the same
// function instead of the same wording maintained twice — and the generator
// that writes the case file calls in here, so a wording change cannot leave the
// measurement describing a question the gate no longer asks.
package jevsearch

import (
	"fmt"

	"github.com/monsterxx03/tachi/pkg/jev"
)

// Candidate is one tool as the question needs it: the fields a deferred tool
// already caches, flattened so this package depends on no other type.
type Candidate struct {
	Name    string // "mcp__postgres__query"
	Server  string // "postgres"
	Hint    string // SearchHint: server + name parts + description keywords
	Summary string // the description's first line
}

const (
	// Frame is the shape of the question with the candidate left out. The gate
	// never sends it — every question carries its own tool — but the case file
	// needs a file-level question, and this is what that file means.
	Frame = "The state is a request from the user. Decide whether the tool described below can carry that request out directly."

	// questionFmt is the per-candidate text: which tool, where it lives, the
	// keywords SearchHint carries, and the first line of its description —
	// then the question itself. English, like every tool description the model
	// sees; the STATE is whatever language the model wrote its query in.
	questionFmt = "Tool %s (server: %s; related terms: %s): %s\nCan this tool carry the request out directly?"

	// Yes and No are the noul criteria. Supplying them is what makes the
	// probability interpretable — without them the model has to guess what
	// "yes" covers, and the guess leans toward the label it is about to pick.
	// They are deliberately separate from the tool text: the criteria are the
	// same judgment for every candidate, which is what makes one threshold
	// meaningful across a whole pool.
	Yes = "Calling this tool now would directly carry out what the request asks for."
	No  = "The request asks for something else, or this tool needs input the request does not provide, or its action is only RELATED to what was asked rather than the same action."
)

// Instructions is the text one candidate's question carries.
func Instructions(c Candidate) string {
	return fmt.Sprintf(questionFmt, c.Name, c.Server, c.Hint, c.Summary)
}

// Question is the noul question for one candidate ("can this tool carry the
// request out directly?"), keyed by the caller with the tool's own name so the
// answer maps back to it.
func Question(c Candidate) jev.Question {
	return jev.NewNoul(Instructions(c), jev.NoulCriteria{Yes: Yes, No: No})
}
