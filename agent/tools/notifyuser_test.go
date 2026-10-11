package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubNotifySink records what the tool asked it to deliver.
type stubNotifySink struct {
	channels []string
	err      error
	got      []string
}

func (s *stubNotifySink) Channels() []string { return s.channels }

func (s *stubNotifySink) Notify(_ context.Context, text string) (string, error) {
	s.got = append(s.got, text)
	if s.err != nil {
		return "", s.err
	}
	if len(s.channels) == 0 {
		return "", errors.New("no notification channel is configured")
	}
	return s.channels[0], nil
}

func TestNotifyUserSuccessNamesTheChannel(t *testing.T) {
	sink := &stubNotifySink{channels: []string{"weixin"}}
	out, err := NewNotifyUserTool(sink).ExecuteContext(context.Background(), `{"message":"任务跑完了"}`)
	if err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}
	if !strings.Contains(out, "weixin") {
		t.Errorf("result %q must name the channel that carried it", out)
	}
	if len(sink.got) != 1 || sink.got[0] != "任务跑完了" {
		t.Fatalf("sink received %v", sink.got)
	}
}

func TestNotifyUserFailureIsAFailedCall(t *testing.T) {
	sink := &stubNotifySink{
		channels: []string{"weixin", "discord"},
		err:      errors.New("weixin: no stored credentials; discord: user is not in a shared server"),
	}
	out, err := NewNotifyUserTool(sink).ExecuteContext(context.Background(), `{"message":"hi"}`)
	if err == nil {
		t.Fatalf("a delivery failure must be a failed call, got success %q", out)
	}
	if !strings.Contains(err.Error(), "not in a shared server") {
		t.Errorf("error %q must carry the reason", err.Error())
	}
}

func TestNotifyUserRequiresMessage(t *testing.T) {
	sink := &stubNotifySink{channels: []string{"weixin"}}
	tool := NewNotifyUserTool(sink)

	for _, args := range []string{`{}`, `{"message":""}`, `{"message":"   "}`, `{not json`} {
		if _, err := tool.ExecuteContext(context.Background(), args); err == nil {
			t.Errorf("args %q must be refused", args)
		}
	}
	if len(sink.got) != 0 {
		t.Fatalf("nothing should have been delivered, got %v", sink.got)
	}
}

func TestNotifyUserWithoutASinkFailsLoudly(t *testing.T) {
	out, err := NewNotifyUserTool(nil).ExecuteContext(context.Background(), `{"message":"hi"}`)
	if err == nil {
		t.Fatalf("a tool with no sink must fail, got %q", out)
	}
}

func TestNotifyUserDescriptionListsChannels(t *testing.T) {
	desc := NewNotifyUserTool(&stubNotifySink{channels: []string{"discord", "weixin"}}).Description()
	if !strings.Contains(desc, "discord, weixin") {
		t.Errorf("description must name the routes in order, got %q", desc)
	}

	// No sink / no channels: the sentence is simply absent — never a dangling
	// "Available channels: ".
	if d := NewNotifyUserTool(&stubNotifySink{}).Description(); strings.Contains(d, "Available channels") {
		t.Errorf("description must not advertise an empty list, got %q", d)
	}
}

func TestNotifyUserSummaries(t *testing.T) {
	args := `{"message":"构建完成了，要我看下报告吗？"}`
	if got := ToolArgsSummary(ToolNameNotifyUser, args); got != "构建完成了，要我看下报告吗？" {
		t.Errorf("ToolArgsSummary = %q", got)
	}
	if got := ToolArgsTitle(ToolNameNotifyUser, args); got != "构建完成了，要我看下报告吗？" {
		t.Errorf("ToolArgsTitle = %q", got)
	}
}
