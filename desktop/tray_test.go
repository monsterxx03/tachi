package main

import "testing"

// 菜单栏那一格的读数测试。
//
// 这个文件原先叫 sessioninfo_test.go：那时它还覆盖设备双击确定看会话账的那三行排版
// （"12.3k / 32k (39%)" 之类）。那张屏和那条协议一起删了（协议 v2，见
// desktop/link/link.go 的 Proto），留下的是菜单栏自己在用的 formatCost / toFloat——
// trayContext 的测试在 agent_turn_test.go 里。

func TestFormatCost(t *testing.T) {
	// 单位与位数与桌面端一致（人民币、三位——单次调用可能只有几厘）。
	if got := formatCost(0.42); got != "¥0.420" {
		t.Errorf("formatCost = %q", got)
	}
	if got := formatCost(12.5); got != "¥12.500" {
		t.Errorf("formatCost = %q", got)
	}
	// 拿不到就别画 "¥0"——那看起来像「这次没花钱」，而真相是「还没算」。
	for _, v := range []any{nil, 0.0, "0.42"} {
		if got := formatCost(v); got != "" {
			t.Errorf("formatCost(%v) = %q, want empty", v, got)
		}
	}
}

// 费用数字装在 any 里（refreshTray 的输入来自 map[string]any），而 JSON 往返之后宽度
// 不一定还是 int——宽容一点比 assert 到底更省事。
func TestNumberCoercion(t *testing.T) {
	if toFloat(7) != 7 || toFloat(float32(0.5)) != float64(float32(0.5)) || toFloat(nil) != 0 {
		t.Error("toFloat 没按预期宽容/保守")
	}
}

// TestTrayStatusItemText：托盘菜单里那行状态要同时说清「在干什么」和两个数字。
//
// 为什么这条值得钉：Linux 上托盘菜单是这两个数字唯一能出现的地方（SetLabel 和 tooltip
// 都不显示，见 setupTray），而只给数字会让人不知道 app 是忙还是闲——状态词不能因为
// 「有数字了」就被省掉。
func TestTrayStatusItemText(t *testing.T) {
	st := AgentState{Status: StatusIdle, Label: "空闲"}
	tests := []struct {
		cost, context, want string
	}{
		{"¥0.386", "42%", "运行状态: 空闲 · ¥0.386 · 42%"},
		{"¥0.386", "", "运行状态: 空闲 · ¥0.386"},
		{"", "42%", "运行状态: 空闲 · 42%"},
		// 两个都没有时只剩状态词，不能留一个空的「运行状态: 」。
		{"", "", "运行状态: 空闲"},
	}
	for _, tc := range tests {
		if got := trayStatusItemText(st, tc.cost, tc.context); got != tc.want {
			t.Errorf("trayStatusItemText(%q, %q) = %q, want %q", tc.cost, tc.context, got, tc.want)
		}
	}
}
