package main

import (
	"context"

	"github.com/monsterxx03/tachi/desktop/link"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// 设备（badge）对主窗口的两个遥控动作：翻页，和把它换到前台。
//
// 设备不知道窗口在哪里、也不知道「翻一屏」是多大——它只报手势（见 link.Key），而这两个动作
// 一个属于窗口的排版（滚动位置在 DOM 里），一个属于 AppKit。所以它们是这个文件唯一的住户。
// chatScrollEventName 是 Go→前端：把会话区往上/下翻一段（{delta}，>0 往下）。
//
// 之所以是一条事件而不是直接摆弄窗口，是因为滚动只有页面知道：会话区多高、现在停在哪、
// 用户是不是还在「跟随最新」，全是 DOM 里的事实（见 App.tsx 里订阅它的那段）。
const chatScrollEventName = "chat:scroll"

// remoteLog 是这条路径的日志出口。与 badge 那条同理走 pkg/logger：app 从 Finder 起来时
// stderr 没有看客，而「设备上按了、主机这边到底动没动」只有这几行能回答。
func remoteLog() *logger.Logger { return logger.New("desktop") }

// scrollWindow 让主窗口的会话区翻一下：direction > 0 往下读，< 0 往回读。
//
// 它与窗口里的 Ctrl+U / Ctrl+D 是**同一个动作**（前端那边就是同一段代码）：设备上那两下
// 按键在人心里就是「翻页」，两个入口一个行为，而不是两套滚动。
func (d *desktopApp) scrollWindow(direction int) {
	if d.app == nil {
		return
	}
	d.app.Event.Emit(chatScrollEventName, map[string]any{"delta": direction})
}

// activateWindow 把主窗口换到前台。
//
// 设备上双击确定问的是「它刚才说了什么」，而那句话现在只在窗口里——所以这一下要做的就是把人
// 送回去。Show + Focus 与托盘菜单的「打开主窗口」、通知被点开时是同一对调用（窗口被 ⌘W 收起
// 来时 Show 让它重新出现，见 main.go 的 WindowClosing 钩子）。
//
// 它**不**看窗口现在是不是已经在前台：那会让「按了没反应」变成一种需要解释的状态，而一次
// 多余的 Show/Focus 是无害的。
func (d *desktopApp) activateWindow() {
	if d.window == nil {
		return
	}
	d.window.Show()
	d.window.Focus()
	remoteLog().Info(context.Background(), "badge: 主窗口切到前台")
}

// Key 实现 link.Source：设备送回来的一个手势。
//
// 三个动作的含义全部在这里决定，设备那边只是「你按了哪一下」——这是整条链路一贯的分工：
// 语义留在主机。
//
//	up / down   窗口里的会话往上/下翻半屏（Ctrl+U / Ctrl+D）
//	activate    主窗口换到前台
//
// 不认识的 action 直接忽略：设备的手势将来可能多一个，而一个没实现的动作用户按下去什么都
// 不发生，比让链路变得脆弱要好（见 link.Server.handle 的 key 分支）。
func (d *desktopApp) Key(k link.Key) error {
	if k.Action == link.KeyActivate {
		d.activateWindow()
		return nil
	}
	if delta, ok := scrollDirection(k.Action); ok {
		d.scrollWindow(delta)
	}
	return nil
}

// scrollDirection 把设备的手势翻译成滚动方向：>0 往下读，<0 往回读。第二个返回值是「这是不是
// 一个翻页手势」——activate 不是，没见过的动作也不是（按下去什么都不发生）。
//
// 单独抽出来，是因为方向反了在屏幕上看着像「按键坏了」：按上却往下跑，而这条链路上没有任何
// 东西会报错。
func scrollDirection(action string) (int, bool) {
	switch action {
	case link.KeyUp:
		return -1, true
	case link.KeyDown:
		return 1, true
	}
	return 0, false
}
