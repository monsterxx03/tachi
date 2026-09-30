//go:build darwin && cgo

// Package stt 把 macOS 自己的语音识别接进来——本机转写，不联网、不走 llm provider。
//
// 这一层的形状由两件事决定，都不是选择：
//
//  1. **底层是 Swift，不是 ObjC。** macOS 26 的 SpeechAnalyzer / SpeechTranscriber 只在
//     Swift 的模块接口里存在，ObjC 头文件里只有老的 SFSpeechRecognizer。所以垫片由
//     shim.swift 编译而来（desktop/stt/build_shim.sh），而不是像 desktop 里那些内联
//     ObjC 一样写在 cgo 的注释块里。
//  2. **它挡在同步与异步之间。** Swift 那边的 API 全是 async，而 cgo 只能调同步函数；
//     垫片自己用信号量把结果收回来（见 shim.swift 的 syncWait）。
//  3. **链接要多一条搜索路径，运行时还要一条 rpath。** 部署目标 12.0 的 Swift 产物里带了一条
//     `-lswiftCompatibility56` 的 auto-link 指令，而那个库只在 toolchain 里、不在 SDK 里
//     （见 build_shim.sh），所以下面 LDFLAGS 里的 `-L${SRCDIR}` 不是多余的：少了它整个包
//     链接失败，报的是 `__swift_FORCE_LOAD_$_swiftCompatibility56` 未定义。
//     而 `-Wl,-rpath,/usr/lib/swift` 管的是**跑起来之后**：识别那半用了 Swift 并发，产物因此
//     依赖 `libswift_Concurrency.dylib`，dyld 按 `@rpath` 找它——app 的 bundle 自带那条
//     搜索路径，**Go 链接出来的测试二进制没有**，少了它就是一句
//     `Library not loaded: @rpath/libswift_Concurrency.dylib` 加一个 abort。
//
// 没能力的时候要说出来，不能装死：Available 为 false 时调用方应当把这件事讲给用户
// （设备屏幕上要看得见），而不是静默失败。
//
//go:generate ./build_shim.sh
package stt

/*
#cgo LDFLAGS: -L${SRCDIR} ${SRCDIR}/libtachi_stt.a -framework Speech -framework AVFoundation -framework Foundation
#cgo LDFLAGS: -Wl,-rpath,/usr/lib/swift
#include <stdint.h>
#include <stdlib.h>

int32_t tachi_stt_available(void);
char *tachi_stt_engine_note(void);
char *tachi_stt_transcribe(const int16_t *samples, size_t count, double sample_rate);
void tachi_stt_free(char *pointer);

int32_t tachi_stt_mic_status(void);
int32_t tachi_stt_mic_ask(double timeout_seconds);
int32_t tachi_stt_record_start(void);
int32_t tachi_stt_record_stop(int16_t **samples, size_t *count, double *rate);
void tachi_stt_free_samples(int16_t *samples);
*/
import "C"

import (
	"errors"
	"time"
	"unsafe"
)

// ErrUnavailable 表示这台机器做不了本机转写。返回它的地方都不该静默——调用方要把它
// 变成用户看得见的一句话（设备屏幕上那条 error）。
var ErrUnavailable = errors.New("本机语音识别不可用")

// Available 回答「这台机器能不能做本机转写」。
func Available() bool {
	return C.tachi_stt_available() == 1
}

// EngineNote 返回一句引擎现状（已装资产的语言等），给日志和自检用。
//
// 它大部分时候没有调用价值，存在的意义是把整条链一次问通：Go → cgo → Swift →
// Speech.framework → 异步资产查询 → Swift → cgo → Go。任何一环断了，这里就是第一个
// 说出来的地方。
func EngineNote() string {
	raw := C.tachi_stt_engine_note()
	if raw == nil {
		return ""
	}
	// 所有权：垫片 malloc 出来的缓冲由 Go 释放（见 shim.swift 的 tachi_stt_free）。
	defer C.tachi_stt_free(raw)
	return C.GoString(raw)
}

// Transcribe 把一段单声道 PCM 转成一句话。
//
// 两个模块的结果都拿回来了（垫片交回一行 JSON），由 Choose 选一个——判据放在 Go 侧是因为
// 它是纯逻辑，能在主机上被真机数据测（choose_test.go 里那 18 条）。
//
// Result.Text 为空表示「听清了，但没人说话」；拿不到能力或转写本身失败才是 error。两者
// 必须分得开：前者是正常输入（用户按错了），后者要变成用户看得见的一句话。
//
// ⚠ 失败时也把 Result 交出去（带 Missing）：一门语言缺离线资产**不是**「这台机器做不了」
// ——另一门还能用，垫片也已经在后台装它了。调用方据此决定跟用户说什么（desktop/badge.go）。
func Transcribe(samples []int16, sampleRate int) (Result, error) {
	if len(samples) == 0 || sampleRate <= 0 {
		return Result{}, nil
	}
	if !Available() {
		return Result{}, ErrUnavailable
	}
	raw := C.tachi_stt_transcribe((*C.int16_t)(unsafe.Pointer(&samples[0])),
		C.size_t(len(samples)), C.double(sampleRate))
	if raw == nil {
		// NULL 只剩「垫片压根没跑起来」这一种意思（系统太老、参数没法用）——转写失败的
		// **原因**不走这条路，它在 JSON 的 error 里，见 shim.swift。
		return Result{}, errors.New("stt: 垫片没有给出结果")
	}
	defer C.tachi_stt_free(raw)

	got, err := ParseTranscription(C.GoString(raw))
	if err != nil {
		// 连原因一起交出去：调用方要能分辨「缺资产」（会自己好）和别的失败。
		return Result{Missing: got.Missing, Installing: got.Installing}, err
	}
	return Result{Text: Choose(got), Missing: got.Missing, Installing: got.Installing}, nil
}

// --- 采集（麦克风）--------------------------------------------------------------

// 采集与转写是这条链的两半，权限也是两个：麦克风那条问的是「能不能听」（
// NSMicrophoneUsageDescription），转写那条问的是「能不能识别」（NSSpeechRecognitionUsage
// Description）。有其中一个不等于有另一个，所以下面这几个 error 要分得开——调用方要把它们
// 变成三句不同的话，而那句「缺离线资产」跟它们都不是一回事。
var (
	// ErrMicUnasked 表示授权窗弹出来了但还没有结果（用户没点，或者点了之后没等到）。
	// 这不是拒绝：下一次调用会重新问一遍状态，多半就好了。
	ErrMicUnasked = errors.New("麦克风授权还没有结果")
	// ErrMicDenied 表示用户拒过（或系统策略挡着）。这条路只会一直拒下去，得去系统设置里改。
	ErrMicDenied = errors.New("麦克风权限被拒绝")
	// ErrMicUnavailable 表示问下来有权限、引擎却起不来：没有输入设备，或者被别的程序独占。
	ErrMicUnavailable = errors.New("麦克风起不来")
	// ErrNotRecording 表示当前没有在采集（重复点了停止，或者本来就没起来）。
	ErrNotRecording = errors.New("当前没有在录音")
)

// micAskTimeout 是「等用户回答授权窗」的上限。等的是人，所以给得宽；超时只是让调用方
// 能说一句「那个窗口还开着」，而不是把它变成一次永远不返回的点击。
const micAskTimeout = 30 * time.Second

// micPermission 是垫片对授权状态的回答，和 tachi_stt_mic_status 的三个返回值一一对应。
type micPermission int32

const (
	micUnasked micPermission = 0
	micGranted micPermission = 1
	micDenied  micPermission = 2
)

// StartRecording 开始采集，拿到的样本与 badge 那条路的输入同形（16 kHz 单声道 Int16）。
//
// **首次使用时它会先弹一次授权窗并等用户点完**（最多 micAskTimeout）。等，是因为调用它的
// 就是「用户按了麦克风」那一下：不等的话第一次使用要按两下，而用户按的正是「开始说话」。
// 等出来的三种结果各是一句话，所以是三个 error 而不是一个。
//
// 这里不检查 Available()：采集本身在 macOS 26 之前就有（AVAudioEngine 老得多），要求 26 的
// 是转写那一半。整个功能能不能用由调用方按 Available() 决定。
func StartRecording() error {
	switch micStatus() {
	case micUnasked:
		if status := micPermission(C.tachi_stt_mic_ask(C.double(micAskTimeout.Seconds()))); status != micGranted {
			return micAskError(status)
		}
	case micDenied:
		return ErrMicDenied
	}
	if C.tachi_stt_record_start() != 1 {
		return ErrMicUnavailable
	}
	return nil
}

// StopRecording 结束采集并交回样本与它们的采样率。
//
// 样本为空**不是**错误：那是「按下了但没说话」，识别会如实回答一句空文本。所以调用方拿到
// 的是 (nil, rate, nil)——采样率照样有意义，用它去转写空样本才是错的（Transcribe 对空输入
// 直接返回空结果）。
func StopRecording() ([]int16, int, error) {
	var samples *C.int16_t
	var count C.size_t
	var rate C.double
	if C.tachi_stt_record_stop(&samples, &count, &rate) != 1 {
		return nil, 0, ErrNotRecording
	}
	hz := int(rate)
	if samples == nil || count == 0 {
		return nil, hz, nil
	}
	// 所有权：这一段是垫片 allocate 出来的，必须由 tachi_stt_free_samples 释放——它和
	// tachi_stt_free 那一对（strdup 出来的字符串）不是同一次分配，配错了就是堆损坏。
	defer C.tachi_stt_free_samples(samples)
	out := make([]int16, int(count))
	copy(out, unsafe.Slice((*int16)(unsafe.Pointer(samples)), int(count)))
	return out, hz, nil
}

// micStatus 问一次当前的授权状态。
func micStatus() micPermission {
	return micPermission(C.tachi_stt_mic_status())
}

// micAskError 把「等授权窗」的结果变成 error：还没点、被拒、或者刚刚拿到了权限（无事发生）。
func micAskError(status micPermission) error {
	if status == micDenied {
		return ErrMicDenied
	}
	// 剩下的是「窗口还开着」：不把它说成拒绝——用户多半会点允许，只是我们没等到。
	return ErrMicUnasked
}
