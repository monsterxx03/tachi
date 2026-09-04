package manager

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDrainEvents_SegmentBreakBetweenToolRounds verifies that consecutive LLM
// iterations (text → tool call → text) are separated by a newline when the
// previous round's text doesn't end in one, so the reply doesn't fuse into a
// run-on line ("让我先搜索一下搜索结果是…").
func TestDrainEvents_SegmentBreakBetweenToolRounds(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, nil, "drain-seg-thread")

	ch := make(chan agent.AgentEvent, 16)
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "让我先搜索一下"}
	ch <- agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolName: "web_search", ToolID: "t1"}
	ch <- agent.AgentEvent{Type: agent.AgentEventToolCallArgs, ToolName: "web_search", ToolID: "t1", ToolArgs: `{"q":"x"}`}
	ch <- agent.AgentEvent{Type: agent.AgentEventToolResult, ToolName: "web_search", ToolResult: "ok"}
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "搜索结果是：x"}
	close(ch)

	text, err := mgr.drainEvents(t.Context(), ch, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "让我先搜索一下\n搜索结果是：x", text)
}

// TestDrainEvents_NoDoubleBreakWhenEndsWithNewline verifies a round already
// ending in a newline doesn't get an extra one inserted.
func TestDrainEvents_NoDoubleBreakWhenEndsWithNewline(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, nil, "drain-nl-thread")

	ch := make(chan agent.AgentEvent, 16)
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "让我先搜索一下\n"}
	ch <- agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolName: "web_search", ToolID: "t1"}
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "搜索结果是：x"}
	close(ch)

	text, err := mgr.drainEvents(t.Context(), ch, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "让我先搜索一下\n搜索结果是：x", text)
}

// TestDrainEvents_NoLeadingBreakWhenToolCalledFirst verifies a tool call made
// before any text (LLM went straight to tools) doesn't leave a stray leading
// newline in the reply.
func TestDrainEvents_NoLeadingBreakWhenToolCalledFirst(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, nil, "drain-first-tool-thread")

	ch := make(chan agent.AgentEvent, 16)
	ch <- agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolName: "web_search", ToolID: "t1"}
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "结果是 x"}
	close(ch)

	text, err := mgr.drainEvents(t.Context(), ch, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "结果是 x", text)
}

// TestDrainEvents_SegmentBreakAcrossMultipleRounds verifies every tool round
// boundary gets a newline.
func TestDrainEvents_SegmentBreakAcrossMultipleRounds(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, nil, "drain-multi-thread")

	ch := make(chan agent.AgentEvent, 16)
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "第一轮"}
	ch <- agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolName: "t1", ToolID: "t1"}
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "第二轮"}
	ch <- agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolName: "t2", ToolID: "t2"}
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "第三轮"}
	close(ch)

	text, err := mgr.drainEvents(t.Context(), ch, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "第一轮\n第二轮\n第三轮", text)
}

// TestDrainEvents_SegmentBreakOnSteerBoundary verifies the steer boundary
// also breaks the segment (ta == nil → steer is skipped, but the boundary
// still applies).
func TestDrainEvents_SegmentBreakOnSteerBoundary(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, nil, "drain-steer-thread")

	ch := make(chan agent.AgentEvent, 16)
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "前半"}
	ch <- agent.AgentEvent{Type: agent.AgentEventSteerCheck}
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "后半"}
	close(ch)

	text, err := mgr.drainEvents(t.Context(), ch, nil, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "前半\n后半", text)
}

// TestSteer_ActiveTurnQueuesAttachments verifies that a directed message
// arriving while an agent turn is active keeps its attachments in the steer
// queue. Regression for mid-turn screenshots: they used to be reduced to
// msg.Content (a bare "[图片]" placeholder) with the attachment silently
// dropped at enqueue time.
func TestSteer_ActiveTurnQueuesAttachments(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := mustNewManager(t, cfg)
	mgr.sessionStore = newTempSessionStore(t)

	// Manually set up a thread with an active turn.
	ta := &threadActivation{
		steerRespCh: make(chan agent.SteerInput, 1),
		resultCh:    make(chan handlerResult, 1),
	}
	ta.ctx, ta.cancel = t.Context(), func() {}
	mgr.threadActivations.Store("test:steer-img-attach", ta)

	imgBytes := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	handler := mgr.buildHandler()

	result := handler(t.Context(), channel.IncomingMessage{
		ThreadID:  "test:steer-img-attach",
		MessageID: "msg-img-1",
		Content:   "[图片]",
		Sender:    "张三",
		Directed:  true,
		Attachments: []channel.Attachment{{
			Type:     channel.AttachmentTypeImage,
			FileName: "shot.png",
			MimeType: "image/png",
			Content:  imgBytes,
			Size:     int64(len(imgBytes)),
		}},
	})

	assert.True(t, result.Steered)

	ta.mu.Lock()
	defer ta.mu.Unlock()
	require.Len(t, ta.pending, 1)
	assert.Equal(t, "[图片]", ta.pending[0].content)
	require.Len(t, ta.pending[0].attachments, 1)
	assert.Equal(t, imgBytes, ta.pending[0].attachments[0].Content)
}

// TestDrainEvents_SteerInjectCarriesImageParts verifies that steer injection
// rebuilds image ContentParts from a queued message's attachments, so a
// mid-turn screenshot reaches the vision pipeline (and the vision-fallback
// describer) instead of arriving as bare placeholder text.
func TestDrainEvents_SteerInjectCarriesImageParts(t *testing.T) {
	mgr, _ := newOneoffTestManager(t, nil, "drain-steer-img")

	steerCh := make(chan agent.SteerInput, 1)
	imgBytes := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	ta := &threadActivation{
		steerRespCh: steerCh,
		resultCh:    make(chan handlerResult, 1),
		pending: []pendingSteerMsg{{
			content: "[图片]",
			attachments: []channel.Attachment{{
				Type:     channel.AttachmentTypeImage,
				FileName: "shot.png",
				MimeType: "image/png",
				Content:  imgBytes,
				Size:     int64(len(imgBytes)),
			}},
		}},
	}
	ta.ctx, ta.cancel = t.Context(), func() {}

	ch := make(chan agent.AgentEvent, 8)
	ch <- agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "前半"}
	ch <- agent.AgentEvent{Type: agent.AgentEventSteerCheck}
	close(ch)

	text, err := mgr.drainEvents(t.Context(), ch, nil, nil, ta, nil)
	require.NoError(t, err)
	assert.Equal(t, "前半", text)

	select {
	case si := <-steerCh:
		// Text carries the image placeholder…
		assert.Contains(t, si.Text, "[图片: shot.png")
		// …and the actual pixels ride along as a multi-modal content part.
		require.Len(t, si.Images, 1)
		assert.Equal(t, llm.ContentPartImage, si.Images[0].Type)
		assert.Equal(t, base64.StdEncoding.EncodeToString(imgBytes), si.Images[0].Data)
	case <-time.After(2 * time.Second):
		t.Fatal("steer input not delivered")
	}
}
