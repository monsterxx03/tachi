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

// TestMicStatusCrossesEveryLayer 钉的是采集那半的第一段：Go → cgo → Swift → AVFoundation。
//
// 和 EngineNote 那条一样，它断言的是「跑到了」，不是某个特定值——授权状态是**这台机器**的状态
// （没问过、给了、拒了都对），不是代码对错。
//
// ⚠ 这里**不**调 StartRecording：没授权时它会去弹一次系统授权窗，而有授权时它会真的开麦克风。
// 一个测试两样都不能干（跑单测的人不会盯着屏幕等弹窗，而开着的麦克风也没人去关它）。真正的采集
// ——引擎起不起来、录到的样本形状对不对——只能在装了 app 的机器上按一次按钮手测，`make test`
// 覆盖不到那一段，别读成它覆盖了。
func TestMicStatusCrossesEveryLayer(t *testing.T) {
	switch got := micStatus(); got {
	case micUnasked, micGranted, micDenied:
		t.Logf("麦克风授权状态: %d（0 没问过 / 1 有 / 2 被拒）", got)
	default:
		t.Errorf("micStatus 返回了垫片不会给的值 %d：新符号没链上，或者返回值读错了", got)
	}
}
