#!/usr/bin/env bash
#
# prebuild 之后跑这个。
#
# android/ 是生成目录（Expo 的 CNG，不入库），所以任何「改生成物」的动作每次
# prebuild 之后都要重做 —— 写成脚本，免得某次构建悄悄用了国外的源、慢到以为卡住，
# 或者栽在下面那个 Gradle 版本的坑上。
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
wrapper="$here/android/gradle/wrapper/gradle-wrapper.properties"
build_gradle="$here/android/build.gradle"

if [[ ! -f "$wrapper" ]]; then
  echo "找不到 $wrapper —— 先跑 npx expo prebuild --platform android" >&2
  exit 1
fi

# 1) Gradle 发行版走腾讯镜像（默认的 services.gradle.org 在国内要绕代理）。
if grep -q 'mirrors.cloud.tencent.com/gradle' "$wrapper"; then
  echo "gradle 发行版已是腾讯镜像"
else
  sed -i '' -E \
    's#^distributionUrl=https\\://services\.gradle\.org/distributions/#distributionUrl=https\\://mirrors.cloud.tencent.com/gradle/#' \
    "$wrapper"
fi

# 2) 版本钉在 9.5.1。
#
#    模板给的是 9.4.1，而它在编译**纯 Java** 的 Android 库时必挂：
#      :react-native-webview:compileDebugJavaWithJavac
#        > java.lang.ClassNotFoundException: org.gradle.internal.compiler.java.IncrementalCompileTask
#
#    看着像 react-native-webview（或 react-native-svg）坏了 —— 换掉那个库、统一 AGP 版本、
#    关掉 fork，错误都一模一样，因为它其实是 Gradle 自己的毛病：9.4.1 的 JdkTools 去加载
#    org.gradle.internal.compiler.java.IncrementalCompileTask，而整个
#    org/gradle/internal/compiler/java/ 包在 9.4.1 里已经没有了。Expo 自己的模块是 Kotlin，
#    不走这条编译路径，所以只有第三方 Java 库会炸。
#
#    为什么是 9.5.1 而不是更新：9.8.1 能让这条路径过去，但会让 Expo 的 gradle 插件
#    Kotlin 编译失败（`org.jetbrains.kotlin...GradleKotlinCompilerWorkAction`），
#    那是另一头不兼容。9.5.1 两头都过。
if grep -q 'gradle-9.5.1-bin.zip' "$wrapper"; then
  echo "gradle 版本已是 9.5.1"
else
  sed -i '' -E 's#gradle-[0-9.]+-bin\.zip#gradle-9.5.1-bin.zip#' "$wrapper"
  echo "gradle 版本 -> 9.5.1"
fi
grep '^distributionUrl' "$wrapper"

# 3) 第三方库请求的老 AGP 统一到 RN 自己用的那个版本。
#
#    react-native-webview 的 build.gradle 无条件声明 classpath AGP 7.0.4、
#    react-native-svg 声明 7.4.2，而 RN 0.88 用的是 9.2.1。不统一的话，构建会多下一份
#    用不上的老插件；它们请求的版本在 Gradle 9 下也没有意义。
#    （这不是上面那个 ClassNotFoundException 的修法 —— 试过，加了也照样报。它只是别让
#    老版本漂进来。版本号来自 node_modules/react-native/gradle/libs.versions.toml 的 agp。）
if grep -q 'resolutionStrategy.eachDependency' "$build_gradle"; then
  echo "AGP 统一已存在"
else
  cat >> "$build_gradle" <<'EOF'

// 见 scripts/prepare-android.sh（这一节每次 prebuild 后由它补上）：
// 有些第三方库在自己的 buildscript 里硬编码了很老的 Android Gradle Plugin
// （react-native-webview 写 7.0.4、react-native-svg 写 7.4.2），而 RN 用的是 9.2.1。
// 统一到 RN 那个版本，别让用不上的老插件漂进 classpath。
allprojects { p ->
  p.buildscript.configurations.all {
    resolutionStrategy.eachDependency { details ->
      if (details.requested.group == 'com.android.tools.build' && details.requested.name == 'gradle') {
        details.useVersion '9.2.1'
      }
    }
  }
}
EOF
  echo "AGP 版本统一 -> 9.2.1"
fi

# 4) ML Kit 的 registrar 类。
#
#    ML Kit 通过 AndroidManifest 里那个 provider 的 metadata 列出自己的组件
#    （CommonComponentRegistrar / BarcodeRegistrar …），运行时用反射 new 出来。
#    R8 看不到这次反射，release 包会把这些类的**无参构造函数**裁掉，于是：
#
#      W ComponentDiscovery: Could not instantiate com.google.mlkit.vision.barcode.internal.BarcodeRegistrar
#      Caused by: java.lang.NoSuchMethodException: ...BarcodeRegistrar.<init>
#
#    表现是相机能开、预览正常，但扫码永远不回调 —— debug 包不做裁剪，所以只有 release 坏。
proguard="$here/android/app/proguard-rules.pro"
if [[ -f "$proguard" ]] && ! grep -q 'com.google.mlkit' "$proguard"; then
  cat >> "$proguard" <<'EOF'

# ML Kit 的组件是被反射实例化的（见 scripts/prepare-android.sh），R8 必须整包保留。
-keep class com.google.mlkit.** { *; }
-keep class com.google.android.gms.internal.mlkit_vision_barcode.** { *; }
-keep class com.google.android.gms.internal.mlkit_common.** { *; }
-dontwarn com.google.mlkit.**
EOF
  echo "ML Kit 的 keep 规则 -> proguard-rules.pro"
else
  echo "ML Kit 的 keep 规则已存在"
fi
