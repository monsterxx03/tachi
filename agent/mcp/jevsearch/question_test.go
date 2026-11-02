package jevsearch

// Why this file exists: the threshold in docs/agents/jev.md is a number swept against the exact text
// this package produces. Nothing fails when that text changes — the measurement builds its cases by
// calling Instructions too (pkg/jev/tool_search_cases_test.go), which is what keeps the two sides
// consistent, and what makes a drifted question silent: the sweep would keep reporting a cut-off for
// a question the gate no longer asks. So the shape is pinned HERE, against literal bytes, and that
// is the only place a reword has to be looked at on purpose.

import (
	"testing"

	"github.com/monsterxx03/tachi/pkg/jev"
)

// The whole text one candidate produces, byte for byte: the four fields in this order, one line of
// framing, then the question itself.
func TestInstructionsShape(t *testing.T) {
	got := Instructions(Candidate{
		Name:    "mcp__postgres__query",
		Server:  "postgres",
		Hint:    "query, sql, read",
		Summary: "Run a read-only SQL query against the configured database.",
	})
	want := "Tool mcp__postgres__query (server: postgres; related terms: query, sql, read): " +
		"Run a read-only SQL query against the configured database.\n" +
		"Can this tool carry the request out directly?"
	if got != want {
		t.Fatalf("Instructions =\n%q\nwant\n%q", got, want)
	}
}

// An empty hint or summary leaves its field VISIBLE rather than collapsing the sentence: every
// candidate has to reach the model in the same shape, because the threshold was swept against that
// shape. A "nicer" text for the empty case would be a different question for those tools only.
func TestInstructionsKeepsEmptyFields(t *testing.T) {
	got := Instructions(Candidate{Name: "mcp__x__y", Server: "x"})
	want := "Tool mcp__x__y (server: x; related terms: ): \nCan this tool carry the request out directly?"
	if got != want {
		t.Fatalf("Instructions with an empty hint and summary =\n%q\nwant\n%q", got, want)
	}
}

// Question is the gate's question: the same instructions, plus the yes/no rubric that makes the
// returned probability interpretable. Both halves come from this package's constants — a call site
// that composed its own criteria would be asking a different question under the same threshold.
func TestQuestionCarriesTheGateCriteria(t *testing.T) {
	c := Candidate{Name: "mcp__postgres__query", Server: "postgres", Hint: "sql", Summary: "Run SQL."}
	q := Question(c)

	if want := Instructions(c); q.Instructions != want {
		t.Errorf("Question.Instructions = %q, want Instructions(c) = %q", q.Instructions, want)
	}
	noul, ok := q.Criteria.(jev.NoulCriteria)
	if !ok {
		t.Fatalf("Criteria is %T, want a noul rubric (yes/no) — this is a probability gate", q.Criteria)
	}
	if noul.Yes != Yes {
		t.Errorf("criteria.yes = %q, want the package's Yes", noul.Yes)
	}
	if noul.No != No {
		t.Errorf("criteria.no = %q, want the package's No", noul.No)
	}
}
