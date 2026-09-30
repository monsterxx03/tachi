//go:build !(darwin && cgo)

// 没有本机转写能力的平台（Linux、或者 CGO 关掉的 darwin）走这里。
//
// 桩存在的理由不是「编译得过就行」：语音回答那条链路上，一个回答就是在批准一条要执行
// 的命令，所以「这台机器做不了」必须是一句能讲给用户听的话（Available=false →
// 调用方回一条 error，设备屏幕上看得到），而不是一个静默的失败。
package stt

import "errors"

// ErrUnavailable 表示这台机器做不了本机转写。
var ErrUnavailable = errors.New("本机语音识别不可用")

// Available 恒为 false：这个平台没有接进来。
func Available() bool { return false }

// EngineNote 返回空串——没有引擎可问。
func EngineNote() string { return "" }

// Transcribe 在这个平台上永远做不了：调用方该把 ErrUnavailable 变成用户看得见的一句话。
func Transcribe(samples []int16, sampleRate int) (Result, error) {
	return Result{}, ErrUnavailable
}

// 下面这一组是采集那半的桩，理由和上面一样：调用方必须能有一句讲给用户听的话，而不是
// 一个静默的失败。**整个语音功能在问权限之前就已经按 Available() 停住了**（它为 false），
// 所以这里的返回值没有真实路径会用到——它们存在只是为了让包编译得过。
var (
	ErrMicUnasked     = errors.New("麦克风授权还没有结果")
	ErrMicDenied      = errors.New("麦克风权限被拒绝")
	ErrMicUnavailable = errors.New("麦克风起不来")
	ErrNotRecording   = errors.New("当前没有在录音")
)

// StartRecording 在这个平台上做不了：调用方拿到的是一句能讲给用户听的话。
func StartRecording() error { return ErrMicUnavailable }

// StopRecording 同上——没有采集可停。
func StopRecording() ([]int16, int, error) { return nil, 0, ErrNotRecording }
