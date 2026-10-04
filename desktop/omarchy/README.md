# Tachi 的 omarchy bar 控件

在 omarchy 的 bar 上显示 Tachi Desktop 的运行状态、今日花费和上下文占用：

```
空闲 · ¥10.737 · 45%
执行 Bash · ¥10.737 · 45%
```

（状态词在干活时用主题色、出错用红、空闲压暗；跑工具时后面跟工具名。）

数据不是问出来的：Tachi Desktop 每次状态变化（回合开始、每次工具调用、结束、出错）都会把同一份
读数写到 `~/.tachi/desktop_status.json`，这个控件每秒读一次（字段见 `desktop/barstatus.go`）。
Tachi 没在跑（文件不在）时控件自己消失，不留空槽。

## 装

```bash
make -C desktop bar-widget      # 拷到 ~/.config/omarchy/plugins/monsterxx03.tachi/
omarchy restart shell           # 见下
omarchy bar put monsterxx03.tachi --after omarchy.agents   # 只在第一次要放位置
```

**`omarchy restart shell` 是必须的**，不是保险：往 bar 上新增控件、或者改了控件的 QML，都不会
自动生效 —— 注册表会重扫，但同一个文件路径的 Component 会被复用（改文件的热重载对 bar 控件不
成立）。装/改完重启一次 shell 即可。

## 改

**改这个目录里的文件，再跑一次 `make -C desktop bar-widget`** —— 仓库这份是母本，装到
`~/.config/omarchy/plugins/` 的是拷贝；直接改那一份会在下次安装时被覆盖掉。

几个常改的点（都在 `Tachi.qml`）：

| 想改什么 | 改哪 |
| --- | --- |
| 上下文多少算"快满了"（超过就变红） | `alertPercent`（默认 70） |
| 空闲时的状态词（默认压暗 65%，设 0 就不显示） | `stateOpacity` |
| tooltip 里 detail 的截断长度 | `detailMaxChars`（默认 80） |
| 读的是哪个文件 | `statusPath` |

"执行"后面那个工具名（`执行 grep`）不在状态文件里，是从 `detail` 的头一段抽的 —— 桌面端的
`toolDetail`（`desktop/agent_turn.go`）写的是 `调用 <工具>：<参数>`，控件用
`/^调用\s+([^：\s]+)/` 抓第一个词。**改了那边的措辞，这里就抽不到**：那时 bar 上只剩“执行”，
不会显示垃圾（模拟模式那句"调用工具 grep"正是被这个 `\s+` 挡在外面的）。要改抓法就改
`Tachi.qml` 里的 `toolName()`。

## 为什么它不跟着 Tachi 一起发

它跑在 omarchy 的 `omarchy-shell` 进程里（第三方插件都是），而 Tachi 不该往别人的配置目录里写
东西 —— 装不装、装哪儿是用户的选择。所以仓库只提供母本和一条安装命令。
