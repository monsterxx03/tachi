//go:build linux

package main

import (
	"fmt"
	"syscall"

	"github.com/godbus/dbus/v5"
)

// 保持唤醒的 Linux 半边：向 logind 要一个 inhibitor。
//
// 选 `login1.Manager.Inhibit` 的原因有两条：它是 systemd（以及 elogind）自带的，不需要桌面环境
// 配合；而它返回的那个 fd 就是断言的寿命 —— fd 一关，禁令就没了，进程死掉时内核替我们关，所以
// 这里没有「忘了释放」这种坏结局。
//
// what=idle:sleep 是两件事一起挡：`sleep` 让 `systemctl suspend`（hypridle 这类空闲守护走的正是
// 它）被拒绝，`idle` 让 logind 自己的 IdleAction 不触发。mode=block 才是「拦下来」；delay 只是
// 推迟一会儿，等于没挡。屏幕变黑不在这条禁令的范围内（那是桌面自己的事，见 awake.go）。

// awakeGrant 是一条活着的 logind inhibitor。fd 必须一直开着 —— 它是断言本身，不是它的句柄。
//
// godbus 交回来的是一个真的文件描述符（dbus.UnixFD 就是那个整数），是我们进程里的一份拷贝，
// 所以由我们关：关掉之后 logind 立刻撤掉禁令。连接本身也留着，直到释放为止。
type awakeGrant struct {
	conn *dbus.Conn
	fd   dbus.UnixFD
}

func (g *awakeGrant) release() {
	// 先关 fd（禁令消失的那一刻），再关连接。
	_ = syscall.Close(int(g.fd))
	if g.conn != nil {
		_ = g.conn.Close()
	}
}

// acquireSleep takes the system-bus inhibitor.
//
// It uses a PRIVATE connection rather than dbus.SystemBus(): the shared one is process-wide and
// lazily connected, so closing it here would tear down a bus somebody else may be using. A private
// connection also keeps the release honest — closing it is this grant's own business.
func acquireSleep(why string) (sleepGrant, error) {
	conn, err := dbus.SystemBusPrivate()
	if err != nil {
		return nil, fmt.Errorf("连不上系统总线：%w", err)
	}
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("系统总线鉴权失败：%w", err)
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("系统总线握手失败：%w", err)
	}

	// Auth 期间协商出来的能力：没有它，logind 那句 Inhibit 的返回值（一个 fd）根本传不过来。
	if !conn.SupportsUnixFDs() {
		_ = conn.Close()
		return nil, fmt.Errorf("系统总线不支持传递文件描述符")
	}

	obj := conn.Object("org.freedesktop.login1", "/org/freedesktop/login1")
	call := obj.Call("org.freedesktop.login1.Manager.Inhibit", 0,
		"idle:sleep", "Tachi", why, "block")
	if call.Err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("logind 不收这条 inhibitor：%w", call.Err)
	}
	var fd dbus.UnixFD
	if err := call.Store(&fd); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("logind 没有给出 inhibitor 的 fd：%w", err)
	}
	return &awakeGrant{conn: conn, fd: fd}, nil
}
