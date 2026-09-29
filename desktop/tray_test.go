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
