package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// A session a SUBSYSTEM drives is listed with a marking and cannot be switched into (design §2.1).
//
// The ORDER of the refusal is the load-bearing part, and it is why this is a test rather than a
// comment: the check runs BEFORE the reader's own session is ended. Moving it below
// `sm.EndCurrent()` would leave a reader who clicked such a row sitting in no session at all —
// the same refusal, with the reader's conversation dropped for nothing.

// managedSessionPair seeds two conversations in one store: the reader's own, and one a subsystem
// owns (its record's Kind is what says so). The reader's own is left current.
func managedSessionPair(t *testing.T) (*session.Manager, *session.Session, *session.Session) {
	t.Helper()
	dir := t.TempDir()
	store, err := session.NewFileStore(dir)
	require.NoError(t, err)
	mgr := session.NewManagerWithStore(store, logger.New("tui-test"))

	_, err = mgr.New("test-provider", dir)
	require.NoError(t, err)
	own := mgr.Current()
	require.NotNil(t, own)

	managed, err := mgr.New("test-provider", dir)
	require.NoError(t, err)
	managed.Kind = session.KindA2AExecution
	require.NoError(t, mgr.UpdateMeta(managed))

	// `New` made the managed one current; the state under test is the reader back in their own.
	_, err = mgr.Load(own.ID)
	require.NoError(t, err)
	return mgr, own, managed
}

func TestSwitchingIntoAManagedSessionIsRefusedAndEndsNothing(t *testing.T) {
	mgr, own, managed := managedSessionPair(t)

	provider, err := llm.NewProvider("openai", "sk", "", "deepseek-v4-flash")
	require.NoError(t, err)
	m := testModel()
	m.agent = newTestAIAgent(t, provider, 10)
	defer m.agent.Close()
	m.agent.SetSessionManager(mgr)
	m.sessionList = []*session.Session{own, managed}

	// The overlay is open on the managed row and the reader presses Enter.
	m.loadSession(1)

	got := mgr.Current()
	require.NotNil(t, got, "a refused switch must leave the reader somewhere")
	require.Equal(t, own.ID, got.ID,
		"the reader's own session must not be ended by a switch that was refused")
	require.Nil(t, m.sessionList, "the refusal closes the overlay like any other exit")
	require.Equal(t, stateIdle, m.state)
}

// The marking appears where a reader meets the session, and it is the SAME wording the web console
// and the desktop use — one fact, one sentence, however many frontends show it.
func TestAManagedSessionIsMarkedWhereTheReaderMeetsIt(t *testing.T) {
	require.Empty(t, managedRowSuffix(""), "an ordinary conversation carries no marking")

	suffix := managedRowSuffix(session.KindA2AExecution)
	for _, want := range []string{"子系统执行记录", "只读", session.KindA2AExecution} {
		require.Contains(t, suffix, want)
	}

	// The @-picker row: the title a remote prompt generated must not be the only thing a reader
	// sees, because that title reads as their own conversation.
	row := sessionRowLabel(atfile.SessionTarget{
		Title:       "检查昨天的备份",
		UpdatedAt:   time.Now(),
		ManagedKind: session.KindA2AExecution,
	})
	require.True(t, strings.Contains(row, "只读") && strings.Contains(row, session.KindA2AExecution),
		"the picker row has to carry the marking too, got %q", row)

	// And a plain conversation's row is untouched.
	plain := sessionRowLabel(atfile.SessionTarget{Title: "我自己的对话", UpdatedAt: time.Now()})
	require.NotContains(t, plain, "只读")
	require.NotContains(t, plain, "execution")

}
