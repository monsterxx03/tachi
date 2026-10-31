#!/usr/bin/env bash
#
# 给 PR 抓一张「改动后的界面」，并把它变成一段能贴进 PR body 的 markdown。
#
# 用法：
#
#   scripts/autopilot/screenshot.sh mobile  --name 会话列表 --upload
#   scripts/autopilot/screenshot.sh web     --name 会话页 --url http://127.0.0.1:8787 --upload
#   scripts/autopilot/screenshot.sh desktop --name 主窗口
#
# 选项：
#   --name <名字>   文件名（默认时间戳）
#   --out <目录>    本地落点（默认 $TMPDIR/tachi-autopilot-shots）
#   --upload        传到 auto-assets 分支，打印可贴进 PR 的 ![](...)
#   --url <地址>    仅 web 后端：要截的页面
#   --size <WxH>    仅 web 后端：视口（默认 420x900，手机宽度）
#   --device <序列> 仅 mobile 后端：指定设备（默认只接受模拟器）
#
# **默认不上传**：截图会进入一个公开仓库，上传必须显式要求。PR 需要图时用 --upload。
#
# 隐私红线：只截 sandbox / mockapi 的假数据。绝不截真实会话、真实文件路径、真实
# 用户名——这个仓库是公开的，图片一推上去就撤不回来了。
#
# 退出码：0 成功；1 抓图失败；2 环境不具备（没有设备 / 没有显示）。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BACKEND="${1:-}"; shift || true

NAME="$(date +%Y%m%d-%H%M%S)"
OUT="${TMPDIR:-/tmp}/tachi-autopilot-shots"
UPLOAD=0
URL="http://127.0.0.1:8787"
SIZE="420x900"
DEVICE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --name)   NAME="${2:?}"; shift 2 ;;
    --out)    OUT="${2:?}"; shift 2 ;;
    --upload) UPLOAD=1; shift ;;
    --url)    URL="${2:?}"; shift 2 ;;
    --size)   SIZE="${2:?}"; shift 2 ;;
    --device) DEVICE="${2:?}"; shift 2 ;;
    -h|--help) sed -n '2,24p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "未知参数：$1" >&2; exit 1 ;;
  esac
done

mkdir -p "$OUT" || exit 1
SHOT="$OUT/$NAME.png"

# ---------------------------------------------------------------------------
# 抓图
# ---------------------------------------------------------------------------
capture() {
  case "$BACKEND" in
    mobile)
      command -v adb >/dev/null 2>&1 || { echo "没有 adb" >&2; return 2; }
      local dev="$DEVICE"
      if [[ -z "$dev" ]]; then
        # 只认模拟器：这台机器上同时插着真机，截真机的屏幕就是截你的相册和聊天记录。
        dev="$(adb devices | awk 'NR>1 && $2=="device" && $1 ~ /^emulator-/{print $1; exit}')"
      fi
      [[ -n "$dev" ]] || { echo "没有已连接的模拟器（先起 emulator -no-window；要用真机就显式 --device）" >&2; return 2; }
      adb -s "$dev" exec-out screencap -p > "$SHOT" || return 1
      ;;
    web)
      local chrome
      chrome="$(command -v google-chrome-stable || command -v chromium || command -v chromium-browser || true)"
      [[ -n "$chrome" ]] || { echo "没有 chrome/chromium" >&2; return 2; }
      "$chrome" --headless=new --disable-gpu --hide-scrollbars --no-sandbox \
        --window-size="${SIZE%x*},${SIZE#*x}" \
        --virtual-time-budget=4000 \
        --screenshot="$SHOT" "$URL" >/dev/null 2>&1 || return 1
      ;;
    desktop)
      if [[ -n "${DISPLAY:-}" ]] && command -v import >/dev/null 2>&1; then
        # X11 这条路截的是整个屏，前提是屏上只有被测应用；真桌面上会把别的窗口一起推上公开仓库。
        import -window root -display "$DISPLAY" "$SHOT" || return 1
      elif [[ -n "${WAYLAND_DISPLAY:-}" ]] && command -v grim >/dev/null 2>&1; then
        local geo
        geo="$(hyprctl activewindow -j 2>/dev/null \
          | python3 -c 'import json,sys; w=json.load(sys.stdin); x,y=w["at"]; ww,wh=w["size"]; print(f"{x},{y} {ww}x{wh}")' 2>/dev/null)"
        if [[ -n "$geo" ]]; then
          grim -g "$geo" "$SHOT" || return 1
        else
          echo "警告：拿不到活动窗口几何，改为截整个输出（会包含你桌面上的其它内容）" >&2
          grim "$SHOT" || return 1
        fi
      else
        echo "没有可用的显示（X11 需要 DISPLAY + imagemagick，Wayland 需要 grim）" >&2
        return 2
      fi
      ;;
    *)
      echo "用法：$0 {desktop|mobile|web} [选项]" >&2
      return 2
      ;;
  esac
}

if ! capture; then
  rc=$?
  [[ $rc -eq 0 ]] && rc=1
  exit "$rc"
fi
echo "截图：$SHOT"

# ---------------------------------------------------------------------------
# 上传（孤儿分支，不进默认分支的历史）
# ---------------------------------------------------------------------------
if [[ $UPLOAD -eq 0 ]]; then
  echo "（未上传。要贴进 PR 就加 --upload —— 这会把它推到公开仓库。）"
  exit 0
fi

REPO_URL="$(git -C "$ROOT" remote get-url origin 2>/dev/null)" || { echo "没有 origin" >&2; exit 1; }
case "$REPO_URL" in
  git@github.com:*)       REPO="${REPO_URL#git@github.com:}"; REPO="${REPO%.git}" ;;
  https://github.com/*)   REPO="${REPO_URL#https://github.com/}"; REPO="${REPO%.git}" ;;
  *) echo "认不出 origin 的形态：$REPO_URL" >&2; exit 1 ;;
esac

BRANCH="$(git -C "$ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo detached)"
SLUG="$(printf '%s' "$BRANCH" | tr '/ ' '--' | tr -cd 'A-Za-z0-9._-')"
ASSETS_BRANCH="auto-assets"
DEST="shots/$SLUG/$NAME.png"

AUTHOR_NAME="$(git -C "$ROOT" config user.name || echo 'Tachi Autopilot')"
AUTHOR_EMAIL="$(git -C "$ROOT" config user.email || echo "noreply@users.noreply.github.com")"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

git -C "$TMP" init -q
git -C "$TMP" remote add origin "$REPO_URL"
if git -C "$TMP" fetch -q --depth 1 origin "$ASSETS_BRANCH" 2>/dev/null; then
  git -C "$TMP" checkout -q FETCH_HEAD
fi
mkdir -p "$TMP/$(dirname "$DEST")"
cp "$SHOT" "$TMP/$DEST"
git -C "$TMP" add "$DEST"
git -C "$TMP" -c user.name="$AUTHOR_NAME" -c user.email="$AUTHOR_EMAIL" \
  commit -qm "shot: $DEST" || { echo "没有变化可提交" >&2; exit 1; }
# 不 --force：远端历史与本地分叉时宁可失败，也不要覆盖已经贴出去的图。
if ! git -C "$TMP" push -q origin "HEAD:$ASSETS_BRANCH"; then
  echo "上传失败（远端 $ASSETS_BRANCH 可能已经分叉）" >&2
  exit 1
fi

RAW="https://raw.githubusercontent.com/$REPO/$ASSETS_BRANCH/$DEST"
echo ""
echo "贴进 PR body 的这一行："
echo ""
echo "![$NAME]($RAW)"
