//go:build !darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// windowWidthSlack is how far the width may drift and still be the size the fixtures describe.
// A compositor is allowed to add a decoration; nothing legitimate halves it.
const windowWidthSlack = 24

// hyprClient is the part of `hyprctl clients -j` this file reads.
type hyprClient struct {
	Pid  int    `json:"pid"`
	Size [2]int `json:"size"`
}

// compositorWindowSize reports the size the compositor actually gave this process's window.
//
// ok is false when nothing here knows how to ask (no hyprctl, or a compositor with no such
// query): the caller has no opinion then, which is a different answer from "the wrong size".
func compositorWindowSize(pid int) (w, h int, ok bool) {
	out, err := exec.Command("hyprctl", "clients", "-j").Output()
	if err != nil {
		return 0, 0, false
	}
	var clients []hyprClient
	if json.Unmarshal(out, &clients) != nil {
		return 0, 0, false
	}
	for _, c := range clients {
		if c.Pid == pid {
			return c.Size[0], c.Size[1], true
		}
	}
	return 0, 0, false
}

// windowChecked makes the preflight a once-per-run question: every scenario opens the same
// window on the same compositor, so the first answer holds for all of them.
var windowChecked bool

// checkWindowSize stops the run when the compositor gave the window a size the fixtures do not
// describe.
//
// This is the honest half of a problem the suite cannot solve by itself: on a tiling compositor
// the window shares the workspace with whatever else is open (on the machine this was measured
// on: the maintainer's own Tachi), so the app comes up at whatever the layout leaves — 1050 wide
// beside a 1320-wide window. Every layout assertion then measures the LAYOUT: panels clamp to
// their minimum width, elements that were on screen are collapsed, clicks land on nothing, and
// the run reports ~17 scenario failures that name chips and panels instead of a window. One line
// naming the real cause is worth more than those, and exit 2 says "this environment cannot
// answer", not "the product is broken".
//
// It says nothing at all when nothing here knows how to ask (no hyprctl, another compositor):
// silence is the correct answer to "what size is the window on a platform with no opinion in
// this file".
func checkWindowSize(pid int) {
	if windowChecked || pid <= 0 {
		return
	}
	w, h, ok := compositorWindowSize(pid)
	if !ok {
		return
	}
	windowChecked = true
	if w >= smokeWindowWidth-windowWidthSlack {
		return
	}
	fmt.Fprintf(os.Stderr, "desktop-smoke: 窗口被合成器定成了 %dx%d，而场景假定 %d 宽\n", w, h, smokeWindowWidth)
	fmt.Fprintln(os.Stderr, "  平铺合成器按布局给窗口定尺寸，应用说了不算：它和别的窗口并排，面板被压到最小宽度，"+
		"点击落到看不见的元素上——于是场景以「chip 不见了」「等了 30s」的样子失败，看着像产品坏了。")
	fmt.Fprintln(os.Stderr, "  给这个套件一个独立显示：Xvfb :99 + GDK_BACKEND=x11（见 docs/agents/desktop.md 的「验证」）。")
	killAll()
	os.Exit(2)
}
