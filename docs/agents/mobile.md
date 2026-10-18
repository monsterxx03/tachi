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

- **JDK 必须是 17。** 这台机器上可能还装着更老的（`java -version` 报 1.8 是常见的），所以一律显式指定 `JAVA_HOME=$(/usr/libexec/java_home -v 17)`。macOS 只认 `~/Library/Java/JavaVirtualMachines/<名>.jdk/Contents/Home` 这个布局，tar.gz 解压出来要按它摆。
- **Node 不能是 20。** React Native 0.88 的 engines 是 `^22.13 || ^24.3 || >=26`；用 20 会在装依赖时以 `EBADENGINE` 起步，出问题的位置和 Node 毫无关系。本机 homebrew 的 26 就够用。
- **`ANDROID_HOME=~/Library/Android/sdk`。**

SDK 里放什么，分两类：

| 手动装（Gradle 不会替你装） | 目录 |
| --- | --- |
| 命令行工具 | `cmdline-tools/latest/`（`sdkmanager` / `avdmanager`；新版提示 sdkmanager 已改名 `android`，老的还能用） |
| adb | `platform-tools/` |
| 模拟器与系统镜像 | `emulator/`、`system-images/android-36/google_apis/arm64-v8a/`（Apple Silicon 必须用 **arm64-v8a** 镜像） |

**其余（platform、build-tools、NDK）交给 Gradle 自己补**：它按项目实际要求装，实测会拉 `platforms/android-37.0`、`build-tools/37.0.0`、`ndk/27.1.12297006`——手动装一份 36 反而白装。耗时取决于那台机器的网络。

两个手工步骤，都是"包装里没有、而工具要求"的东西：

- **`emulator/` 要补一个 `package.xml`**，否则 `avdmanager create avd` 直接报 `"emulator" package must be installed!`（它靠这个文件认已安装的包，Google 的 zip 里不带）。照着系统镜像目录里那份 `package.xml` 仿写即可。
- **模型/镜像放好后，`avdmanager` 才认得**：`avdmanager create avd -n tachi-phone -k "system-images;android-36;google_apis;arm64-v8a" -d pixel_8`（平板用 `-d pixel_tablet`）。

启动模拟器：`"$ANDROID_HOME/emulator/emulator" -avd tachi-phone &`，冷启动要一两分钟，等 `adb devices` 里出现 `device`、`adb shell getprop sys.boot_completed` 回 `1`。

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
| 协议与桌面那半边 | Go 测试（`httptest` 打接口）+ 一条真实桌面进程的端到端脚本 | 全自动 |
| 手机端纯逻辑 | `npx tsc --noEmit` + `npm test`（jest-expo；`src/state/chat.test.ts` 钉的是事件归并和错误文案那层纯函数） | 全自动 |
| 手机端界面 | **web 目标**：`npm run web`，浏览器里自己点、截图、看 console 与网络 | 全自动（agent 用浏览器工具） |
| 模拟器上的真界面 | `adb exec-out screencap -p > x.png` 看一眼；改代码后 Metro 会热重载，不用重装 | 全自动 |
| 触摸交互、扫码、手感、锁屏重连 | Maestro 驱动点击（`~/.local/maestro/bin/maestro`，装在仓库外） | 半自动 |
| 真机验收 | 只能人看 | 人 |

- **写界面时保持它能跑在 web 上**——那是 agent 能自己看见界面的另一条路。代价是原生模块要有一层平台适配（安全存储在 web 上退化成 localStorage），这层本来也该有。
- **端到端用 `mobile/e2e/`**：`bash mobile/e2e/run.sh`（它先把 mockapi 重启成种子状态，再让 Maestro 跑 `main.yaml`）。两个踩过的点：**Maestro 的文本匹配是精确相等、不是包含**（要包含就写成 `".*命令跑完了.*"`）；flow 会消耗种子数据（点掉那张待答卡），所以每次跑之前都得重置 mock 服务 —— 脚本做这件事。
- **模拟器扫不了码**：它的相机没有真实画面，所以扫码这条路只能验到"界面能打开、权限流程正常"，二维码的解析由 `src/api/qr.test.ts` 钉住（码里装的是 `{"v":1,"url":"…","token":"…"}`）。
- 桌面的会话记录是权威，实时流只延长尾巴；验界面时以"重读快照后画面正确"为准，不要只信流。

## 服务端

（待实现：移动接口的端点、那几条按会话寻址的桌面方法、`agent:user_message` 的 origin 参数。实现后写在这里，指针指向 `desktop/mobile*.go`。）
