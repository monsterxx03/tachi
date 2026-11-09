package node

import (
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/tasks"
	"github.com/monsterxx03/tachi/agent/tools"
)

// A settled remote task becomes the same kind of event a local one does: the
// handle the model already has (the local delegation id), the machine it ran on,
// and the text it produced.
func TestDelegationEventCompleted(t *testing.T) {
	d := &Delegation{
		ID: "d_1a2b3c4d", LogicalID: "conv", Title: "检查备份",
		LastKnown: lastKnown{State: a2a.TaskStateCompleted, Text: "两个目录异常"},
	}
	view := tools.RemoteTaskInfo{ID: d.ID, Node: "hostkvm", Title: d.Title, State: "completed", Result: "两个目录异常"}

	e := delegationEvent(d, view)
	require.Equal(t, tasks.KindRemote, e.Kind)
	require.Equal(t, d.ID, e.Name)
	require.Equal(t, "hostkvm", e.Node)
	require.Equal(t, "检查备份", e.Command)
	require.Equal(t, tasks.StatusExited, e.Status)
	require.Equal(t, "两个目录异常", e.Stdout)
}

func TestDelegationEventFailed(t *testing.T) {
	d := &Delegation{
		ID: "d_dead", LogicalID: "conv", Title: "检查备份",
		LastKnown: lastKnown{State: a2a.TaskStateFailed, Text: "磁盘满了"},
	}
	view := tools.RemoteTaskInfo{ID: d.ID, Node: "nas", State: "failed", Result: "磁盘满了"}

	e := delegationEvent(d, view)
	require.Equal(t, tasks.StatusError, e.Status)
	require.Equal(t, "failed", e.Error)
	require.Equal(t, "磁盘满了", e.Stdout)
}

// Waiting is not an ending, but it is the one state an idle conversation MUST
// hear about: nobody else can answer it, and the answer has to name the question.
func TestDelegationEventWaiting(t *testing.T) {
	d := &Delegation{
		ID: "d_ask", LogicalID: "conv", Title: "迁移数据", QuestionID: "q_7",
		LastKnown: lastKnown{State: a2a.TaskStateInputRequired, Text: "哪个目录？"},
	}
	view := tools.RemoteTaskInfo{
		ID: d.ID, Node: "nas", State: "input_required",
		QuestionID: "q_7", QuestionText: "哪个目录？",
	}

	e := delegationEvent(d, view)
	require.Equal(t, tasks.StatusWaiting, e.Status)
	require.Equal(t, "q_7", e.QuestionID)
	require.Equal(t, "哪个目录？", e.Stdout)
}

// auth_required also parks the task on this side; there is no question id, but
// the text says what is needed.
func TestDelegationEventAuthRequired(t *testing.T) {
	d := &Delegation{
		ID: "d_auth", LogicalID: "conv",
		LastKnown: lastKnown{State: a2a.TaskStateAuthRequired, Text: "请先配置访问令牌"},
	}
	view := tools.RemoteTaskInfo{ID: d.ID, State: "auth_required", Result: "请先配置访问令牌"}

	e := delegationEvent(d, view)
	require.Equal(t, tasks.StatusWaiting, e.Status)
	require.Empty(t, e.QuestionID)
	require.Equal(t, "请先配置访问令牌", e.Stdout)
}
