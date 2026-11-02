#!/usr/bin/env bash
#
# 两台机器抢同一件活时的占位。
#
#   scripts/autopilot/claim.sh take <n>           试着占住 issue / PR n；0=归你了，1=别人在干
#   scripts/autopilot/claim.sh status <n>         现在谁占着
#   scripts/autopilot/claim.sh release <n> [说明]  干完了或放下了，让出来
#
# 为什么要占位：两台机器共用同一个 GitHub App 身份（tachi-autopilot[bot]），PR 和评论的作者
# 一模一样，GitHub 上分不清这条是这台机器还是那台发的。所以「我是不是已经有人在做」这件事
# 只能显式写在被抢的那件活上，并且带上机器号。
#
# 为什么是评论：打标签、发评论在 GitHub 上都不是原子的（没有 compare-and-swap）。唯一能用的
# 办法是「两边都写，再读回来比」——按评论 id 最小者赢，id 在 GitHub 上单调递增，两台机器读
# 到的顺序一致。输的那台删掉自己那条评论走人。
#
# 占位只管「同时」：干完要 release；忘了也不要紧，超过租约（默认 120 分钟，AUTOPILOT_LEASE_MINUTES
# 可覆盖）的占位允许被接管。没有过期机制的锁，第一次崩机器就会把那件活永久封死。
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GH="$HERE/gh"
LEASE_MIN="${AUTOPILOT_LEASE_MINUTES:-120}"
LABEL="in-progress"

die() { echo "claim: $*" >&2; exit 1; }

# machine_name —— 这条占位是谁打的：就是 hostname。
# 两台机器同名的话占位会互相认成自己，所以装机时要保证 hostname 各不相同（见 autopilot.md §7）。
# 名字会进公开仓库的评论，只写主机名，别写路径、用户名。
machine_name() {
  local h
  h="$(hostname -s 2>/dev/null || hostname 2>/dev/null)"
  printf '%s' "${h:-unknown}" | tr -s '[:space:]' '-' | tr -cd 'A-Za-z0-9._-'
}

# events —— 拉该 issue 的全部评论，交给 python 算占位状态。
# 输出一行：<状态>\t<占位者>\t<占位时间>\t<评论 id>
# 状态：free（没人占）/ own（是我占着）/ stale（占位已过期，可接管）/ busy（别人正占着）
events() {
  local n="$1"
  local raw
  raw="$("$GH" api "repos/{owner}/{repo}/issues/$n/comments" --paginate --slurp)" \
    || die "读不到 #$n 的评论"
  CLAIM_EVENTS="$raw" CLAIM_ME="$MACHINE" CLAIM_NOW="$(date +%s)" \
    CLAIM_LEASE_MIN="$LEASE_MIN" python3 - <<'PY'
import json, os, re, sys

MARK = re.compile(r"<!--\s*autopilot kind=(claim|takeover|release) machine=(\S+) at=(\d+)\s*-->")
me = os.environ["CLAIM_ME"]
now = int(os.environ["CLAIM_NOW"])
lease = int(os.environ["CLAIM_LEASE_MIN"]) * 60

pages = json.loads(os.environ["CLAIM_EVENTS"])
comments = [c for page in pages for c in page] if pages and isinstance(pages[0], list) else pages

events = []
for c in sorted(comments, key=lambda c: c["id"]):
    m = MARK.search(c.get("body") or "")
    if not m:
        continue
    events.append({"kind": m.group(1), "machine": m.group(2), "at": int(m.group(3)), "id": c["id"]})

# 状态机：release 清空；takeover 夺取；claim 只在空位时占。
# 于是赢家 = 最后一条 release 之后、最后一条 takeover 起的第一个事件。
holder = None
for e in events:
    if e["kind"] == "release":
        holder = None
    elif e["kind"] == "takeover":
        holder = e
    elif e["kind"] == "claim" and holder is None:
        holder = e

if holder is None:
    print("free\t-\t0\t0")
elif holder["machine"] == me:
    print("own\t%s\t%d\t%d" % (holder["machine"], holder["at"], holder["id"]))
elif holder["at"] + lease <= now:
    print("stale\t%s\t%d\t%d" % (holder["machine"], holder["at"], holder["id"]))
else:
    print("busy\t%s\t%d\t%d" % (holder["machine"], holder["at"], holder["id"]))
PY
}

# comment_body <kind> <at> <extra> —— 一条占位评论。上面那行 HTML 注释是机器读的，
# 下面那行是给人看的（issue 是公开的，别写路径和 token）。
comment_body() {
  local kind="$1" at="$2" extra="$3" human
  case "$kind" in
    claim)    human="占位：$MACHINE 接手处理（$(date -r "$at" '+%m-%d %H:%M' 2>/dev/null || date -d "@$at" '+%m-%d %H:%M')）" ;;
    takeover) human="接管：$MACHINE 接手（原来的占位超过 ${LEASE_MIN} 分钟没有动静）" ;;
    release)  human="释放：$MACHINE 已让出" ;;
  esac
  printf '<!-- autopilot kind=%s machine=%s at=%s -->\n%s%s\n' \
    "$kind" "$MACHINE" "$at" "$human" "${extra:+（${extra}）}"
}

post_comment() {
  "$GH" api "repos/{owner}/{repo}/issues/$1/comments" -f body="$2" --jq .id
}

drop_comment() {
  "$GH" api -X DELETE "repos/{owner}/{repo}/issues/comments/$1" >/dev/null 2>&1 || true
}

ensure_label() {
  "$GH" label create "$LABEL" --color FBCA04 \
    --description "autopilot 正在处理（见 scripts/autopilot/claim.sh）" --force >/dev/null 2>&1 || true
}

set_label() {
  local n="$1" want="$2"
  ensure_label
  if [[ "$want" == add ]]; then
    "$GH" issue edit "$n" --add-label "$LABEL" >/dev/null 2>&1 || true
  else
    "$GH" issue edit "$n" --remove-label "$LABEL" >/dev/null 2>&1 || true
  fi
}

cmd_take() {
  local n="${1:-}"
  [[ -n "$n" ]] || die "用法：$0 take <issue 或 PR 号>"

  local state holder at cid
  IFS=$'\t' read -r state holder at cid < <(events "$n")
  [[ -n "$state" ]] || die "算不出 #$n 的占位状态"

  case "$state" in
    own)
      echo "#$n 已经是 $MACHINE 在占着" >&2
      set_label "$n" add
      return 0
      ;;
    busy)
      echo "#$n 被 $holder 占着（$(date -r "$at" '+%m-%d %H:%M' 2>/dev/null || date -d "@$at" '+%m-%d %H:%M')），换一件" >&2
      return 1
      ;;
  esac

  # free / stale：写下自己的占位，然后读回来。两边同时写时，id 小的赢。
  local kind="claim" extra=""
  [[ "$state" == stale ]] && kind="takeover" && extra="原占位者 $holder"
  local now mine
  now="$(date +%s)"
  mine="$(post_comment "$n" "$(comment_body "$kind" "$now" "$extra")")" \
    || die "写不进占位评论"
  [[ -n "$mine" ]] || die "写不进占位评论"

  IFS=$'\t' read -r state holder at cid < <(events "$n")
  if [[ "$cid" == "$mine" ]]; then
    echo "#$n 占位成功：$MACHINE" >&2
    set_label "$n" add
    return 0
  fi

  # 输了：删掉自己那条，把位置留给赢家。标签不动 —— 那是赢家打的（或下一轮它会打）。
  drop_comment "$mine"
  echo "#$n 被 $holder 抢到了（id 更早），换一件" >&2
  return 1
}

cmd_status() {
  local n="${1:-}"
  [[ -n "$n" ]] || die "用法：$0 status <issue 或 PR 号>"
  local state holder at cid
  IFS=$'\t' read -r state holder at cid < <(events "$n")
  case "$state" in
    free)  echo "#$n 没人占" ;;
    own)   echo "#$n 是 $MACHINE 占着" ;;
    stale) echo "#$n 占位已过期：${holder}（$(date -r "$at" '+%m-%d %H:%M' 2>/dev/null || date -d "@$at" '+%m-%d %H:%M')），可以接管" ;;
    busy)  echo "#$n 被 $holder 占着（$(date -r "$at" '+%m-%d %H:%M' 2>/dev/null || date -d "@$at" '+%m-%d %H:%M')）" ;;
  esac
}

cmd_release() {
  local n="${1:-}" note="${2:-}"
  [[ -n "$n" ]] || die "用法：$0 release <issue 或 PR 号> [说明]"
  post_comment "$n" "$(comment_body release "$(date +%s)" "$note")" >/dev/null \
    || die "写不进释放评论"
  set_label "$n" remove
  echo "#$n 已让出" >&2
}

MACHINE="$(machine_name)"

case "${1:-}" in
  take)    shift; cmd_take "$@" ;;
  status)  shift; cmd_status "$@" ;;
  release) shift; cmd_release "$@" ;;
  *)       sed -n '3,9p' "${BASH_SOURCE[0]}"; exit 1 ;;
esac
