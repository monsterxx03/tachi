package main

import (
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// 一条通知什么时候不该发：每一个"是"都意味着有人已经看到了。
func TestShouldPush(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		muted   bool
		desktop bool
		phone   bool
		want    bool
	}{
		{"nobody is looking", true, false, false, false, true},
		{"global switch off", false, false, false, false, false},
		{"this conversation is muted", true, true, false, false, false},
		{"the window is showing it", true, false, true, false, false},
		{"a phone is subscribed to it", true, false, false, true, false},
		{"everything is off", false, true, true, true, false},
	}
	for _, c := range cases {
		if got := shouldPush(c.enabled, c.muted, c.desktop, c.phone); got != c.want {
			t.Errorf("%s: shouldPush = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSenderForOnlyBuildsConfiguredRelays(t *testing.T) {
	if got := senderFor(config.PushConfig{Enabled: true}); len(got) != 0 {
		t.Errorf("an empty block built %d senders", len(got))
	}
	both := config.PushConfig{
		Enabled: true,
		Bark:    config.BarkPushConfig{URL: "https://bark.example.com", Keys: []string{"k1"}},
		Ntfy:    config.NtfyPushConfig{URL: "https://ntfy.example.com", Topic: "t"},
	}
	if got := senderFor(both); len(got) != 2 {
		t.Errorf("both configured built %d senders, want 2", len(got))
	}
	// A key list with no URL (or the reverse) is a half-filled block, not a sender.
	half := config.PushConfig{Enabled: true, Bark: config.BarkPushConfig{Keys: []string{"k1"}}}
	if got := senderFor(half); len(got) != 0 {
		t.Errorf("half-filled bark built %d senders", len(got))
	}
}
