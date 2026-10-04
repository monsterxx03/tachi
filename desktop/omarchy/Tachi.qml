import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui

// Tachi Desktop 在 bar 上的读数：运行状态、今日花费、上下文占用。
//
// 数据不是问出来的，是 Tachi 自己写的：每次状态变化（回合开始、每次工具调用、结束……）它
// 都会重画托盘，并把同一份读数落到 ~/.tachi/desktop_status.json（字段见 Tachi 源码
// desktop/barstatus.go）。那个托盘在 Linux 上没有文字位——SNI 只有图标、tooltip 和菜单——
// 所以这些字只有落在这根 bar 上才能一直看得见。文件不在（Tachi 没在跑）时本控件自己消失。
//
// 排版跟托盘自己那份保持一致（desktop/tray.go 的 formatCost / trayContext）：花费是三位
// 小数的 ¥，上下文是整数百分比，两样都在量不出来时干脆不写——宁可少一个数，不编一个。
BarWidget {
  id: root
  moduleName: "monsterxx03.tachi"

  property var status: null

  // 上下文占到多少就把那个数字标红。花掉的钱是既成事实，永远用前景色；上下文快满了才是
  // 要动手的信号，所以变色只发生在这一个数字上。
  readonly property real alertPercent: 70

  // tooltip 里的 detail 可能是一整条命令（Bash 调用就是这样），而 bar 的 tooltip 是按内容
  // 撑开的弹窗——不截断就会糊满屏幕。80 个字符，和桌面端通知里那条命令的额度一样
  // （desktop/notify.go 的 notifyPermBodyMaxRune）：认出一条命令靠的是开头，不是结尾。
  readonly property int detailMaxChars: 80

  readonly property string statusPath: Quickshell.env("HOME") + "/.tachi/desktop_status.json"

  readonly property real cost: status && typeof status.cost === "number" ? status.cost : 0
  readonly property int percent: status && typeof status.contextPercent === "number" ? status.contextPercent : -1
  readonly property string stateWord: status && status.label ? String(status.label) : ""
  readonly property string detail: status && status.detail ? String(status.detail) : ""
  readonly property string stateStatus: status && status.status ? String(status.status) : ""
  readonly property bool alert: percent >= alertPercent

  // 和 Tachi 源码里 AgentStatus.busy() 是同一组：有回合在跑。空闲和出错之外都算「在干活」。
  readonly property bool working: ["thinking", "tool_running", "busy"].indexOf(stateStatus) >= 0
  readonly property bool failed: stateStatus === "error"

  // 字体的来源和 WidgetButton 一致（它就是 bar 上写字的那个部件）：本控件要自己排版只是
  // 因为一个 WidgetButton 只能有一个颜色。
  readonly property string fontFamily: bar ? bar.fontFamily : Style.font.family
  readonly property color foreground: bar ? bar.barForeground : Color.foreground
  readonly property color alertColor: bar ? bar.urgent : Color.urgent
  readonly property color workingColor: Color.accent

  // 状态词的三种颜色：出错是红，在干活是主题的 accent，空闲压暗一点（它一直在那儿，但不该
  // 跟数字抢注意力）。
  readonly property color stateColor: failed ? alertColor : (working ? workingColor : foreground)
  readonly property real stateOpacity: failed || working ? 1.0 : 0.65

  function costText() {
    return cost > 0 ? "¥" + cost.toFixed(3) : ""
  }

  function contextText() {
    return percent >= 0 ? percent + "%" : ""
  }

  function hasNumbers() {
    return costText() !== "" || contextText() !== ""
  }

  // 两个数字拼一段（各自量不出来就不写这一段）。
  function numbersText() {
    var parts = []
    if (costText() !== "") parts.push(costText())
    if (contextText() !== "") parts.push(contextText())
    return parts.join(" · ")
  }

  // 状态词在前、数字在后（状态是「它在干什么」，一眼要看到的是这个；数字是细节）。全都没有
  // 时就只剩状态词——bar 上永远有东西可读，而不是空一格。
  function readout() {
    var parts = []
    if (stateWord !== "") parts.push(stateWord)
    if (hasNumbers()) parts.push(numbersText())
    return parts.join(" · ")
  }

  // tooltip 里状态词和数字仍然是分开的（中间好放 detail），所以它不复用 readout()。
  function tooltip() {
    if (!status) return "Tachi Desktop 未在运行"
    var parts = ["Tachi"]
    if (stateWord) parts.push(stateWord)
    if (numbersText()) parts.push(numbersText())
    if (detail) parts.push(truncatedDetail())
    return parts.join(" · ")
  }

  function truncatedDetail() {
    var chars = Array.from(detail)
    return chars.length > detailMaxChars ? chars.slice(0, detailMaxChars).join("") + "…" : detail
  }

  // 手动刷新：不用等下一个 tick（刚改过文件、或者就想立刻看一眼）。
  IpcHandler {
    target: "monsterxx03.tachi"

    function refresh(): void {
      root.broadcast("refresh")
    }
  }

  function refresh() {
    statusFile.reload()
  }

  visible: status !== null
  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  FileView {
    id: statusFile
    path: root.statusPath
    printErrors: false
    onLoaded: root.apply(text())
    onLoadFailed: root.status = null
  }

  function apply(content) {
    try {
      root.status = JSON.parse(String(content || "null"))
    } catch (e) {
      root.status = null
    }
  }

  // 轮询，不用 watchChanges：那份文件是「临时文件 + rename」原子替换写的，每次写入之后
  // 路径上的 inode 都换一个，而文件监视器认的是它打开时那一个——换成新的还报不报得动，
  // 是监视器的实现细节，不是这里的契约。一秒读一个几百字节的文件买不起那个不确定性。
  //
  // Tachi 每次状态变化都会重写它，所以这一秒的延迟就是全部的延迟：回合开始、每次工具
  // 调用、结束，bar 上最多晚一秒。
  Timer {
    interval: 1000
    running: true
    repeat: true
    triggeredOnStart: true
    onTriggered: statusFile.reload()
  }

  // 几段字颜色/透明度各不相同，所以不是 WidgetButton 的一个 text（一个部件只有一个颜色）。
  // tooltip 仍走 bar 自己那套 —— 和 WidgetButton 内部调的是同一个 bar.showTooltip。
  Item {
    id: button
    anchors.fill: parent
    implicitWidth: root.vertical ? root.barSize : row.implicitWidth + Style.spaceReal(6) * 2
    implicitHeight: root.vertical ? row.implicitHeight + Style.spaceReal(6) * 2 : root.barSize

    Row {
      id: row
      anchors.centerIn: parent
      spacing: 0

      Part {
        text: root.stateWord
        color: root.stateColor
        opacity: root.stateOpacity

        Behavior on color {
          ColorAnimation { duration: 160 }
        }
      }

      // 状态词和数字之间的分隔符：两边都有才画。
      Part {
        text: " · "
        visible: root.stateWord !== "" && root.hasNumbers()
      }

      Part { text: root.costText() }

      // 两个数字之间的分隔符，同样只在都在的时候画。
      Part {
        text: " · "
        visible: root.costText() !== "" && root.contextText() !== ""
      }

      Part {
        text: root.contextText()
        color: root.alert ? root.alertColor : root.foreground

        Behavior on color {
          ColorAnimation { duration: 160 }
        }
      }
    }

    MouseArea {
      anchors.fill: parent
      hoverEnabled: true
      cursorShape: Qt.ArrowCursor
      onEntered: if (root.bar) root.bar.showTooltip(button, root.tooltip())
      onExited: if (root.bar) root.bar.hideTooltip(button)
    }
  }

  // bar 上的一段字。默认前景色、空文本就不占位，几处一样的属性只写一遍。
  component Part: Text {
    color: root.foreground
    font.family: root.fontFamily
    font.pixelSize: Style.font.caption
    renderType: Text.NativeRendering
    visible: text !== ""
  }
}
