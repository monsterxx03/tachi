package main

import (
	"os"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/desktop/link"
)

// 设备翻页事件的名字在这条链路的两端各写了一份（remote.go 的 chatScrollEventName，App.tsx 里
// Events.On 的字符串），而拼错一个字的后果是「设备上按了、窗口什么都不动」——没有任何东西会报错。
// 把前端源码读出来对一遍，是主机侧唯一能发现它的地方。
func TestTheDeviceScrollEventNameMatchesTheFrontend(t *testing.T) {
	src, err := os.ReadFile("frontend/src/App.tsx")
	if err != nil {
		t.Fatalf("读不到前端源码：%v", err)
	}
	if !strings.Contains(string(src), "'"+chatScrollEventName+"'") {
		t.Errorf("App.tsx 里没有订阅 %q —— 设备翻页会静默地什么也不做", chatScrollEventName)
	}
}

// 手势到滚动方向的翻译。方向反了在屏幕上看着像「按键坏了」（按上却往下跑），而这一层是唯一
// 能钉住它的地方——再往下就是 DOM 了。
func TestScrollDirection(t *testing.T) {
	cases := []struct {
		action string
		delta  int
		scroll bool
	}{
		{link.KeyUp, -1, true},
		{link.KeyDown, 1, true},
		// 切前台不是翻页：它必须落进「不是翻页」那一支，否则一下按键会同时做两件事。
		{link.KeyActivate, 0, false},
		// 不认识的动作用户按下去什么都不发生（设备的手势将来可能多一个）。
		{"ok", 0, false},
		{"ok_double", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		delta, scroll := scrollDirection(c.action)
		if scroll != c.scroll || delta != c.delta {
			t.Errorf("scrollDirection(%q) = (%d, %v), want (%d, %v)", c.action, delta, scroll, c.delta, c.scroll)
		}
	}
}
