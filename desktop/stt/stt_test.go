//go:build darwin && cgo

package stt

import (
	"strings"
	"testing"
)

// TestEngineNoteCrossesEveryLayer 是这一层的存在理由：它一次问通整条链——
// Go → cgo → Swift → Speech.framework → 异步资产查询 → Swift → cgo → Go。
//
// 这条链上有四处是「猜的」而不是「已知的」，任何一处断了，人收到的都是一句看不懂的
// 链接错误或者一个静默的空串：
//
//   - swiftc 编出来的静态库能不能被 cgo 链上（Swift 运行时的搜索路径对不对）；
//   - C ABI 的字节与字符串能不能过（@_cdecl + strdup + free 的所有权）；
//   - 用信号量把 async 收敛成同步会不会死锁（syncWait 的形状）；
//   - SpeechTranscriber 那些属性要不要 TCC 授权（测试是裸 CLI，没有 .app 的 plist）。
//
// 所以断言的是**内容**而不只是「没报错」：一句 「installed=」 就说明 Swift 真的跑到了
// 资产查询那一步，而不是返回了一个空壳。
func TestEngineNoteCrossesEveryLayer(t *testing.T) {
	note := EngineNote()
	if note == "" {
		t.Fatal("EngineNote 是空的：垫片没被链上，或者 Swift 那边静默失败了")
	}
	t.Logf("引擎现状: %s", note)

	for _, want := range []string{"available=", "installed=", "supported="} {
		if !strings.Contains(note, want) {
			t.Errorf("EngineNote 少了 %q：%s", want, note)
		}
	}
}

// TestAvailable 只钉一件事：这个布尔必须真的来自 Speech.framework，而不是硬编码。
//
// 它无法断言「一定是 true」——那是机器状态（没装资产、或者系统太老），不是代码对错。
// 但 Available 与 EngineNote 必须自洽：后者里的 available= 是 Swift 直接问出来的。
func TestAvailableAgreesWithEngineNote(t *testing.T) {
	note := EngineNote()
	if note == "" {
		t.Skip("拿不到引擎现状，连自洽都没得谈（见上一个测试）")
	}
	// Swift 的 Bool 在这里被拼成 1/0，和 Available() 走的是同一个属性。
	want := "available=0"
	if Available() {
		want = "available=1"
	}
	if !strings.Contains(note, want) {
		t.Errorf("Available()=%v 但引擎说 %q", Available(), note)
	}
}
