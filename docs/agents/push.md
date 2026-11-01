# Tachi —— 推送手册

把"这一轮结束了 / 出错了 / 在等你"叫到**人手上**的那一层。系统通知（窗口版）管的是"人在电脑旁、看着别的窗口"；推送管的是"人不在电脑旁"。两者是同一个 `notifierHost` 的两个终点，不是两套触发逻辑。

**它是提纲——每条 bullet 是一个不变量。** 机制属于它所讲的代码。

## 目录

| 章节 | 什么时候读 |
| --- | --- |
| **为什么不是自己的 App** | 有人问"为什么不直接给手机 App 加推送"，或想改推送通道 |
| **配置** | 你要加一个渠道、改字段、或在设置页动这一块 |
| **一条通知怎么出去** | 你碰 `notifierHost`、触发点、或"该不该推"的判断 |
| **开关** | 你碰会话记录、手机端点、或 `session_prefs` |
| **点开通知** | 你碰深链、机器 id、或手机切设备那条路 |
| **坑** | 手机不响 / 响得晚 / 响在不该响的时候 |

## 为什么不是自己的 App

- **iOS 的推送要付费开发者账号签发的证书**（免费个人签名 7 天、不带推送能力）；**Android 国内要逐家接厂商通道**（没有谷歌服务就收不到 FCM）。两条都不划算，所以借一个现成的通知 App 当中继：**Bark 给 iOS、ntfy 给 Android**。宿主只发一条 HTTP，证书和通道是那个 App 自己的事。
- 两个 App 都支持**自建服务器**，地址是配置：`push.example.com` 和 `ntfy.sh` 走同一条代码路径。
- 两端各有各的好处，所以两个渠道并存：Bark 走苹果推送（手机不必能连到宿主的服务器，锁屏也秒到）；ntfy 一个 App 带 Android（自建时靠长连接，iOS 上自建要经 ntfy.sh 转发）。

## 配置

```yaml
push:
  enabled: true            # 总开关。默认 false：不配就不推
  bark:
    # url 默认 https://api.day.app（官方服务），自建就换这一个字段
    keys: ["k1", "k2"]     # 每台 iOS 设备一个，一条请求全发
  ntfy:
    # url 默认 https://ntfy.sh（官方服务），自建就换这一个字段
    topic: ""              # 公共服务器的 topic 等于密码
    token: ""              # 可选：服务器开了鉴权才要
```

- **两个 url 的默认值就是官方地址**（`default:` tag）：读者要填的只有自己的 key / 主题。默认地址不等于"配好了"——没有 key（或没有 topic）的渠道不建 sender，什么都不发。
- `config_test.go` 的 `TestPushURLsDefaultToThePublicServices` 钉住这两条默认值，以及"默认地址 + 空 key 仍然不发"。

- **每个叶子都是在发送那一刻现读的**（`senderFor` 从 live config 构建），所以设置页保存即生效，不用重启，也不用重连手机。
- `enabled` 为 true 但某个渠道的地址/主题是空的 = 那个渠道不推；两个都空 = 什么都不发，也不报错。
- 设置页上这三行是 `applyLazy`（`push.enabled` / `push.bark` / `push.ntfy` 各一行，Note 按 block 写——设置表要求一条 note 只能属于一个 block）。
- 无头宿主没有设置页，改 `~/.tachi/config.yaml`。

## 一条通知怎么出去

- **触发点只有一处**：`desktop/notify_host.go` 的 `notifierHost`（回合完成 / 出错 / 等待授权 / 等待回答 / 定时任务完成 / 失败 / 旁路运行完成）。窗口版装的是 `compositeNotifier{ *notifier, *pusher }`，无头版装的是 `*pusher`（`main_headless.go`）。**加一种送达方式 = 加一个 notifierHost 实现，不碰运行时那一行。**
- **措辞在两个宿主之间共用一份**：`desktop/notify_text.go` 没有 build tag，`notify.go`（窗口）和 `push.go`（手机）都从那里取句子。别把文案搬回 `notify.go`。
- **决定"该不该推"的是 `shouldPush`**（`desktop/push.go`），四个问题依次问，任一为真就不推：全局开关关着、这一段自己关了推送、桌面前台正显示着它、**某台手机正订阅着它**。
- **"手机在看"的判据是订阅，不是"App 开着"**：手机进后台会发 `open ""` 退订（`App.tsx` 的 AppState）。没有这一步，揣进口袋的手机还订阅着，回合结束时就不会响——那正是最该响的时刻。
- 发送是 fire-and-forget：自己的 goroutine、`push.DefaultTimeout`、失败只写日志。一轮结束不能因为推送服务没应答而卡住。
- 多台设备天然同时收到：Bark 的 keys 一条请求全发；ntfy 是同一个 topic，所有订阅者都收到。
- 点通知的深链是 `tachi://session/<会话 id>?device=<机器 id>`（`pkg/push.DeepLink`）。

## 开关

- **全局**：`push.enabled`（设置页 / 配置文件）。关掉谁都不推。
- **会话级**：会话记录上的 `NotifyOff`（`session/`），手机在会话信息页关，桌面改的是同一条记录。
  - 端点 `POST /v1/sessions/{id}/notify`，body `{off}`，与 `skip-approval` 同形。
  - 写入方是 `setNotifyOffFor`，成功后广播 `session_prefs`（`provider` / `skipApproval` / `notifyOff` 三个一起发）。
  - **fork 继承，压缩的子会话也继承**——和 `SkipApproval` 相反：用户关掉的是"这段对话别推我"，压缩只是把同一段对话写成两半，在压缩点重新开始推送会让人莫名其妙。
- 两者是"与"：全局关 = 都不推；会话关 = 只这段不推。

## 点开通知

- 两个 App 都支持"点通知打开一个 URL"：Bark 的 `url`、ntfy 的 `click`。URL 用自定义 scheme（`mobile/app.json` 的 `scheme: tachi`），所以**不需要证书、不需要上架**。
- 手机上：`src/api/link.ts` 解析，`mobile.ts` 的 `handleLink` 执行。两条入口——冷启动 `Linking.getInitialURL()`、热启动 `addEventListener('url')`。
- **机器 id 是 `~/.tachi/machine.json` 里的随机值**（`desktop/machine_id.go`，首次用到时生成）。不用 hostname：用户会在手机上改名、hostname 也会变，两台机器同名也不是不可能。它跟着 `/v1/ping` 和配对二维码走；老配对在下次 ping 时学会（`rememberAddress` 只在 ping 说了的时候写）。
- **时序是"先切机器、再开会话"**：切设备会重建 socket，立刻 open 会订阅到旧那一条上。`handleLink` 把目标记进 `pendingOpen`，等 `pairing` 真的换过去之后再由 effect 打开。
- 找不到那台机器时说一句"这条通知来自一台还没在这部手机上配对的电脑"——静默什么也不做，点通知的人只会以为坏了。

## 坑

- **Android 自建 ntfy 收不到即时推送**：自建服务器默认没有 FCM，App 靠长连接；国产系统的省电策略会冻掉它。把 ntfy 加进"不受限制"，或者用 `ntfy.sh`（官方服务器走 FCM）。
- **iOS 自建 ntfy 要能出网到 ntfy.sh**：自建服务器通过 `upstream-base-url`（默认就是它）转发一个"有新消息"的信号，手机收到后回你的服务器取内容——**内容不出你的机器**，但那个信号要出去。
- **Bark 自建不要求手机能连服务器**：bark-server 内置官方 App 的 APNs 证书，推送是"服务器 → 苹果 → 手机"。手机只要在配对那一次连得上（局域网或 tailnet）就行。
- **手机改的开关桌面看不到**：桌面端目前不画 `notifyOff`（协议里有两端，UI 在手机上）。改协议时别顺手加第二套同步路径——`session_prefs` 是唯一那一份。
- **重复的警告不是 bug**：手机订阅着那段对话时，系统通知仍会照常发（那是窗口的规则），推送则被抑制。两条规则各自问"我在不在看"，不要合并。

## 怎么验

- **发送器**：`pkg/push` 的单测用 httptest 钉住请求的形状——Bark 的 `/push` 与一次带全部 keys、ntfy 的 JSON 发布与 Bearer、失败不影响另一个渠道、深链的转义。
- **判断**：`desktop/push_test.go` 钉住 `shouldPush` 的六种组合，以及 `senderFor` 只把「地址和它需要的那一半都填了」的 block 变成渠道。
- **端到端只能手验**（要真的发 HTTP 到第三方，冒烟套件不碰网络）：配好之后 `curl` 两个渠道确认手机真的响；然后开一段对话、把手机切到后台跑一轮。**"不推"的四种情况也要各做一次**——关全局、关这一段、桌面窗口前台显示这一段、手机停在对话里。
- 手机端能自动验的是解析那一层：`npx tsc --noEmit` + `npm test`（`src/api/link.test.ts` 钉住深链的解析）。
