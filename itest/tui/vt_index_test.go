//go:build integration

package tui

import (
	"strings"
	"testing"
)

// rows returns the screen flattened to one string per row, trailing blanks trimmed.
func rows(s *vtScreen) []string {
	out := make([]string, s.height)
	for y := range out {
		var b strings.Builder
		for x := 0; x < s.width; x++ {
			b.WriteString(s.grid[y][x].text)
		}
		out[y] = strings.TrimRight(b.String(), " ")
	}
	return out
}

// A newline on the LAST ROW OF THE SCROLL REGION must scroll the region, not walk the cursor past
// it. The renderer sets ESC[20;39r and draws with newlines inside that region; testing the screen
// height instead of the region's bottom leaves the region's last row unwritten and shifts everything
// one line lower than a terminal would — a duplicated row plus a missing one on screen.
func TestNewlineScrollsTheRegionNotTheScreen(t *testing.T) {
	s := newVTScreen(8, 40)
	// Region rows 20..39 (1-based) — what the picker's renderer sets.
	s.feed("\x1b[20;39r")
	// Two markers: one above the region (row 1) and one in the region's first row (row 20).
	s.feed("\x1b[1;1HABOVE")
	// A marker in the MIDDLE of the region: a scroll up moves it one row up. (The region's first row
	// is the one a scroll up discards — putting the marker there would test nothing.)
	s.feed("\x1b[30;1HMID")
	// Bottom of the region, then a newline: the region scrolls by one.
	s.feed("\x1b[39;1H\n")

	got := rows(s)
	if got[0] != "ABOVE" {
		t.Fatalf("row 1 (outside the region) changed: %q — a region scroll must not touch it", got[0])
	}
	if got[28] != "MID" {
		t.Fatalf("row 29 = %q, want %q: the region scrolled up, so row 30's content moved up one", got[28], "MID")
	}
	if got[38] != "" {
		t.Fatalf("row 39 (region bottom) = %q, want blank: that is the line a scroll frees up", got[38])
	}
}

// Newline above the region's bottom only moves the cursor: rows must not move.
func TestNewlineAboveTheRegionBottomDoesNotScroll(t *testing.T) {
	s := newVTScreen(8, 40)
	s.feed("\x1b[20;39r")
	s.feed("\x1b[20;1HROW20")
	s.feed("\x1b[25;1HROW25")
	s.feed("\x1b[21;1H\n") // row 21 → 22, no scroll

	got := rows(s)
	if got[19] != "ROW20" || got[24] != "ROW25" {
		t.Fatalf("rows moved without a scroll: row20=%q row25=%q", got[19], got[24])
	}
	if s.curY != 21 { // 0-based: row 22
		t.Fatalf("cursor at y=%d, want 21 (row 22)", s.curY)
	}
}
