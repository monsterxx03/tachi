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
//
// 没能力的时候要说出来，不能装死：Available 为 false 时调用方应当把这件事讲给用户
// （设备屏幕上要看得见），而不是静默失败。
//
//go:generate ./build_shim.sh
package stt

/*
#cgo LDFLAGS: ${SRCDIR}/libtachi_stt.a -framework Speech -framework AVFoundation -framework Foundation
#include <stdint.h>
#include <stdlib.h>

int32_t tachi_stt_available(void);
char *tachi_stt_engine_note(void);
void tachi_stt_free(char *pointer);
*/
import "C"

import "errors"

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
