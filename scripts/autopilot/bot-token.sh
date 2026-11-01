#!/usr/bin/env bash
#
# 以 bot 的身份换一个 GitHub 安装令牌，给 autopilot 的 gh 和 git 用。
#
#   scripts/autopilot/bot-token.sh            打印令牌
#   scripts/autopilot/bot-token.sh --identity 打印 git 要的作者行：「名字 <邮箱>」
#
# 令牌是 GitHub 现签现给、一小时就废的，所以缓存只是为了省一次网络往返：过了 50 分钟就重新换。
#
# 凭证不在仓库里，也不在任何一次 PR 里。它们住在 $TACHI_HOME/autopilot/：
#   app.pem      App 的私钥，权限 600（在 App 设置页 Private keys 里生成，只有下载那一次）
#   app.json     {"client_id": "Iv2...", "slug": "tachi-autopilot"}
#                「装在哪儿」不用手填：第一次问 GitHub 要，之后缓存；一个账号装了两处时，
#                在 app.json 里加 "installation_id" 指明。
#
# 只认私钥，**不认 client secret**：那个是给「用户拿 App 登录」那条路用的，签不了令牌。
#
# 换不到就报错退出（退出码非 0）——调用方一定不能拿空令牌继续往下走。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TACHI_HOME="${TACHI_HOME:-$HOME/.tachi}"
CONF_DIR="$TACHI_HOME/autopilot"
KEY="$CONF_DIR/app.pem"
META="$CONF_DIR/app.json"
CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/tachi-autopilot"
TOKEN_FILE="$CACHE_DIR/token"
INSTALL_FILE="$CACHE_DIR/installation_id"
# 令牌寿命一小时；留 10 分钟余量，免得一次长命令跑到一半过期。
TOKEN_TTL=3000

die() { echo "bot-token: $*" >&2; exit 1; }

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

meta() {
  python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d.get(sys.argv[2],""))' "$META" "$1"
}

api_get() {
  curl -sS --fail-with-body -H "Authorization: Bearer $1" \
    -H "Accept: application/vnd.github+json" "$2"
}

# jwt —— 十分钟内有效、给 App 自己用的通行证。GitHub 要 RS256，iss 填 client id。
jwt() {
  local iat exp header payload signing
  iat=$(( $(date +%s) - 60 ))   # 回拨一分钟：对方的钟可能比我们慢一点
  exp=$(( iat + 600 ))          # GitHub 只收 10 分钟以内的 JWT
  header="$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)"
  payload="$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' "$iat" "$exp" "$(meta client_id)" | b64url)"
  signing="$header.$payload"
  printf '%s.%s' "$signing" \
    "$(printf '%s' "$signing" | openssl dgst -sha256 -sign "$KEY" -binary | b64url)"
}

# installation_id —— 这个 App 装在哪。装了两处才会用到 app.json 里那个显式的值。
installation_id() {
  local pinned id
  pinned="$(meta installation_id)"
  if [[ -n "$pinned" ]]; then
    printf '%s' "$pinned"
    return 0
  fi
  if [[ -f "$INSTALL_FILE" ]] && [[ -s "$INSTALL_FILE" ]]; then
    cat "$INSTALL_FILE"
    return 0
  fi
  id="$(api_get "$(jwt)" "https://api.github.com/app/installations" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')" \
    || die "问不到 App 装在哪（App 还没安装？）"
  mkdir -p "$CACHE_DIR"
  printf '%s' "$id" > "$INSTALL_FILE"
  printf '%s' "$id"
}

# mint_response —— 换令牌的原始响应。里面还有「这个 App 到底拿到了哪些权限、能碰哪些仓库」，
# 排查「为什么 403」时看它，比猜快。
mint_response() {
  curl -sS --fail-with-body -X POST \
    -H "Authorization: Bearer $(jwt)" \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com/app/installations/$(installation_id)/access_tokens"
}

mint() {
  local token
  token="$(mint_response | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')"
  [[ -n "$token" ]] || return 1
  printf '%s' "$token"
}

identity() {
  # GitHub 只把提交认到账号名下，当作者邮箱正好是它的 noreply 地址时。
  # App 的这份是 <bot 用户 id>+<slug>[bot]@users.noreply.github.com。
  local slug id
  slug="$(meta slug)"
  id="$(curl -sS --fail-with-body -H "Accept: application/vnd.github+json" \
    "https://api.github.com/users/${slug}%5Bbot%5D" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')" \
    || die "查不到 bot 账号 ${slug}[bot]"
  printf '%s[bot] <%s+%s[bot]@users.noreply.github.com>' "$slug" "$id" "$slug"
}

case "${1:-}" in
  --identity) identity; exit 0 ;;
  --inspect) mint_response | python3 -m json.tool; exit 0 ;;
  "") ;;
  *) die "未知参数：$1" ;;
esac

[[ -r "$KEY" ]] || die "找不到私钥 $KEY（App 设置页 Private keys → Generate a private key）"
[[ -r "$META" ]] || die "找不到 $META"

if [[ -f "$TOKEN_FILE" ]]; then
  age=$(( $(date +%s) - $(stat -c %Y "$TOKEN_FILE" 2>/dev/null || stat -f %m "$TOKEN_FILE") ))
  if [[ $age -lt $TOKEN_TTL ]]; then
    cat "$TOKEN_FILE"
    exit 0
  fi
fi

if ! token="$(mint)"; then
  # 缓存的安装 id 过期（App 被卸载重装过）是唯一合理的失败：丢掉它再问一次。
  rm -f "$INSTALL_FILE"
  token="$(mint)" || die "换令牌失败"
fi

# 先写临时文件再改名：两个调用方同时进来时，谁也不会读到半个令牌。
umask 077
mkdir -p "$CACHE_DIR"
printf '%s' "$token" > "$TOKEN_FILE.tmp.$$"
mv "$TOKEN_FILE.tmp.$$" "$TOKEN_FILE"
printf '%s' "$token"
