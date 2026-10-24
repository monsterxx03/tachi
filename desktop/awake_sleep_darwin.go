//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

// 保持唤醒的 macOS 半边：`caffeinate -i -w <pid>`。
//
// 为什么是子进程，而不是 cgo 里的 IOPMAssertion —— 明明后者更直接：这个模块还有一种
// `-tags headless` 的构建，而它把 cgo 关掉了（`CGO_ENABLED=0`，见 Makefile 的 headless 目标），
// 一份 cgo 断言在那条路上根本编不进来，于是同一个开关在两种构建下会有两种行为。caffeinate 是
// 系统自带的（10.8 起），`-i` 是「别因为闲置而睡」（**不含**屏幕：屏幕照常黑），而 `-w` 让它盯
// 着我们的 pid：进程一没，它自己退出，断言随之消失 —— 与 Linux 那半（fd）是同一条不变量。
//
// 一个细节值得记住：caffeinate 起的断言在 `pmset -g assertions` 里署名
// 「caffeinate asserting on behalf of Process ID <我们>」—— 谁在挡，一眼可查。
type awakeGrant struct{ cmd *exec.Cmd }

func (g *awakeGrant) release() {
	if g.cmd == nil || g.cmd.Process == nil {
		return
	}
	_ = g.cmd.Process.Kill()
	// 收尸：一个被杀掉又没人 Wait 的孩子会一直挂在进程表里，开关来回拨五十次就是五十个僵尸。
	_, _ = g.cmd.Process.Wait()
}

// acquireSleep 起一个 caffeinate。它自己就是那条断言，所以失败必须报出来 —— 一个「开着」却
// 什么都没挡住的开关，比没有这个开关更糟。
func acquireSleep(_ string) (sleepGrant, error) {
	cmd := exec.Command("/usr/bin/caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("caffeinate 起不来：%w", err)
	}
	return &awakeGrant{cmd: cmd}, nil
}
