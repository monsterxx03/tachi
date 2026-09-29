#!/usr/bin/env bash
# 建一张本地的代码签名证书——TCC 要的那个「稳定身份」。
#
# 为什么需要它：macOS 把权限（蓝牙、语音识别、通知）记在 app 的**代码身份**上。wails3
# 打出来的包是 ad-hoc 签名，而 ad-hoc 的身份就是内容哈希：改一行代码重建，哈希就变，
# 于是 macOS 认为这是「一个还没被允许过的新 app」，把权限再问一遍。用一张证书签名，
# 身份就跨重建稳定了（desktop/Makefile 的 sign-local 与 LOCAL_SIGN_ID 就是为这个存在的）。
#
# 换机器怎么办（这一条是刻意的，不是副产品）：
#   · 把这份脚本产出的 identity.p12 带走（它含私钥，存到安全的地方，别入库），在
#     新机器上跑 `security import`（见下面打印的那两行），或者
#   · 干脆在新机器上重跑一次这份脚本——代价是那台机器上第一次启动要重新授权一次
#     （身份变了，而 TCC 的记录是本机的）。
#
# 用法：./scripts/dev-cert.sh [证书名]
#   默认名字与 desktop/Makefile 的 LOCAL_SIGN_ID 一致。
set -euo pipefail

NAME="${1:-Tachi Local Code Signing}"
OUT_DIR="${HOME}/.tachi/dev-signing"
KEYCHAIN="${HOME}/Library/Keychains/login.keychain-db"

mkdir -p "${OUT_DIR}"
cd "${OUT_DIR}"

if security find-identity -v -p codesigning 2>/dev/null | grep -qF "${NAME}"; then
    echo "✓ 已经有这张证书了：${NAME}"
    echo "  （要重建就先在「钥匙串访问」里删掉它，以及 ${OUT_DIR} 下的旧文件）"
    exit 0
fi

echo "→ 生成自签证书（${OUT_DIR}）"
# 关键的两条扩展：keyUsage/codeSigning 让 codesign 认它，basicConstraints=CA:false 表明
# 它不是 CA（我们只要它签自己这一个 app）。
/usr/bin/openssl req -x509 -newkey rsa:2048 -keyout key.pem -out cert.pem -days 3650 -nodes \
    -subj "/CN=${NAME}" \
    -addext "keyUsage=critical,digitalSignature" \
    -addext "extendedKeyUsage=critical,codeSigning" \
    -addext "basicConstraints=critical,CA:false" 2>/dev/null

# ⚠ 两个坑都踩过了，别改回去：
#
#   1. **用 /usr/bin/openssl，也就是系统自带的 LibreSSL。** Homebrew 那个 OpenSSL 3
#      默认用 AES-256 + SHA-256 打包 p12，而 macOS 的 SecKeychainItemImport 只认老算法
#      （SHA1/3DES）——报的是 "MAC verification failed during PKCS12 import (wrong
#      password?)"，它说密码，其实是算法不认。
#   2. **p12 不能是空密码。** `-passout pass:` + `security import -P ""` 也会撞上同一句
#      "wrong password?"。这个密码不是安全边界（文件本身就在你的 home 下），它只是让
#      钥匙串愿意收下这份东西，所以写死一个就行。
P12_PASS="tachi"
/usr/bin/openssl pkcs12 -export -inkey key.pem -in cert.pem -out identity.p12 \
    -passout "pass:${P12_PASS}" -name "${NAME}" 2>/dev/null

echo "→ 导入登录钥匙串（-T 让 codesign 有权用它，否则每次签名都会弹一次确认）"
security import identity.p12 -k "${KEYCHAIN}" -P "${P12_PASS}" \
    -T /usr/bin/codesign -T /usr/bin/security >/dev/null

echo "→ 把这张证书标成受信任（这一步会弹一次系统授权框，需要你点一下）"
security add-trusted-cert -r trustRoot -k "${KEYCHAIN}" cert.pem >/dev/null

echo
echo "✓ 完成。检查一下："
security find-identity -v -p codesigning | grep -F "${NAME}" || {
    echo "✗ 没找到它——多半是「信任」那一步没点，去 钥匙串访问 → 系统/登录 → 找到"
    echo "  「${NAME}」→ 显示简介 → 信任 → 代码签名：始终信任"
    exit 1
}

echo
echo "换机器时（把这三行带走）："
echo "  security import identity.p12 -k ~/Library/Keychains/login.keychain-db -P tachi -T /usr/bin/codesign"
echo "  security add-trusted-cert -r trustRoot -k ~/Library/Keychains/login.keychain-db cert.pem"
echo "  （identity.p12 与 cert.pem 就在 ${OUT_DIR}；它们含私钥，别入库）"
