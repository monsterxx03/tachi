#!/usr/bin/env bash
# 把 shim.swift 编成 libtachi_stt.a，给 cgo 链。
#
# 为什么不把产物入库：它是编译结果（和 .o 一样），入库只会让「改了 .swift 忘了重编」
# 变成一种看起来正常的状态。所以每次构建都先跑一遍这个脚本，产物在 .gitignore 里。
#
# 用法：./build_shim.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/libtachi_stt.a"
# 和 app 的其余部分同一个部署目标（build/darwin/Taskfile.yml 的 MACOSX_DEPLOYMENT_TARGET）。
# 定成 26.0 会让整个 app 在 macOS 15 上连启动都不行——见 shim.swift 顶上那段。
DEPLOY_TARGET="12.0"
TARGET="$(uname -m)-apple-macosx${DEPLOY_TARGET}"

# -static：桌面 app 要的是一个自包含的二进制（没有随包的 dylib 要 dylib 路径去找）。
# -parse-as-library 是必需的：有 @main 之外，顶层代码在库里没有意义。
swiftc -O -parse-as-library -emit-library -static \
    -module-name TachiSTT \
    -target "$TARGET" \
    -o "$OUT" \
    "$HERE/shim.swift"

# 部署目标 12.0 让产物里带了一条 `-lswiftCompatibility56` 的 auto-link 指令（Swift 运行时
# 的 back-deployment 库），而那个库**不在 SDK 里**，只在 toolchain 自己的
# lib/swift/<平台>/ 下——cgo 用 clang 链接、默认不搜那里，于是整个包链接失败：
#   Undefined symbols: __swift_FORCE_LOAD_$_swiftCompatibility56
# 因为它不在 SDK 里，就没法靠 -framework / SDK 路径解决，只能把它带到 cgo 找得到的地方：
# 链到本目录，由 stt_darwin.go 的 `-L${SRCDIR}` 收进搜索路径。
#
# 路径从 swiftc 自己的位置推（CLT 与 Xcode 各有一份、位置不同），所以这里没有写死的路径；
# 链的是符号链接而不是拷贝：那是 toolchain 的一部分，跟着它升级。
SWIFT_LIB="$(cd "$(dirname "$(xcrun --find swiftc)")/../lib/swift/macosx" && pwd)"
for lib in libswiftCompatibility56.a libswiftCompatibilityPacks.a; do
    if [ ! -f "$SWIFT_LIB/$lib" ]; then
        echo "toolchain 里没有 ${lib}（找的是 ${SWIFT_LIB}）——换个 toolchain 或者重新看这段注释" >&2
        exit 1
    fi
    ln -sf "$SWIFT_LIB/$lib" "$HERE/$lib"
done

echo "→ $OUT ($(du -h "$OUT" | cut -f1)) + 2 个兼容库链接（给 cgo 的 -L）"
