package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/session"
)

// bigResult is a tool result comfortably over the trim threshold, and the ids are
// named so a failure says which record moved.
func bigResult(id, name string) session.Message {
	return session.Message{
		Type:       session.MessageTypeToolResult,
		Name:       name,
		ToolCallID: id,
		Result:     strings.Repeat("x", trimMinResultChars+500),
	}
}

// msgPtr is AppendMessage's shape; the fixtures above stay values because the planner
// takes a slice of them.
func msgPtr(m session.Message) *session.Message { return &m }

// noImageBytes is the sizing function for conversations without images: every part
// costs nothing, which is what the planner did for all records before image bytes were
// counted at all.
func noImageBytes(session.Part) int64 { return 0 }

func smallResult(id, name string) session.Message {
	return session.Message{
		Type:       session.MessageTypeToolResult,
		Name:       name,
		ToolCallID: id,
		Result:     "ok",
	}
}

// The window is a POSITION in the conversation, not a count of eligible results: a
// small result inside the newest N must not let an hour-old big one through.
func TestPlanToolResultTrimKeepsTheNewestPositionalResults(t *testing.T) {
	msgs := []session.Message{bigResult("old", tools.ToolNameBash)}
	for i := 0; i < trimKeepRecentResults; i++ {
		msgs = append(msgs, smallResult(fmt.Sprintf("recent-%d", i), tools.ToolNameBash))
	}

	plan := planToolResultTrim(msgs, noImageBytes)

	assert.Contains(t, plan.replacements, "old", "everything behind the window is eligible")
	for i := 0; i < trimKeepRecentResults; i++ {
		assert.NotContains(t, plan.replacements, fmt.Sprintf("recent-%d", i),
			"the newest results stay, whatever their size")
	}
}

// AskUserQuestion's "output" is a decision the model cannot ask for again, so it is
// kept at any size.
func TestPlanToolResultTrimNeverTouchesAPersonsAnswer(t *testing.T) {
	msgs := []session.Message{bigResult("answer", tools.ToolNameAskUser)}
	for i := 0; i < trimKeepRecentResults; i++ {
		msgs = append(msgs, smallResult(fmt.Sprintf("recent-%d", i), tools.ToolNameBash))
	}

	plan := planToolResultTrim(msgs, noImageBytes)
	assert.NotContains(t, plan.replacements, "answer")
	assert.Zero(t, plan.charsSaved)
}

func TestPlanToolResultTrimSkipsSmallResultsAndDisplayOnlyRecords(t *testing.T) {
	msgs := []session.Message{
		smallResult("small", tools.ToolNameBash),
		{Type: session.MessageTypeToolResult, Name: tools.ToolNameBash, ToolCallID: "sh", Result: strings.Repeat("y", 9000), AutoExpand: true},
	}
	for i := 0; i < trimKeepRecentResults; i++ {
		msgs = append(msgs, smallResult(fmt.Sprintf("recent-%d", i), tools.ToolNameBash))
	}

	plan := planToolResultTrim(msgs, noImageBytes)
	assert.Empty(t, plan.replacements)
}

// An image is worth removing at any text length: the tokens are in the parts.
func TestPlanToolResultTrimTakesImagesRegardlessOfTextSize(t *testing.T) {
	msgs := []session.Message{
		{
			Type: session.MessageTypeToolResult, Name: tools.ToolNameRead, ToolCallID: "img",
			Result: "[Image: shot.png]",
			Parts:  []session.Part{{Type: session.PartTypeImage, Ref: "sha"}},
		},
	}
	for i := 0; i < trimKeepRecentResults; i++ {
		msgs = append(msgs, smallResult(fmt.Sprintf("recent-%d", i), tools.ToolNameBash))
	}

	plan := planToolResultTrim(msgs, noImageBytes)
	replacement := plan.replacements["img"]
	require.NotEmpty(t, replacement)
	assert.Contains(t, replacement, "图片")
}

// End to end: the records on disk are what the next request is rebuilt from, so the
// trim has to change those — and the history handed back must be what a reload
// produces.
func TestTrimToolResultsRewritesTheSessionAndReturnsItsNewHistory(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)
	_, err = sm.New("anthropic", t.TempDir())
	require.NoError(t, err)
	require.NoError(t, sm.AppendMessage(&session.Message{Type: session.MessageTypeUser, Content: "读一下这几个文件"}))

	ids := make([]string, 0, trimKeepRecentResults+2)
	for i := 0; i < trimKeepRecentResults+2; i++ {
		id := fmt.Sprintf("call-%d", i)
		ids = append(ids, id)
		require.NoError(t, sm.AppendMessage(&session.Message{
			Type: session.MessageTypeToolCall, Name: tools.ToolNameBash,
			ToolCallID: id, Args: map[string]string{"command": "ls"},
		}))
		require.NoError(t, sm.AppendMessage(&session.Message{
			Type: session.MessageTypeToolResult, Name: tools.ToolNameBash,
			ToolCallID: id, Result: strings.Repeat("z", trimMinResultChars+100),
		}))
	}
	// A person's answer, far enough back that only the keep-list saves it.
	require.NoError(t, sm.AppendMessage(msgPtr(bigResult("answer", tools.ToolNameAskUser))))
	for i := 0; i < trimKeepRecentResults; i++ {
		require.NoError(t, sm.AppendMessage(msgPtr(smallResult(fmt.Sprintf("tail-%d", i), tools.ToolNameBash))))
	}

	a := newBareTestAgent(t, &mockStreamProvider{name: "anthropic"}, 1)
	a.Config.SessionManager = sm

	report, err := a.TrimToolResults(context.Background())
	require.NoError(t, err)
	assert.Empty(t, report.Blocked)
	// Ten big results behind the window; the eight tail results are the window, and the
	// person's answer is behind it but on the keep-list.
	assert.Equal(t, trimKeepRecentResults+2, report.Trimmed)
	assert.Positive(t, report.CharsSaved)
	require.NotEmpty(t, report.Sidecar)

	// The history is rebuilt from the records, so it carries the placeholders.
	var trimmedInHistory, answerInHistory bool
	for _, m := range report.History {
		if m.Role != "tool" {
			continue
		}
		if m.ToolCallID == ids[0] {
			trimmedInHistory = true
			assert.Contains(t, m.Content, "已裁剪")
		}
		if m.ToolCallID == "answer" {
			answerInHistory = true
			assert.Equal(t, bigResult("answer", tools.ToolNameAskUser).Result, m.Content)
		}
	}
	assert.True(t, trimmedInHistory, "the oldest result must reach the history as a placeholder")
	assert.True(t, answerInHistory, "the person's answer must survive as it was")

	// And the file itself no longer holds the removed text.
	raw, err := os.ReadFile(report.Sidecar)
	require.NoError(t, err)
	assert.Contains(t, string(raw), strings.Repeat("z", trimMinResultChars+100), "the original is kept whole")
}

// A second trim has nothing left to do — the same run twice must not invent work.
func TestTrimToolResultsTwiceIsIdempotent(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)
	_, err = sm.New("anthropic", t.TempDir())
	require.NoError(t, err)
	require.NoError(t, sm.AppendMessage(msgPtr(bigResult("old", tools.ToolNameBash))))
	for i := 0; i < trimKeepRecentResults; i++ {
		require.NoError(t, sm.AppendMessage(msgPtr(smallResult(fmt.Sprintf("tail-%d", i), tools.ToolNameBash))))
	}

	a := newBareTestAgent(t, &mockStreamProvider{name: "anthropic"}, 1)
	a.Config.SessionManager = sm

	first, err := a.TrimToolResults(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, first.Trimmed)

	second, err := a.TrimToolResults(context.Background())
	require.NoError(t, err)
	assert.Zero(t, second.Trimmed)
	assert.Nil(t, second.History)
}

func TestTrimToolResultsRefusesAManagedSession(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	sm := session.NewManagerWithStore(store, nil)
	cur, err := sm.New("anthropic", t.TempDir())
	require.NoError(t, err)
	cur.Kind = session.KindA2AExecution
	require.NoError(t, sm.UpdateMeta(cur))

	a := newBareTestAgent(t, &mockStreamProvider{name: "anthropic"}, 1)
	a.Config.SessionManager = sm

	report, err := a.TrimToolResults(context.Background())
	require.NoError(t, err)
	assert.NotEmpty(t, report.Blocked)
	assert.Zero(t, report.Trimmed)
}

// imageResult is a tool result that carried one image blob; its size comes from the
// byRef map, the way a real record's does.
func imageResult(id, ref string) session.Message {
	parts := []session.Part{{Type: session.PartTypeImage, Ref: ref, MediaType: "image/png"}}
	return session.Message{
		Type:       session.MessageTypeToolResult,
		Name:       tools.ToolNameRead,
		ToolCallID: id,
		Result:     "[Image: shot.png]",
		Parts:      parts,
	}
}

// byRef sizes blobs from the ref, which is where the real store keeps the bytes.
func byRef(sizes map[string]int64) func(session.Part) int64 {
	return func(p session.Part) int64 { return sizes[p.Ref] }
}

// A screenshot's note is one short line: measured in characters it looks like nothing,
// which is how a trim could report 121 and leave a 16MB request behind.
func TestTrimCountsImageBytesNotJustText(t *testing.T) {
	msgs := []session.Message{imageResult("img", "old")}
	sizes := map[string]int64{"old": 4 << 20}
	for i := 0; i < trimKeepRecentResults; i++ {
		msgs = append(msgs, smallResult(fmt.Sprintf("recent-%d", i), tools.ToolNameBash))
	}

	plan := planToolResultTrim(msgs, byRef(sizes))

	require.Contains(t, plan.replacements, "img")
	assert.Equal(t, int64(4<<20), plan.bytesSaved, "the 4MB blob is what the trim freed, not its one-line note")
	assert.Contains(t, plan.replacements["img"], "1 张图片")
}

// Images are trimmed on their own byte budget, which reaches into the recent-results
// window: a run of screenshots has nothing older to remove and is still too big.
func TestTrimImageBudgetReachesIntoTheRecentWindow(t *testing.T) {
	msgs := []session.Message{
		imageResult("oldest", "a"),
		imageResult("middle", "b"),
		imageResult("newest", "c"),
	}
	sizes := map[string]int64{"a": 3 << 20, "b": 3 << 20, "c": 3 << 20}

	plan := planToolResultTrim(msgs, byRef(sizes))

	assert.NotContains(t, plan.replacements, "newest", "the newest picture is the one the model may still be looking at")
	assert.Contains(t, plan.replacements, "oldest")
	assert.Contains(t, plan.replacements, "middle")
	assert.Equal(t, int64(6<<20), plan.bytesSaved)
}

// Images inside the budget are left where they are, however recent or old: going
// through a picture the model already saw costs a re-read for no gain.
func TestTrimLeavesImagesInsideTheBudgetAlone(t *testing.T) {
	msgs := []session.Message{
		imageResult("ancient", "a"),
		imageResult("older", "b"),
		imageResult("newest", "c"),
	}
	sizes := map[string]int64{"a": 100 << 10, "b": 100 << 10, "c": 100 << 10}

	plan := planToolResultTrim(msgs, byRef(sizes))
	assert.Empty(t, plan.replacements)
}
