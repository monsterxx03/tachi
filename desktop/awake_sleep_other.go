//go:build !darwin && !linux

package main

import "errors"

// 保持唤醒只有 macOS 与 Linux 两半（desktop 只出这两个平台，见 docs/agents/desktop.md）。这里
// 明确失败而不是假装成功：一个「开着」却什么也没挡住的开关比没有这个开关更糟。
func acquireSleep(string) (sleepGrant, error) {
	return nil, errors.New("这个平台不支持保持唤醒")
}
