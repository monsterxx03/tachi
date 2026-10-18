package main

import (
	"context"
	"strings"
	"testing"

	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/config"
)

// /dream is a desktop command now: the registry must advertise it AND a
// handler must exist, or the palette offers a command that answers "未知命令".
func TestDreamCommandIsAdvertisedAndImplemented(t *testing.T) {
	if _, ok := desktopCommandHandlers["dream"]; !ok {
		t.Fatal("desktopCommandHandlers has no dream handler")
	}
	if got := laneFor("dream"); got != laneOneOff {
		t.Fatalf("laneFor(dream) = %v, want laneOneOff", got)
	}
	var found bool
	for _, def := range cmds.MatchPrefixForMode("dream", cmds.ModeDesktop) {
		if def.Name == "dream" {
			found = true
		}
	}
	if !found {
		t.Fatal("shared registry does not offer /dream to desktop")
	}
}

// The timer and a typed /dream share one gate: two passes must never rewrite
// the same memory directory at once.
func TestDreamPassGateIsExclusive(t *testing.T) {
	d := newDesktopApp()
	if !d.beginDreamPass() {
		t.Fatal("first pass was refused")
	}
	if d.beginDreamPass() {
		t.Fatal("second pass started while the first one held the gate")
	}
	if !d.dreamPassRunning() {
		t.Fatal("dreamPassRunning says idle while a pass holds the gate")
	}
	d.endDreamPass()
	if d.dreamPassRunning() {
		t.Fatal("gate still held after the pass ended")
	}
	if !d.beginDreamPass() {
		t.Fatal("gate was not released")
	}
	d.endDreamPass()
}

// A pass must refuse to pretend it ran when there is no config to run against.
func TestRunDreamPassWithoutConfig(t *testing.T) {
	d := newDesktopApp()
	_, err := d.runDreamPass(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "配置未加载") {
		t.Fatalf("runDreamPass without config: got %v, want 配置未加载", err)
	}
}

// With the switch off the desktop does not consolidate at all — not on the
// timer (which is never started) and not through /dream, which is the same
// pass. A user who has not turned memory consolidation on must not get their
// sessions rewritten by a command or a tick.
func TestRunDreamPassRefusesWhenDisabled(t *testing.T) {
	d := newDesktopApp()
	d.cfg = &config.Config{} // Dream.Enabled defaults to false

	_, err := d.runDreamPass(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "dream.enabled") {
		t.Fatalf("runDreamPass with dream off: got %v, want a dream.enabled refusal", err)
	}
}
