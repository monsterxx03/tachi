package tui

import (
	"os"
	"strings"
	"testing"
)

// TestReplayTheDirtyScreenFromCI 是这条 bug 的**确定性复现**：`testdata/m27-dirty-screen.ansi`
// 就是 CI 上 M2.7 失败那一刻渲染端真正写出去的最后 4KB（从 `gh run view --attempt 1
// --log-failed` 里把那串 quoted 流解出来，一个字没改）。
//
// 为什么要它：这条用例在此之前只能靠 CI 掷骰子 —— 本机四路 CPU 打满跑 20 次、全套
// `-p --procs=4` 三轮 93 个 spec 全绿，复现不出来。有了这段真实的字节，行寻址的讨论就从
// 「猜是哪条序列」变成「喂进去看屏幕」，而且以后动 vt 的人立刻知道自己有没有弄坏别处。
//
// 它钉的是两颗 bug 合起来的后果（见 #130 `copy` 与 #132 `\n`/IND/RI 认区域，以及 ED 清屏
// 清错行）：
//
//   - ED（`\x1b[J`）的循环调了 eraseLine —— 那个函数动的是游标行，于是「清到屏幕尾」只把
//     光标那一行清了 N 次，下面的旧帧一行没动；
//   - `\n` 判的是屏幕高度而不是滚动区域底部，于是整个画面比真实终端低一行。
//
// 两个都修好之后，重放出来的一屏只剩过滤结果；少任何一个，下面任何一条断言都会红。
func TestReplayTheDirtyScreenFromCI(t *testing.T) {
	raw, err := os.ReadFile("testdata/m27-dirty-screen.ansi")
	if err != nil {
		t.Fatalf("读不到 fixture：%v", err)
	}
	s := newVTScreen(120, 40)
	s.feed(string(raw))
	out := s.Text()

	// 过滤到 mock-07 之后该留下的：标题里的 1/25 与那一行本身。
	for _, want := range []string{"1/25", "mock-07"} {
		if !strings.Contains(out, want) {
			t.Errorf("屏幕上应该有 %q，实际：\n%s", want, out)
		}
	}
	// 旧帧的行不该留：那次 CI 失败就是因为 mock-08 还在屏幕上（断言
	// 「过滤之后别的行还在屏幕上」）。多挑几行，免得只是碰巧擦掉了一行。
	for _, stale := range []string{"mock-08", "mock-12", "mock-18", "mock-24"} {
		if strings.Contains(out, stale) {
			t.Errorf("过滤之后 %q 还在屏幕上（清屏只清了光标行）：\n%s", stale, out)
		}
	}
}
