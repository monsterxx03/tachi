package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// The tray readout, mirrored to disk for consumers OUTSIDE this process.
//
// A Linux tray host shows neither the SNI label nor a live tooltip (see refreshTray), so the
// two numbers a desktop user wants at a glance have no surface there at all — only Tachi's own
// menu carries them, which is one click away from being useless. This file is the missing
// surface: every tray repaint writes the state, today's spend and the displayed session's
// context share to $TACHI_HOME/desktop_status.json, and anything that can read a small JSON
// file can draw them. On this machine that consumer is a quickshell bar widget, which lives in
// the user's own config (~/.config/omarchy/plugins/<user>.tachi) rather than in this repo; it
// polls the file, because every write replaces the path's inode (see writeDesktopStatus).
//
// The values ARE the tray's — the same AgentState, the same two numbers, formatted only by
// whoever draws them — so the bar and the menu can never disagree about what today cost.

const desktopStatusFile = "desktop_status.json"

// desktopStatus is the file's shape.
//
// cost and contextPercent are OMITTED when they are not known, exactly as the tray label
// omits them (formatCost / trayContext): a zero in the file would read as "nothing spent" or
// "an empty context", which is a different claim from "not measured yet".
type desktopStatus struct {
	Status string `json:"status"`
	// Label is the short state word the tray menu carries ("空闲", "思考"), Detail the
	// one-line description under it — the two halves of what Tachi is doing, which the
	// numbers do not say.
	Label  string  `json:"label"`
	Detail string  `json:"detail"`
	Cost   float64 `json:"cost,omitempty"`
	// A POINTER, not a plain int: 0% is a real reading (a session that just started) and
	// has to survive omitempty, while "not measured" (-1) must not be written as 0.
	ContextPercent *int `json:"contextPercent,omitempty"`
	// UpdatedAt is RFC3339, for a consumer that wants to tell "the app is gone" from "the
	// app is idle" — the file itself cannot tell the two apart (see removeDesktopStatus).
	UpdatedAt string `json:"updatedAt"`
}

func desktopStatusPath() string { return filepath.Join(config.BaseDir(), desktopStatusFile) }

// desktopStatusMu serializes the writes, because two repaints CAN overlap: the app runs each
// session's turn on its own goroutine and every state change repaints the tray. What that races
// on is not this file but the temp file underneath it — fileutil.AtomicWriteFile writes through
// ONE path (`<path>.tmp`) — so without the lock two writers can interleave into that temp and
// leave a rename finding it already gone. The lock costs nothing and removes the whole class.
var desktopStatusMu sync.Mutex

// writeDesktopStatus publishes one repaint. Best effort: a failure costs the bar its readout
// and nothing else — the tray in this process is already painted.
//
// Written atomically (temp + rename) so a consumer can never read half a file. The price is
// that every write replaces the path's inode, which is why a consumer POLLS this file instead
// of watching it: a watcher holds the inode it opened.
func writeDesktopStatus(st AgentState, cost float64, percent int) {
	s := desktopStatus{
		Status:    string(st.Status),
		Label:     st.Label,
		Detail:    st.Detail,
		Cost:      cost,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	if percent >= 0 {
		s.ContextPercent = &percent
	}
	desktopStatusMu.Lock()
	defer desktopStatusMu.Unlock()
	if err := fileutil.AtomicWriteJSONShared(desktopStatusPath(), s); err != nil {
		log.Printf("desktop: cannot write %s: %v", desktopStatusPath(), err)
	}
}

// removeDesktopStatus withdraws the readout as the app shuts down, so a bar drawing it stops
// drawing rather than showing the last numbers forever.
//
// It is deliberately not a heartbeat: an app that is merely IDLE writes nothing for as long as
// nothing happens, and a consumer that hid the readout on age would blink out exactly when the
// user is working quietly. The cost of this choice is that a CRASH leaves the file behind (the
// cleanup never runs) and the bar keeps showing the numbers of a process that is gone — until
// the next launch overwrites them.
func removeDesktopStatus() {
	desktopStatusMu.Lock()
	defer desktopStatusMu.Unlock()
	if err := os.Remove(desktopStatusPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("desktop: cannot remove %s: %v", desktopStatusPath(), err)
	}
}
