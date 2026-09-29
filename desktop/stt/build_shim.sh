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

echo "→ $OUT ($(du -h "$OUT" | cut -f1))"
