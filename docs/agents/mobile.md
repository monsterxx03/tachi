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
| **iOS（真机）** | 你要在 iPhone 上自己装一份（不分发）：工具链、`app.json` 的 iOS 三项、以及本地网络权限这类只在真机上出现的坑 |
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

工具链：**JDK 17**（Temurin）、**Node 26**、**Android SDK**（`~/Library/Android/sdk`）。构建 `android/` 那一步还要 `ANDROID_HOME`，少了它 Gradle 以「SDK location not found」失败。

- **`babel.config.js` 必须在**（`presets: ['babel-preset-expo']`）。reanimated 4 的 worklet 全靠 babel 插件编译：`babel-preset-expo` 见到 `react-native-worklets` 会自动加上那个插件，而**没有这份配置就没人跑 babel**——App 起得来，却在加载阶段崩在 `Cannot read property 'bytecode' of undefined`（`installUnpackers` 读的就是 worklet 函数的 `__initData`）。`babel-preset-expo` 本身也必须在顶层 `node_modules`，光是嵌在 `expo/node_modules` 里不够（`presets: ['babel-preset-expo']` 从项目根解析）。同理，`react-native-gesture-handler` / `react-native-reanimated` 从 `node_modules` 里消失时 `tsc` 会先报一串 `Cannot find module`，而**原生模块是编在 APK 里的**——补装完 JS 包之后必须重新编译一次 APK，否则 Metro 能加载 JS，运行时仍报 `RNGestureHandlerModule could not be found`。

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
- **配对走 `adb reverse`，别用宿主的局域网 IP**：`adb reverse tcp:9797 tcp:9797`，配对页地址写 `http://127.0.0.1:9797`、令牌就是 `go run ./mockapi -token` 那个。模拟器绕去宿主 IP 是否通取决于 NAT，回环这条一定通。Metro 的 `8081` 同理。**mock 服务占 `9797`，真桌面占 `8787`**——两者可以同时开着，不必为了联调停掉另一个。

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
- **改了 `assets/icon.png` 之后必须重跑 `npx expo prebuild --platform android --no-install`**：图标是 prebuild 从 `app.json` 的 `icon` / `android.adaptiveIcon` 生成到 `android/app/src/main/res/mipmap-*` 的，换源文件不重新生成，装进去的还是旧图标（`android/` 是生成物，见上）。重跑后记得再跑 `scripts/prepare-android.sh`。
- **release 包的 R8 会剪掉 ML Kit 的组件构造函数，表现是扫码永远不回调。** ML Kit 靠 `ComponentDiscovery` 反射 `new` 自己的 registrar，R8 看不到这次反射：
  `W ComponentDiscovery: Could not instantiate com.google.mlkit.vision.barcode.internal.BarcodeRegistrar` /
  `Caused by: java.lang.NoSuchMethodException: …BarcodeRegistrar.<init>`。
  debug 包不裁剪，所以只有 release 坏，相机预览完全正常，很容易误判成「二维码没对准」。keep 规则由 `scripts/prepare-android.sh` 写进生成的 `proguard-rules.pro`（和 Gradle 版本同一处理方式）。排查同一类问题：`adb logcat | grep ComponentDiscovery`。
- 构建产物：`android/app/build/outputs/apk/debug/app-debug.apk`。
- **只给一台真机装的时候，别打四合一包**：默认 `assembleDebug` 会把 arm64-v8a / armeabi-v7a / x86 / x86_64 四份原生库都塞进去（约 152 MB，APK 178 MB，其中三分之二是这台机器用不上的）。真机是 arm64，用 `./gradlew assembleDebug -PreactNativeArchitectures=arm64-v8a`，包降到 70 MB 上下；模拟器要 x86_64，分开构建。**别改 `android/gradle.properties` 钉这个**——它属于生成目录，下一次 prebuild 就没了。
- **真机优先走无线调试，USB 不一定通**（常见于线只供电、或平板插在集线器上）：设置 → 开发者选项 → 无线调试 → 「使用配对码配对设备」，然后 `adb pair <ip>:<配对端口>` 填码。**配对端口不是连接端口**——配对成功后用 `adb mdns services` 找到 `_adb-tls-connect._tcp` 那条的端口再 `adb connect <ip>:<端口>`；直接试 5555 会 Connection refused。之后照旧 `adb reverse tcp:8081 tcp:8081`（无线 adb 上也有效），`adb -s <ip:端口>` 指定设备。
- **dev 包启动后白屏、随即退回桌面不是崩了**：debug APK 里没有 JS bundle，全靠 Metro。先确认 8081 上已经有 Metro（可能用户自己开着，别重复起），`adb reverse tcp:8081 tcp:8081`，然后 `am force-stop` + `am start -n dev.tachi.mobile/.MainActivity`。
- **停 mock 服务别用 `lsof -ti:9797`**：那个命令列出的是**所有碰过这个端口**的进程，包括**连着它的模拟器** —— 它会把模拟器一起杀掉（表现是模拟器莫名其妙退出，系统崩溃报告里是 `qemu-system-aarch64` 的 SIGABRT）。用 `lsof -ti:9797 -sTCP:LISTEN` 只挑监听的那一个，或者干脆按名字 `pkill -f mockapi`。
- **写界面要用 `react-native-safe-area-context` 的 `SafeAreaView`**，RN 自带的那个已弃用（运行时会弹警告条）。
- **mermaid 随包发**（`assets/mermaid.min.txt`，5 MB 全量构建）。它停在 `assets/` 而不是模块里：`metro.config.js` 把 `txt` 加进 `assetExts`，否则 Metro 会把它当 JS 模块去执行。运行时用 `expo-asset` + `expo-file-system` 读成字符串注进 WebView——这样第一次画图也不依赖网络。

## iOS（真机自己装，不分发）

`ios/` 与 `android/` 一样是 **CNG 生成物**，不入库：`npx expo prebuild -p ios` 生成，跟着 `app.json` 走，不手改。

**工具链比 Android 那套更挑：**

- **必须是完整 Xcode，不是 Command Line Tools**：只有 CLT 时 `xcodebuild` 直接报 "requires Xcode"。装完 `sudo xcode-select -s /Applications/Xcode.app` 并跑一次 `sudo xcodebuild -license accept`。
- **CocoaPods 用 Homebrew 装**（`brew install cocoapods`）。系统自带的 Ruby 2.6 太老，`gem install` 会在解析阶段就失败。
- **一个 Apple ID 就够自装**：Xcode 里登进 Personal Team，免费签名 7 天过期，重装即可；iOS 16+ 要在手机上打开「开发者模式」。
- 跑起来：`npx expo run:ios --device`（它会自己 prebuild + pod install）。**`scripts/prepare-android.sh` 是 Android 专用**（Gradle/AGP/R8），别对 iOS 跑；iOS 没有需要钉的等价物。

**`app.json` 里 iOS 那三项各有各的理由：**

- `bundleIdentifier` —— 没有它 `prebuild` 卡在交互式提问，非交互直接失败。
- `NSLocalNetworkUsageDescription` —— iOS 14+ 连局域网要单独授权，缺这条那句询问没有说明。
- `NSAppTransportSecurity` —— 明文 http / ws 到**局域网 IP**。ATS 的例外按域名写，而家里那台机器是个 IP，列不进去，所以是 `NSAllowsArbitraryLoads`；Expo 模板默认就是它，这里**显式钉住**，免得哪天模板变了、配对静默失败。图标是全不透明的 1024 PNG（只是带 alpha 通道），不用换。

**模拟器路线（已验证）：** `xcodebuild -workspace ios/Tachi.xcworkspace -scheme Tachi -sdk iphonesimulator -destination 'platform=iOS Simulator,id=<UDID>' build`，然后 `xcrun simctl install/launch`。模拟器直接能连宿主的 `127.0.0.1`（不需要任何转发），所以配对填 `http://127.0.0.1:9797` 就能跟 mockapi 联调。两个坑：

- **别用 `CODE_SIGNING_ALLOWED=NO` 编。** 那样出来的包能在模拟器里启动、界面也正常，但**没有 Keychain 权限**：配对那一步 SecureStore 会抛 `A required entitlement isn't present`，配对存不下来，App 永远停在配对页。用 Xcode 自己的（模拟器 ad-hoc）签名就好。
- **第一次 `xcodebuild` 可能卡死在 `CompileStoryboard`（`ibtool` 0% CPU 挂几分钟）。** 不是工程的问题：先单独跑一次 `xcrun ibtool ... SplashScreen.storyboard` 把 IB 服务唤醒，或直接重跑 build，第二次就过。

**真机路线（已验证）：** 签名只要一次团队：

```sh
# 先让 Xcode 认一次设备（Window → Devices and Simulators），手机才会出现「开发者模式」
cd mobile/ios
xcodebuild -workspace Tachi.xcworkspace -scheme Tachi -configuration Debug -sdk iphoneos \
  -destination 'platform=iOS,id=<UDID>' DEVELOPMENT_TEAM=<TEAM> CODE_SIGN_STYLE=Automatic \
  -allowProvisioningUpdates build
xcrun devicectl device install app --device <devicectl-id> build/Build/Products/Debug-iphoneos/Tachi.app
xcrun devicectl device process launch --device <devicectl-id> dev.tachi.mobile
```

- **「开发者模式」不是一直存在的菜单项**：它要等 Xcode 真的认过这台手机（Devices 窗口里准备完 DDI）之后，才会出现在 设置 → 隐私与安全性；打开后手机重启、解锁时再确认一次。没开之前 `xcodebuild` 会直接报 `Developer Mode disabled`。
- **免费个人团队装完第一次启动会被拦**：`FBSOpenApplicationErrorDomain error 3 / profile has not been explicitly trusted`。去 设置 → 通用 → VPN 与设备管理 → 开发者 App → 信任那个 Apple ID，之后 `devicectl ... launch` 才通。免费签名 7 天过期，重装即可。
- **Debug 包依赖 Metro**：JS 是从电脑的 8081 拉的，手机和电脑要在同一个 Wi-Fi（这时本地网络权限的弹窗就来了），Metro 一停 App 就是红屏。想要「装上去就能用」，打 `-configuration Release`（JS 打进包里）。

**三个只在真机上才会撞的坑：**

1. **本地网络权限**。第一次连局域网就弹（往往早于配对 —— 连 Metro 的 8081 也算）。点了「不允许」之后连接会被**静默**拦掉，和「电脑没开机」一模一样，很容易误判成服务端坏了。改回来在 设置 → 隐私与安全性 → 本地网络。`messageOfError` 在 iOS 上把那句话多带一条「或没给本地网络权限」，就是这个原因。
2. **没有 `adb reverse`**。模拟器那一整套在真机上不存在：配对地址必须是**电脑的局域网地址**（桌面配对卡片的网卡候选就是为此），手机和电脑同一个 Wi-Fi，访客网络 / AP 隔离会直接断掉。
3. **Metro 可达性**。调试包从电脑的 8081 拉 JS：macOS 防火墙要允许 node 接受入站连接，点错了就是白屏（不是崩溃）。

**一条更快的路：Expo Go。** 现在用的原生模块 Expo Go 都内置，`npx expo start` 扫码即可先看界面；代价是它用不了我们自己的 Info.plist（本地网络与明文由 Expo Go 自己决定），所以它验不了配对那条路。

## 界面

- **正文的 markdown 交给 `@ronradtke/react-native-markdown-display`**（RN 原生的 markdown-it 渲染，表格 / 列表 / 代码块都是它自己的规则）；我们只补两件事：围栏里的 `mermaid` 换成 WebView 那张图，以及所有文字可长按选中。别引 `react-markdown`——那是渲染 DOM 的，手机上用不了。
- **`<system-reminder>` 不进消息流**：桌面把它折成正文旁边一个小标记，手机上没人会点开，所以 `stripReminders` 直接抽掉，`role: 'reminder'` 整条不画。
- **实时那一轮和历史记录必须用同一套组件**（`Message.tsx` 的 `ToolCard` / `Thinking`）。两边各画一套的后果是「正在跑的时候工具卡点不开、也没有参数，停下来切个会话又正常了」。
- **键盘高度自己补**：SDK 58 是 edge-to-edge，manifest 里的 `adjustResize` 不再把窗口缩下来。`App.tsx` 监听 `keyboardDidShow` 给容器垫 `paddingBottom`，否则输入框整条躺在键盘底下。
- 宽屏（≥768）侧栏可以收起，入口是对话 header 左上角那个菜单键。
- **消息列表是 inverted 的**（数据倒过来喂）：打开会话就停在最新一条，往上翻出来的更早一页接在数组末尾（视觉顶部），画面不用补偿滚动位置。往上翻到头会走 `page` 端点续读，`hasMore` 跟着最旧的那条走；快照重读时 `mergeSnapshot` 把已经翻出来的更早消息留在原处（一轮结束、重连、lagged 都会触发重读）。进对话先翻到底部这一条也要它：非 inverted 的列表默认停在顶部，等于让人从一百条前的一页往下翻。
- **列表数据里只放历史**（`buildItems` 只依赖快照）：实时那一轮（`buildLiveTail`）和待答项（`buildAskItems`）挂在 `ListHeaderComponent` 上——inverted 列表的头部就是视觉最底部，待答项因此仍在最靠底的位置（实时那一轮在它上面）。理由：RN 的列表单元是 `PureComponent`，`renderItem` 和 `item` 的引用一变，所有已挂载的单元全部重画；流式文字每个字都在变，混进同一个数组就等于每来一段文字都把可见消息的 markdown 重新解析一遍。同样地，`Chat` 的 `renderItem` 走 `useCallback` 加一个 `env` ref（回调、主题、附件都从那里取），它的引用必须保持稳定。状态层还有一道：`mobile.ts` 把 `text`/`thinking` 事件攒到下一帧再提交（`flushStream`），任何别的事件到来前先把它吐出去，否则快照会看见一段「还没发生」的历史。
- **交付物不是过程**：一次成功的 `SendFile` 不折进「N 步」那一行，而是钉在本轮结尾（`buildItems` 里那个 `files`）—— 它是「把 X 发给我」的答案，折起来等于把答案藏在一个要展开的行后面（桌面 `turnView` 的 attachments 那一栏是同一个分法）。卡片优先用快照的 `attachments`（有大小、可下载），没有那一条时从调用参数里的 `path` 兜一个出来（`fileFromSendFileArgs`），否则一个快照还没带上附件的调用会退回成普通工具卡。
- **一轮的过程折成一行（`ProcessFold`）**：思考和成功的工具调用都收进去，**按记录里的先后**排（点开照这个顺序展开，不按种类分组）；只有工具时标 `N 步`，只有思考时标 `思考 N 段`。正文留在外面，失败的那一步也留在外面。判据是两条用户消息之间，不是「相邻」。真实记录里两个 `tool_call` 之间一定夹着 `tool_result`，还常常夹着 `thinking` 和**内容为空的 assistant**（模型每调一次就写一条）——按相邻判断的话每段只剩一个工具，永远折不起来，手机上看到的就是「每个 tool call 都露在外面」。失败的那一步留在原地，出错要看得见。分组的纯函数在 `src/state/items.ts`，拆成三份：`buildItems`（历史）、`buildLiveTail`（实时那一轮）、`buildAskItems`（待答项）；`items.test.ts` 用真实记录的形状钉住。实时那一轮仍在跑的时候逐步显示（过程就是信息）。
- **从左边往右划 = 返回上一屏**：凡是「推上去的一页」都挂 `src/ui/edgeBack.ts` 的 `useEdgeBack` —— 对话（窄屏回列表 / 宽屏侧栏收起时把列表拉回来）、会话信息、设置、新建会话、添加电脑（配对页只在有上一屏时才有返回）。**文件阅读器故意不挂**：那里的图片是可以双指缩放、拖着看的，横向手势归它。iOS 没有系统返回手势，由这个 hook 的 PanResponder 认（起点在左缘 24pt 内、横向划够 64pt，正文里的横向操作不被吃）。**Android 上这条手势归系统**（手势导航的左缘返回），App 收不到 —— 所以同一件事必须由 `BackHandler` 完成：它先判断「宽屏且侧栏收起」，再决定是展开侧栏还是关闭会话；只做 PanResponder 的话，Android 上这一划会直接退到桌面。
- **左滑露出两个动作：置顶 / 删除**（`SwipeRow` 的 actions）。收起的动作层从可达性树里摘掉，否则读屏和自动化会点到一个看不见的按钮。
- **这一行的手势在 UI 线程上（`react-native-gesture-handler` 的 `Gesture.Pan` + `reanimated` 的共享值）**，不是 `PanResponder`。`PanResponder` 每一帧都要过一次 JS 线程，而这块屏的 JS 线程随时被会话事件占着 —— 表现就是「跟不上手指、一划就弹回去」，调动画参数救不回来。判据也交给 GH：`activeOffsetX([-8, 8])` 让横向归这一行、`failOffsetY([-14, 14])` 让纵向归列表，两者不再互相抢；松手用 `withTiming` 按「还剩多远」定快慢。**这条路线要求根节点是 `GestureHandlerRootView`**（`App.tsx` 最外层），缺了它手势静默不触发。
- **页面返回（`edgeBack`）同在 UI 线程**，判据与本行不同：起手点必须在**左缘 16pt** 以内（正文的左右内边距正好是 16，所以代码块、表格这些从 16 开始 —— 手势区与它们不重叠，在代码块上横划只滚代码块），往右 10px 才认领、竖着动 16px 就让给列表滚动；整屏跟手右移，松手过三成屏宽（或甩得够快）才补完动画返回。用 `PanResponder` 时它是**先抢到触摸**再判断，正文里的横向滚动抢不过它，于是「在代码块上划一下就退回列表」。还有一条不在手势里、但读者一定感觉得到：**离开对话要退订**（`open` 的 `sessionId` 传空串）—— 否则那段在跑的对话每秒几十条增量还在往手机上灌。
- **会话的增删改是广播，不是某一段会话的流**：桌面新建 / 删除 / **重命名 / 置顶**、手机建 / 删 / 置顶都发 `sessions_changed`，客户端重读 `/v1/sessions`，并把**打开着的那段**的标题等一并修掉（不修的话，桌面上改了名的对话在这里一直显示旧名）。手机上是「列表左滑 → 置顶 / 删除」，删除走原生确认框与 `DELETE /v1/sessions/{id}`，正在跑的那一轮会被拒绝（和桌面同一条 `DeleteSession`）。
- **新建会话在列表 header 的 `+`**：项目可选，无项目时手输工作目录（空 = `~/workspace`，`~` 由电脑展开）。桌面那份 `NewSession` 会把窗口切到新会话上，手机不走它 —— 协议不变量 7 要求手机不改变桌面正在显示的那一段。
- **记住多台电脑，同时只连一台**：`src/api/storage.ts` 存的是「列表 + 当前下标」（`tachi.devices`），单台时代的 `tachi.pairing` 在第一次读取时迁进来并删掉。切换 = 断开那条 socket、清掉画面、重连；会话在电脑上，所以切过去看到的是另一台电脑的会话。设置页里能加、切、删；**加完自动回设置页**（留在配对页会让人以为没成功）。
- **会话信息页有四个页签**：概览（上下文 / 工作区 / 模型 / 审批 / 用量）、MCP、Agent、定时任务。后三个面板里 MCP 与 Agent 是只读的，定时任务有三个动作（运行一次 / 暂停·恢复 / 删除）；**每个会话只自动问一次**（`asked`），带一个刷新键；能连 server、能授权、能建任务、能改计划的只有桌面。定时任务的三个动作与电脑同一个写入路径，改动由 `cron_changed` 广播到每一块屏（窗口收到 `agent:cron_changed` 重读，手机收到就重读面板）——**双端谁改的都立刻在另一边可见**，没有第二条同步路径。**别把「panels 为空」当成再拉一次的条件** —— 请求失败时那就是一个没有出口的重试循环：屏幕上 loading 翻个不停、电脑被轮番请求。失败的原因显示在面板里（`panelError`），重试只由刷新键发起。MCP 面板说「这段对话载入 x/y 个工具」—— 工具搜索下 y 可以是全部而 x 是零。
- **切后台回来要重建 WebSocket**（`MobileSocket.forceReconnect`）：锁屏期间定时器被挂起、连接可能半开，而 `readyState` 仍报 OPEN——只调 `ensureConnected` 的表现为「回来一直显示连不上，杀掉重进才好」。
- **运行中压缩会换会话 id，画面要跟着搬**：压缩成功时 agent 把历史写进**子会话**，这一轮剩下的部分接着在子会话里跑，而 `idle` 也发往子会话。桌面在同一刻换轨（`agent:session_switched`），手机收到 `session_moved`（`from` / `to` / `end`）后把打开的那个 id、WS 订阅、还没发出去的队列一起搬过去，并重读子会话的快照。不搬的表现是：画面停在压缩点、状态永远「在跑」、排队的话发不出去，只能退回列表再点进来。换轨发生在**一轮结束之后**——压缩那一刻就搬，会把正在写的消息从画面底下抽走。**队列靠通知里的 `end` 补发**：换订阅一定比 `idle` 晚一步，等那条事件是等不到的；规则与 `idle` 那条相同（自然结束就发，被打断/出错就留）。

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
- **端到端用 `mobile/e2e/`**：`bash mobile/e2e/run.sh`（每条 flow 之前都把 mockapi 重启成种子状态：`main.yaml` 是主链路，`compact.yaml` 是运行中压缩换轨 + 队列补发）。三个踩过的点：**Maestro 的文本匹配默认是整串相等**（要包含就写成 `".*命令跑完了.*"`）；**iOS 上一行在无障碍树里是合并后的一整串**（`移动端接入方案, tachi · 昨天`），所以点会话要用 `.*标题.*`，Android 上是分开的元素；flow 会消耗种子数据（点掉那张待答卡、造出子会话），所以每条之前都得重置 mock 服务 —— 脚本做这件事。
- **模拟器扫不了码**：它的相机没有真实画面，所以扫码这条路只能验到"界面能打开、权限流程正常"，二维码的解析由 `src/api/qr.test.ts` 钉住（码里装的是 `{"v":1,"url":"…","token":"…"}`）。
- 桌面的会话记录是权威，实时流只延长尾巴；验界面时以"重读快照后画面正确"为准，不要只信流。

## 服务端

`desktop/mobile*.go`，package `main`，随桌面进程起落（`main.go` 的 `startMobile`）；配置块 `mobile.{enabled,listen,max_attachment_mb}`，监听地址默认 `0.0.0.0:8787`。**端点、事件、字段的出处是 [mobile-protocol.md](mobile-protocol.md)**，这一节只说那些不做就会静默坏掉的地方。

- **用量与上下文不能挂在一个 live run 上**（`mobile_session.go` 的 `mobileUsage` / `estimateContextFromRecords`）：手机点开一段电脑上没打开过的对话时没有 run，报告「0 元、0 token」看着就是坏了。金额与积分从**用量账本**（`agent.GlobalUsageRecorder().Rows(id, createdAt)`）算，上下文估算从记录里最近一次调用的 `usage` 算，上下文窗口从配置里那个 provider 的 `spec.context_window`（没有就用模型表）算。分项（`ContextInfo.Parts`）只有活着的 agent 才有，所以空着的时候界面说「只有总量」。
- **手机建会话、置顶都走桌面同一条路**：置顶复用 `SetSessionPinned`；建会话只写记录（不建 agent、不碰 `activeID`），第一条消息才由 `loadSessionForRemote` 建起来。
- **`emitSessionsChanged` 同时发给窗口和手机**：一个事实一个出口 —— 重命名、置顶、新建、删除都从它出去，谁少发一次，另一块屏就停在旧列表上。
- **面板是只读的，而且按会话寻址**（`mobileSessionPanels`，`GET /v1/sessions/{id}/panels`）：MCP 那半用的是**那一段会话**的载入集合（`listMCPServersFor`，窗口用的是 `activeID` 那条路），否则手机看到的会是桌面正开着的那一段能调什么。agent 那半直接读 `PeerBoard()`（带缓存，不强制探测），定时任务那半读 `ListCronJobs()`。

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
- **配对地址是「候选 + 一个选择」**：`mobileLANCandidates` 列出这台机器上手机够得着的 IPv4（物理网卡优先；`utun*` 的 VPN 地址、`bridge*`/`vmenet*`/`docker*` 的虚拟网络、链路本地地址都不算），有多于一个时候卡片给一个下拉框。选择存进 `mobile.json` 的 `interface`——按**接口名**而不是地址存，DHCP 换了地址也不会指到一个没人持有的 IP；空 = 自动（候选里的第一个）。选中的接口没了就退回自动。`SetMobilePairingHost` 只接受当前候选里的接口名。
