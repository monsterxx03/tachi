package main

import "testing"

// 这几行是设备屏幕上直接画出来的字，所以它们的排版只有一处实现、也只有这一份测试：
// 12345/32000 该显示成 "12.3k / 32k (39%)"，多一位少一位都会让那一行挤不下或看不明白。
func TestFormatTokens(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1k"},
		{12345, "12.3k"},
		{32000, "32k"},
		{999999, "1000k"}, // 四舍五入到 1000.0k 也不换成 M：1M 那条线是给更大的数留的
		{1000000, "1M"},
		{1234567, "1.2M"},
	}
	for _, c := range cases {
		if got := formatTokens(c.in); got != c.want {
			t.Errorf("formatTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatContext(t *testing.T) {
	// 知道窗口：给百分比。取整用四舍五入——12345/32000 = 38.6% 要显示 39%。
	if got := formatContext(12345, 32000); got != "12.3k / 32k (39%)" {
		t.Errorf("formatContext = %q", got)
	}
	// 窗口未知（模型的窗口没告诉我们）：只给 tokens，不给百分比——凭空算一个比不显示更坏。
	if got := formatContext(12345, 0); got != "12.3k" {
		t.Errorf("窗口未知时 formatContext = %q, want just the tokens", got)
	}
	// 什么都没测到：这一行不画（空字符串），而不是画一个 "0"。
	if got := formatContext(0, 0); got != "" {
		t.Errorf("formatContext(0,0) = %q, want empty", got)
	}
}

func TestFormatCost(t *testing.T) {
	if got := formatCost(0.42); got != "$0.42" {
		t.Errorf("formatCost = %q", got)
	}
	if got := formatCost(12.5); got != "$12.50" {
		t.Errorf("formatCost = %q", got)
	}
	// 拿不到就别画 "$0"——那看起来像「这次没花钱」，而真相是「还没算」。
	for _, v := range []any{nil, 0.0, "0.42"} {
		if got := formatCost(v); got != "" {
			t.Errorf("formatCost(%v) = %q, want empty", v, got)
		}
	}
}

// 那两个数据源的数字装在 any 里（GetProviderInfo / GetSessionUsage），而 JSON 往返之后
// 宽度不一定还是 int——宽容一点比 assert 到底更省事。
func TestNumberCoercion(t *testing.T) {
	if toInt64(int32(7)) != 7 || toInt64(float64(7.9)) != 7 || toInt64("7") != 0 {
		t.Error("toInt64 没按预期宽容/保守")
	}
	if toFloat(7) != 7 || toFloat(float32(0.5)) != float64(float32(0.5)) || toFloat(nil) != 0 {
		t.Error("toFloat 没按预期宽容/保守")
	}
}
