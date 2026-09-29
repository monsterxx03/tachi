package main

import (
	"fmt"

	"github.com/monsterxx03/tachi/desktop/link"
)

// SessionInfo 实现 link.Source：把「当前活跃会话的账」排成设备能直接画的那几行。
//
// 数据全是现成的（模型与上下文占用在 GetProviderInfo 里，花费在 GetSessionUsage 里），
// 这里只做**排版**：货币、k/M 缩写、百分比。排版留在主机是有意的——设备那一屏要的是能
// 直接画出来的字符串，而不是几个还得自己算的数（设计文档 §2：语义留在主机）。
//
// 它说的是**当前活跃会话**（和语音投递同一个口径），不是设备屏幕上正显示的那个会话：
// 所以那个会话名必须画出来，否则这一屏会误导（见下面 Title 那段）。
func (d *desktopApp) SessionInfo() link.Info {
	svc := &AgentService{desk: d}

	d.mu.Lock()
	id := d.activeID
	d.mu.Unlock()

	if id == "" {
		// 没有活跃会话时那一屏也该有话说：空白看起来像坏了。
		return link.Info{Title: "（没有活跃会话）"}
	}

	info := link.Info{
		// 会话名不能省：设备屏幕上此刻可能正显示着**另一个**会话在等你（屏幕看的是
		// 「谁在等你」，而这里说的是当前活跃会话的账）。少了名字，这一屏就是在骗人。
		Title: d.sessionTitle(id),
	}
	if provider := svc.GetProviderInfo(); provider != nil {
		info.Model, _ = provider["model"].(string)
		info.Context = formatContext(toInt64(provider["contextEstimate"]),
			toInt64(provider["contextWindow"]))
	}
	info.Cost = formatCost(svc.GetSessionUsage(id)["cost"])
	return info
}

// formatContext 排成 "12.3k / 32k (42%)"。
//
// 窗口未知（win == 0）时只给 tokens、不给百分比——这是 GetProviderInfo 那边定下的约定
// （"the frontend shows tokens without a percentage"），照它来：凭空算一个百分比比
// 不显示更坏。
func formatContext(estimate, window int64) string {
	if estimate <= 0 && window <= 0 {
		return "" // 什么都没测到：这一行不画
	}
	if window <= 0 {
		return formatTokens(estimate)
	}
	percent := int(float64(estimate)/float64(window)*100 + 0.5)
	return fmt.Sprintf("%s / %s (%d%%)", formatTokens(estimate), formatTokens(window), percent)
}

// formatTokens 把 token 数缩到三四个字符：12345 → "12.3k"，1234567 → "1.2M"。
//
// 屏幕只有 240 像素宽，而这一行还要装下 " / 32k (42%)"——所以缩写在设备这一端是必需的，
// 而它只该有一个实现（这里是它唯一的实现）。
func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return trimZero(float64(n)/1_000_000) + "M"
	case n >= 1_000:
		return trimZero(float64(n)/1_000) + "k"
	default:
		return fmt.Sprintf("%d", n)
	}
}

// trimZero 保留一位小数，整数时不带小数点：12.3 → "12.3"，32.0 → "32"。
func trimZero(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	if len(s) > 2 && s[len(s)-2:] == ".0" {
		return s[:len(s)-2]
	}
	return s
}

// formatCost 排成 "$0.42"；拿不到就不显示（空字符串），而不是画一个 "$0"——那看起来像
// 「这次没花钱」，而真相是「还没算」。
func formatCost(v any) string {
	f := toFloat(v)
	if f <= 0 {
		return ""
	}
	return fmt.Sprintf("$%.2f", f)
}

// toInt64 / toFloat 是给 map[string]any 用的：那两个数据源（GetProviderInfo /
// GetSessionUsage）把数字装在 any 里，而 JSON 之外没人保证它是哪个宽度。
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float32:
		return float64(n)
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}
