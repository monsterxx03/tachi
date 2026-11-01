//go:build integration

package tui

import (
	"strings"
	"testing"
)

// A scroll inside a region must MOVE rows, not make them share memory: the helpers below blank the
// rows that scrolled in, and with a slice assignment that blanking also wipes the row that was just
// moved up (the two are the same array). On screen that shows up as a line that disappears or
// repeats itself — the symptom itest/tui's M2.7 flakes with.
func TestScrollRegionMovesRowsWithoutAliasingThem(t *testing.T) {
	s := newVTScreen(8, 4)
	s.top, s.bot = 0, 3
	writeRow := func(y int, text string) {
		for x := range s.grid[y] {
			s.grid[y][x] = vtCell{text: " ", width: 1}
		}
		for i, r := range text {
			s.grid[y][i] = vtCell{text: string(r), width: 1}
		}
	}
	rowText := func(y int) string {
		out := ""
		for _, c := range s.grid[y] {
			out += c.text
		}
		return strings.TrimRight(out, " ")
	}
	writeRow(0, "AAAA")
	writeRow(1, "BBBB")
	writeRow(2, "CCCC")
	writeRow(3, "DDDD")

	s.scrollUp(1)

	for y, want := range []string{"BBBB", "CCCC", "DDDD", ""} {
		if got := rowText(y); got != want {
			t.Fatalf("after scrollUp(1) row %d = %q, want %q (rows %v)", y, got, want,
				[]string{rowText(0), rowText(1), rowText(2), rowText(3)})
		}
	}

	// And back down the other way: the same aliasing hazard, mirrored.
	s.scrollDown(1)
	for y, want := range []string{"", "BBBB", "CCCC", "DDDD"} {
		if got := rowText(y); got != want {
			t.Fatalf("after scrollDown(1) row %d = %q, want %q", y, got, want)
		}
	}
}

// Same for the line-editing pair, which scrolls within [curY..bot].
func TestInsertAndDeleteLinesKeepTheRowsWhole(t *testing.T) {
	s := newVTScreen(8, 4)
	s.top, s.bot = 0, 3
	writeRow := func(y int, text string) {
		for x := range s.grid[y] {
			s.grid[y][x] = vtCell{text: " ", width: 1}
		}
		for i, r := range text {
			s.grid[y][i] = vtCell{text: string(r), width: 1}
		}
	}
	rowText := func(y int) string {
		out := ""
		for _, c := range s.grid[y] {
			out += c.text
		}
		return strings.TrimRight(out, " ")
	}
	writeRow(0, "AAAA")
	writeRow(1, "BBBB")
	writeRow(2, "CCCC")
	writeRow(3, "DDDD")

	s.curY = 1
	s.insertLines(1)
	for y, want := range []string{"AAAA", "", "BBBB", "CCCC"} {
		if got := rowText(y); got != want {
			t.Fatalf("after insertLines(1) at row 1, row %d = %q, want %q", y, got, want)
		}
	}

	s.curY = 1
	s.deleteLines(1)
	for y, want := range []string{"AAAA", "BBBB", "CCCC", ""} {
		if got := rowText(y); got != want {
			t.Fatalf("after deleteLines(1) at row 1, row %d = %q, want %q", y, got, want)
		}
	}
}
