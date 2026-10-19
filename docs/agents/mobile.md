# Tachi —— 移动端手册

`mobile/` 是手机 App（Expo / React Native + TypeScript）。**它只有界面**：会话、工具、审批都在桌面进程里跑，手机连的是那台开着的电脑。桌面那半边（`desktop/mobile*.go`、那个移动接口）见本手册末尾。

`mobile/AGENTS.md` 是 Expo 模板带的、写给写代码的 agent 的通用建议（跟着 SDK 更新，值得读）。它与本文冲突时以本文为准——本文说的是这个项目怎么跑。

**它是提纲——每条 bullet 是一个不变量。** 机制属于它所讲的代码与配置（`mobile/app.json`、`mobile/scripts/`、`~/.gradle/init.gradle`），不在本文里复述。

## 目录

| 章节 | 什么时候读 |
| --- | --- |
| **协议** | 你要加一个端点、一类事件，或改一个字段——**读 [mobile-protocol.md](mobile-protocol.md)，那是这一层的唯一出处** |
| **新机器上的环境** | 在一台没有 Android 工具链的机器上第一次碰 `mobile/`——**照这一节做，不要照网上的教程** |
| **国内镜像** | 某个下载慢到以为卡住时；或者新机器上要重新配一遍 |
| **构建与运行** | 你要构建、安装、跑这个 App；或者 prebuild 之后构建失败 |
| **验证** | 你想知道一个改动该拿什么证明它是对的、agent 自己验得到哪一层 |
| **服务端** | 你碰桌面那半边（`desktop/mobile*.go`）或手机要用的那几个桌面方法 |

## 新机器上的环境

**最短路径**（照这个顺序，细节在下面）：

```sh
# 1. 工具链：JDK 17、Node ≥26、Android SDK（组件清单见下）、一个 AVD
# 2. 装依赖
cd mobile && npm install

# 3. 生成原生工程（生成物，不入库）
npx expo prebuild --platform android --no-install
bash scripts/prepare-android.sh        # 钉 Gradle 版本、镜像、AGP —— 每次 prebuild 后都要跑

# 4. 构建并装进模拟器
cd android && ./gradlew assembleDebug
adb install -r app/build/outputs/apk/debug/app-debug.apk
cd ..

# 5. 跑起来：一个终端起 Metro，一个终端起假服务端
npx expo start                         # 另开：adb reverse tcp:8081 tcp:8081
go run ./mockapi -token dev-token      # 它会把「局域网地址 + 令牌」打出来
```

打开 App，在配对页填/扫那个地址和令牌，就能看到会话列表。验证：`npm test`（纯逻辑）、`bash e2e/run.sh`（端到端）。

工具链：**JDK 17**（Temurin）、**Node 26**、**Android SDK**（`~/Library/Android/sdk`）。

- **JDK 必须是 17。** 这台机器上可能还装着更老的（`java -version` 报 1.8 是常见的），所以一律显式指定 `JAVA_HOME=$(/usr/libexec/java_home -v 17)`。macOS 只认 `~/Library/Java/JavaVirtualMachines/<名>.jdk/Contents/Home` 这个布局，tar.gz 解压出来要按它摆——而且**摆的是整棵 `Contents/`**（`Info.plist` 在内）：只拷 `Home/` 的话 `java -version` 照样能跑，但 `/usr/libexec/java_home -v 17` 看不见它，只会回落到 1.8。
- **Node 不能是 20。** React Native 0.88 的 engines 是 `^22.13 || ^24.3 || >=26`；用 20 会在装依赖时以 `EBADENGINE` 起步，出问题的位置和 Node 毫无关系。本机 homebrew 的 26 就够用。
- **`ANDROID_HOME=~/Library/Android/sdk`。**

SDK 里放什么，分两类：

| 手动装（Gradle 不会替你装） | 目录 |
| --- | --- |
| 命令行工具 | `cmdline-tools/latest/`（`sdkmanager` / `avdmanager`；新版提示 sdkmanager 已改名 `android`，老的还能用） |
| adb | `platform-tools/` |
| 模拟器与系统镜像 | `emulator/`、`system-images/android-36/google_apis/arm64-v8a/`（Apple Silicon 必须用 **arm64-v8a** 镜像） |

**其余（platform、build-tools、NDK）交给 Gradle 自己补**：它按项目实际要求装，实测会拉 `platforms/android-37.0`、`build-tools/37.0.0`、`ndk/27.1.12297006`——手动装一份 36 反而白装。耗时取决于那台机器的网络。若 `sdkmanager` 和 Gradle 的自补撞在一起，SDK 下会多出个 `platform-tools-2/`，`avdmanager` 每次都警告「inconsistent location」——那是重复的一份，删掉即可，`platform-tools/` 才是在用的。

两个手工步骤，都是"包装里没有、而工具要求"的东西：

- **`emulator/` 要补一个 `package.xml`**，否则 `avdmanager create avd` 直接报 `"emulator" package must be installed!`（它靠这个文件认已安装的包，Google 的 zip 里不带）。照着系统镜像目录里那份 `package.xml` 仿写即可。**用 `sdkmanager --install emulator` 装的不用这一条**——它自己写这个文件，只有手动解压 zip 才要补。
- **模型/镜像放好后，`avdmanager` 才认得**：`avdmanager create avd -n tachi-phone -k "system-images;android-36;google_apis;arm64-v8a" -d pixel_8`（平板用 `-d pixel_tablet`）。

启动模拟器：`"$ANDROID_HOME/emulator/emulator" -avd tachi-phone -gpu host &`，冷启动要一两分钟，等 `adb devices` 里出现 `device`、`adb shell getprop sys.boot_completed` 回 `1`。

- **新建的 AVD 默认配置会把 SystemUI 拖到反复 ANR。** 默认是 `hw.gpu.enabled=no` + `hw.ramSize=2G`，宿主一有负载，模拟器就隔一会儿弹一次「System UI isn't responding」——它挡住界面，看着像 App 崩了，其实 App 在底下正常渲染。把 `~/.android/avd/<名>.avd/config.ini` 里 `hw.gpu.mode` 改成 `host`、`hw.ramSize` 提到 `4G`，启动时带 `-gpu host`，就没有了。真遇到又不想改配置，点 Wait 也能继续用。
- **配对走 `adb reverse`，别用宿主的局域网 IP**：`adb reverse tcp:8787 tcp:8787`，配对页地址写 `http://127.0.0.1:8787`、令牌就是 `go run ./mockapi -token` 那个。模拟器绕去宿主 IP 是否通取决于 NAT，回环这条一定通。Metro 的 `8081` 同理。

**Maestro**（跑端到端流程用）不是包管理器装的：把官方的 `maestro.zip` 解到 `~/.local/maestro` 就有可执行的 `bin/maestro`（它只是几个 jar 加一个脚本；跑的时候 `JAVA_HOME` 要指向 17）。

**Gradle 的镜像**（可选）：想要阿里云那套就 `cp mobile/scripts/gradle-init.gradle ~/.gradle/init.gradle`——它必须放在仓库外，因为 `android/` 是生成物。网络能直通海外的话不配也行。

## 国内镜像

| 什么 | 走哪 | 配在哪 |
| --- | --- | --- |
| npm 包 | `registry.npmmirror.com` | `mobile/.npmrc`（**项目级**，不动用户的全局配置） |
| Gradle 构建的 maven 依赖 | 阿里云 `public` + `google` | `~/.gradle/init.gradle`——**必须在仓库外**：`android/` 是生成物，写进去会被 prebuild 冲掉 |
| Gradle 发行版（wrapper 拉的 zip） | 腾讯 `mirrors.cloud.tencent.com/gradle/` | `mobile/scripts/prepare-android.sh`（同上，每次 prebuild 后重跑） |
| Android SDK 各组件 | 腾讯 `mirrors.cloud.tencent.com/AndroidSDK/` | 手动下 + 解压 |
| JDK / Node 的 tar.gz | 清华（Adoptium / nodejs-release） | 手动下 |

**镜像不是必需品，是加速**：网络能直通海外就直连；国内镜像胜在稳和快（同一个文件，镜像 13 MB/s、直连 `dl.google.com` 0.4 MB/s），所以 SDK 组件宁可手动下——`sdkmanager` 走的还是 `dl.google.com`。

- **Gradle 用的镜像一定要验 sha256**。腾讯镜像是 `dl.google.com/android/repository` 的投影，目录层级一致（系统镜像在 `sys-img/google_apis/` 下），文件名要从 `repository2-3.xml` / `sys-img*/sys-img2-3.xml` 里取；**每个包在索引里有 Linux / macOS / Windows 好几份 zip，取第一个会拿到 Linux 的**（按 `host-os=macosx`、`host-arch=aarch64` 过滤）。下载完拿索引里的 sha1 核对一遍，包对不上会以完全无关的方式炸。
- **压缩包内的目录名 ≠ 安装目录名**：platform 包里是 `android-36/`、build-tools 包里是 `android-16/`（Android 16 = API 36），要分别落成 `platforms/android-36/`、`build-tools/36.0.0/`。
- Gradle 发行版的缓存目录名是 **distributionUrl 的 MD5 的 base36**。想跳过 wrapper 自己下载（速度差一个量级），算好这个名字把 zip 直接放进去即可。
- **`mobile/.npmrc` 把 npm 指到了淘宝镜像**。那台机器如果直通海外、走镜像反而慢，删掉这个文件就用官方源——它只影响这个项目，不碰全局配置。
- **这台机器上另有一份 `~/.gradle/init.gradle`**（仓库里的模板是 `mobile/scripts/gradle-init.gradle`）：它把官方仓库换成阿里云镜像。新机器上不是必需的，想要镜像就 `cp mobile/scripts/gradle-init.gradle ~/.gradle/init.gradle`。

## 构建与运行

```sh
cd mobile
npm install                        # 走 .npmrc 里的淘宝镜像
npx expo prebuild --platform android --no-install
bash scripts/prepare-android.sh    # 必须：android/ 是生成物，见下
cd android && ./gradlew assembleDebug
adb install -r app/build/outputs/apk/debug/app-debug.apk
adb reverse tcp:8081 tcp:8081 && npx expo start   # Metro；dev 包要靠它加载 JS
```

- **`android/` 是生成目录，不入库**（Expo 的 CNG）。任何"改生成物"的动作都要么写进 `app.json` / config plugin，要么写进 `scripts/prepare-android.sh`——手改 `android/` 里的文件，下一次 prebuild 就没了。那个脚本现在钉两件事：**Gradle 版本 9.5.1** 和第三方库请求的老 AGP。
- **Gradle 版本别动。** 模板给的 9.4.1 会在编译**纯 Java** 的 Android 库时必挂，报的是那个库的名字：
  `:react-native-webview:compileDebugJavaWithJavac > ClassNotFoundException: org.gradle.internal.compiler.java.IncrementalCompileTask`。
  这不是库坏了——9.4.1 的 `JdkTools` 去加载一个它自己已经删掉的包，而 Expo 自己的模块是 Kotlin、不走这条路径，所以只有第三方 Java 库会炸。9.5.1 修好了它；9.8.1 又会让 Expo 的 gradle 插件 Kotlin 编译失败。**换库、统一 AGP、关 fork 都试过，全无效**——那一串实验值多少钱就看这一条。
- 两个配置**不要**回退成 app.json 里的裸字段（它们在 SDK 58 不生效）：允许明文 HTTP 要用 `expo-build-properties`，深浅色跟随系统要装 `expo-system-ui`。验证方式：生成出来的 `android/app/src/main/AndroidManifest.xml` 里应有 `android:usesCleartextTraffic="true"`。
- 构建产物：`android/app/build/outputs/apk/debug/app-debug.apk`。
- **停 mock 服务别用 `lsof -ti:8787`**：那个命令列出的是**所有碰过这个端口**的进程，包括**连着它的模拟器** —— 它会把模拟器一起杀掉（表现是模拟器莫名其妙退出，系统崩溃报告里是 `qemu-system-aarch64` 的 SIGABRT）。用 `lsof -ti:8787 -sTCP:LISTEN` 只挑监听的那一个，或者干脆按名字 `pkill -f mockapi`。
- **写界面要用 `react-native-safe-area-context` 的 `SafeAreaView`**，RN 自带的那个已弃用（运行时会弹警告条）。
- **mermaid 随包发**（`assets/mermaid.min.txt`，5 MB 全量构建）。它停在 `assets/` 而不是模块里：`metro.config.js` 把 `txt` 加进 `assetExts`，否则 Metro 会把它当 JS 模块去执行。运行时用 `expo-asset` + `expo-file-system` 读成字符串注进 WebView——这样第一次画图也不依赖网络。

## 验证

按能不能自己跑完分层，**越往下越需要人**：

| 层 | 怎么验 | 谁验 |
| --- | --- | --- |
| 协议与桌面那半边 | Go 测试（`httptest` 打接口）+ 冒烟场景 `mobile-api`（真进程上打 HTTP/WebSocket，`make desktop-smoke ARGS="-run mobile-api -v"`） | 全自动 |
| 手机端纯逻辑 | `npx tsc --noEmit` + `npm test`（jest-expo；`src/state/chat.test.ts` 钉的是事件归并和错误文案那层纯函数） | 全自动 |
| 手机端界面 | **web 目标**：`npm run web`，浏览器里自己点、截图、看 console 与网络 | 全自动（agent 用浏览器工具） |
| 模拟器上的真界面 | `adb exec-out screencap -p > x.png` 看一眼；改代码后 Metro 会热重载，不用重装 | 全自动 |
| 触摸交互、扫码、手感、锁屏重连 | Maestro 驱动点击（`~/.local/maestro/bin/maestro`，装在仓库外） | 半自动 |
| 真机验收 | 只能人看 | 人 |

- **写界面时保持它能跑在 web 上**——那是 agent 能自己看见界面的另一条路。代价是原生模块要有一层平台适配（安全存储在 web 上退化成 localStorage），这层本来也该有。
- **「手机 + 真桌面」的联调不碰你自己的应用**：造一个隔离的 HOME——`config.yaml` 里 `mobile.enabled: true`，一个 `mobile.json` 写着想要的 token（`0600`），再在 `~/.tachi/session/<id>/` 手写一份 `meta.json` + `messages.jsonl`——然后 `open -n --env HOME=<sandbox> desktop/bin/tachi-desktop.app` 起第二个实例。手机端照旧 `adb reverse tcp:8787 tcp:8787` 连 `127.0.0.1:8787`。收工只杀那一个：从 `ps eww` 按 `HOME=` 认 pid，**别对整个 bundle `pkill`**（会命中用户自己那份）。没有配 provider 时发送会被 409 `agent_unavailable` 挡下，那正是错误透传那条路的现成用例（手机应显示「这台电脑还没配好模型，先在电脑上配置」，输入框里的字要留着）。
- **端到端用 `mobile/e2e/`**：`bash mobile/e2e/run.sh`（它先把 mockapi 重启成种子状态，再让 Maestro 跑 `main.yaml`）。两个踩过的点：**Maestro 的文本匹配是精确相等、不是包含**（要包含就写成 `".*命令跑完了.*"`）；flow 会消耗种子数据（点掉那张待答卡），所以每次跑之前都得重置 mock 服务 —— 脚本做这件事。
- **模拟器扫不了码**：它的相机没有真实画面，所以扫码这条路只能验到"界面能打开、权限流程正常"，二维码的解析由 `src/api/qr.test.ts` 钉住（码里装的是 `{"v":1,"url":"…","token":"…"}`）。
- 桌面的会话记录是权威，实时流只延长尾巴；验界面时以"重读快照后画面正确"为准，不要只信流。

## 服务端

`desktop/mobile*.go`，package `main`，随桌面进程起落（`main.go` 的 `startMobile`）；配置块 `mobile.{enabled,listen,max_attachment_mb}`，监听地址默认 `0.0.0.0:8787`。**端点、事件、字段的出处是 [mobile-protocol.md](mobile-protocol.md)**，这一节只说那些不做就会静默坏掉的地方。

- **令牌是凭据，不是配置**：`~/.tachi/mobile.json`（`0600`，首次配对时生成），不进 `config.yaml`；比较用 `crypto/subtle`。`RegenerateMobileToken` 换值**并且断开每一条活连接**——留着旧 socket，等于没作废。
- **手机发消息前必须走 `loadSessionForRemote`**（`prepareSession` + `LoadSessionHistory`，**不写 `activeID`、不发状态事件**）。`startTurn` 读的是 `r.history`，而 `prepareSession` 不恢复它：一段桌面没打开过的会话，手机发的第一句会让模型忘掉之前的一切，而界面看起来完全正常。
- **按会话寻址的三件事各有一份实现**（`switchProviderFor` / `skipApprovalFor` / `setSkipApprovalFor`）：桌面原有的那三个只认 `currentID`，手机直接调会改到窗口正看着的那一段。`setSkipApprovalFor` 先写记录再翻 live agent，且**没有 live run 时只写记录**——一个开关不值得建一个 agent。
- **三种发送的拒绝各是自己的码**：`turn_running` / `not_running` / `steer_unsupported`。定时任务轮没有 steer 通道，它不是「没在跑」。
- **线上只走投影**（`mobile_events.go`）：`Messages` 与完整工具输出不上线，工具结果截到 2 KiB，旁路运行（`/review`、`/commit`）与对话一样被 lane 挡住。
- **背压是丢连接，不是等**：每条连接 256 条事件的队列，满了发 `lagged` 再关；最多 16 条连接。`ask_answered` 是唯一的广播（它可能属于任何会话），其余只发给订阅了那个会话的连接。
- **附件只认 `SendFile` 的记录**：`attachmentId` 就是那次工具调用的 id，下载端点从不接受路径；相对路径按**那次调用所属会话**的根目录解析。
- **两个会话开关也是共享状态**（模型 `provider`、`skipApproval`）：写入方是 `switchProviderFor` / `setSkipApprovalFor`，它们在写完之后发一条 `agent:session_prefs` + 手机的 `session_prefs`，两个值一起走。所以桌面改的手机立刻看见、手机改的桌面立刻看见；事件没有 origin——它是幂等状态，谁改的都要落到每一块屏。窗口的控件与手机的模型表/审批开关都走这两个写入方，不存在第二条广播路径。冒烟：`mobile-prefs`。
- **一块屏发的消息要回声**：`echoUserMessage` 同时发 `agent:user_message`（桌面）和 `user_message`（手机），带 `origin`；收到自己那条的不再画。窗口自己画的用户气泡不来自事件，少了这一条，手机发的消息会显示成「对一句话的回复，但那句话不在」。桌面那一半的监听在 `agentEvents.ts`（`useAgentStream`）：非 `desktop` 的 origin 追加气泡，`steer` 的走 `injectSteerVisual`（切开正在流的那一段）。**这个回声没有冒烟场景**——驱动没法扮演另一块屏；改它只能靠单测与手验。
- **配对卡不是配置叶子**：`SettingsSchemaVO.MobilePairing` 只回答「画在哪个分组」（`catExtend`），卡片自己的数据来自 `GetMobilePairing` 这个绑定，二维码渲染的是后端给的 `payload`（手机端 `qr.ts` 解析同一个形状，两边不得各拼一份）。令牌默认掩码，`RegenerateMobileToken` 返回 `ok` 或原因，卡片换新后重新读一次。
