#!/usr/bin/env bash
#
# 无人值守改动的一道确定性门禁。
#
# 用法（在 worktree 里）：
#
#   scripts/autopilot/verify.sh              # 按本分支相对 origin/main 的改动决定跑什么
#   scripts/autopilot/verify.sh --base HEAD~1
#   scripts/autopilot/verify.sh --dry-run    # 只说要跑什么，不跑
#
# 为什么是脚本而不是提示词：跑哪层由 diff 决定，不由模型临场决定。模型可以撒谎说
# 「测试过了」，退出码不能；而「该跑的没跑」必须是一个响亮的失败，不能安静地算绿。
#
# 退出码：
#   0  —— 该跑的跑了且全过（含「只改了文档，无需重验证」）
#   1  —— 有东西没通过
#   2  —— 环境不具备，某层**没跑成**。这不是通过：PR 里必须写明哪层没验。
#
# 本脚本只读仓库、只往仓库内的既有构建产物目录写，绝不碰 ~/.tachi。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BASE="origin/main"
DRY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --base) BASE="${2:?--base 需要一个 ref}"; shift 2 ;;
    --dry-run) DRY=1; shift ;;
    -h|--help) sed -n '2,20p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "未知参数：$1" >&2; exit 1 ;;
  esac
done

cd "$ROOT" || exit 1

# ---------------------------------------------------------------------------
# 结果收集
# ---------------------------------------------------------------------------
declare -a SUMMARY=()
FAILED=0
UNVERIFIED=0

record() { SUMMARY+=("$1"); }
pass()   { record "PASS  $1"; }
skip()   { record "SKIP  $1  — $2"; }
fail()   { record "FAIL  $1"; FAILED=1; }
nover()  { record "UNVERIFIED  $1  — $2"; UNVERIFIED=1; }

run() {
  local label="$1"; shift
  if [[ $DRY -eq 1 ]]; then
    record "WOULD RUN  $label  —  $*"
    return 0
  fi
  echo ""
  echo "=== $label"
  echo "    \$ $*"
  local start=$SECONDS
  if "$@"; then
    pass "${label}（$((SECONDS - start))s）"
    return 0
  fi
  fail "$label"
  return 1
}

# ---------------------------------------------------------------------------
# 改动是什么
# ---------------------------------------------------------------------------
if ! git rev-parse --verify --quiet "$BASE" >/dev/null; then
  echo "找不到基线 ref：${BASE}（先 git fetch origin，或用 --base 指定）" >&2
  exit 2
fi

MERGE_BASE="$(git merge-base "$BASE" HEAD 2>/dev/null)" || MERGE_BASE="$BASE"
CHANGED="$(git diff --name-only "$MERGE_BASE" HEAD; git diff --name-only; git diff --cached --name-only)"
CHANGED="$(printf '%s\n' "$CHANGED" | sed '/^$/d' | sort -u)"

if [[ -z "$CHANGED" ]]; then
  echo "相对 $BASE 没有任何改动，没什么可验证的。" >&2
  exit 1
fi

echo "基线：${BASE}（merge-base ${MERGE_BASE}）"
echo "注意：基线是本地记录的远端。开工前没 git fetch origin main 的话，你比的是旧的 main。"
echo "改动：$(printf '%s\n' "$CHANGED" | wc -l) 个文件"
printf '%s\n' "$CHANGED" | sed 's/^/  /'

# docs/ 与纯说明文件不带动任何东西。其余任何路径都算「动了代码」。
CODE_CHANGED="$(printf '%s\n' "$CHANGED" \
  | grep -vE '^(docs/|\.github/|\.tachi\.md$|README|LICENSE|.*\.md$)' || true)"

# ---------------------------------------------------------------------------
# 这台机器能接什么活
# ---------------------------------------------------------------------------
# 共用同一个 issue 队列的两台机器能力不一样：一台弱的 VPS 跑不动 smoke / e2e，它就不该接界面活。
# 边界写在这个仓库的公共 git 目录里（.git/autopilot-no-ui，装机时手动放，内容随便写一句人话），
# 不在提示词里——提示词会被遗忘，脚本不会。
#
# 为什么是硬失败而不是 UNVERIFIED：交出一个「界面没验过」的 PR，等于把风险丢给看 PR 的人；
# 宁可这一轮白跑，也要让它在这里响亮地停下来。
LIMITS_FILE="$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null || echo .git)/autopilot-no-ui"
if [[ -f "$LIMITS_FILE" ]]; then
  BLOCKED="$(printf '%s\n' "$CHANGED" | grep -E '^(desktop/|mobile/|web/frontend/src/)' || true)"
  if [[ -n "$BLOCKED" ]]; then
    echo ""
    echo "这台机器不接界面活（$LIMITS_FILE = $(head -c 200 "$LIMITS_FILE" | tr -d '\n')）："
    printf '%s\n' "$BLOCKED" | sed 's/^/  /'
    echo "换一台能跑 smoke / e2e 的机器做，或者把这件留在原 issue 上。"
    exit 1
  fi
fi

if [[ -z "$CODE_CHANGED" ]]; then
  skip "核心门禁" "只改了文档/说明文件"
else
  # ---------------------------------------------------------------- 脚本语法：门禁自己也在里面
  # 没有这一层，改坏 scripts/autopilot/*.sh 只会触发 lint + unit tests，而这两层看不见 bash。
  # bash -n 只能证语法，证不了逻辑——所以「PR 碰了 scripts/autopilot/」这件事本身要人看。
  # 按有没有 shebang 认，而不是按 .sh 后缀：脚本不一定带后缀（autopilot/gh 就是个没有后缀的可执行
  # 文件），靠后缀的那道门禁会静默放过它。
  SH_CHANGED="$(printf '%s\n' "$CHANGED" | while IFS= read -r f; do
    [[ -f "$f" ]] && [[ "$(head -c 2 "$f" 2>/dev/null)" == '#!' ]] && echo "$f"
  done)"
  shell_syntax() {
    local f rc=0
    while IFS= read -r f; do
      [[ -z "$f" ]] && continue
      echo "    bash -n $f"
      bash -n "$f" || rc=1
    done <<< "$SH_CHANGED"
    return $rc
  }
  if [[ -n "$SH_CHANGED" ]]; then
    run "shell syntax" shell_syntax
  else
    skip "shell syntax" "没改 .sh"
  fi

  # ---------------------------------------------------------------- 核心：永远要过
  run "lint" make lint
  run "unit tests" make test

  # ---------------------------------------------------------------- 数据竞争：只有 -race 看得见
  # `make test` 是不带 -race 的：竞争在那里完全不可见（这个仓库已经有过两条，都是会起 goroutine
  # 的包里那些「不报错的错」）。只跑那几个包——全量 -race 大概翻倍，而这几个是竞争住的地方。
  if printf '%s\n' "$CODE_CHANGED" \
      | grep -qE '^(agent/|node/|session/|channel/|cron/|mobile/|pkg/)'; then
    run "race" go test -race ./agent/... ./node/ ./session/ ./channel/... ./cron/ ./mobile/mockapi/
  else
    skip "race" "没碰会起 goroutine 的包"
  fi

  # ---------------------------------------------------------------- 集成：改了共用面
  if printf '%s\n' "$CODE_CHANGED" \
      | grep -qE '^(agent/|config/|llm/|node/|channel/|cron/|dream/|session/|pkg/|tui/|web/|itest/|main[^/]*\.go$|go\.(mod|sum)$)'; then
    run "itest (headless)" make itest-headless
  else
    skip "itest (headless)" "没碰共用面"
  fi

  # ---------------------------------------------------------------- 桌面：模块测试 + 真窗口
  if printf '%s\n' "$CODE_CHANGED" | grep -q '^desktop/'; then
    run "desktop unit tests" make -C desktop test
    if [[ -n "${DISPLAY:-}" || -n "${WAYLAND_DISPLAY:-}" ]]; then
      run "desktop smoke" make desktop-smoke
    else
      nover "desktop smoke" "没有显示器 —— 这层没跑过，别在 PR 里说验过"
    fi
  else
    skip "desktop" "没碰 desktop/"
  fi

  # ---------------------------------------------------------------- 手机：e2e
  if printf '%s\n' "$CODE_CHANGED" | grep -qE '^(mobile/|desktop/mobile)'; then
    if command -v adb >/dev/null 2>&1 && [[ -n "$(adb devices | awk 'NR>1 && $2=="device"{print $1; exit}')" ]]; then
      run "mobile e2e" bash mobile/e2e/run.sh
    else
      nover "mobile e2e" "没有可用的 adb 设备（先起 emulator -no-window）"
    fi
  else
    skip "mobile" "没碰 mobile/"
  fi
fi

# ---------------------------------------------------------------------------
# 结论
# ---------------------------------------------------------------------------
echo ""
echo "================ 验证结论 ================"
printf '%s\n' "${SUMMARY[@]}"
echo "=========================================="

# 界面文件动了就一定要有人看一眼「变了没有、变成什么样」。脚本判断不了这只是重构还是
# 真的改了显示，所以它只负责提醒，不负责判绿：PR body 里没有改动后的截图，就是没做完。
UI_CHANGED="$(printf '%s\n' "$CHANGED" \
  | grep -E '^(desktop/frontend/|mobile/(src/|assets/|App\.tsx)|web/frontend/src/)' || true)"
if [[ -n "$UI_CHANGED" ]]; then
  echo ""
  echo "CHECK SHOT  改到了界面文件："
  printf '%s\n' "$UI_CHANGED" | sed 's/^/  /'
  echo "  如果显示效果有变化，PR 必须附改动后的截图 —— scripts/autopilot/screenshot.sh。"
  echo "  截图只能来自 sandbox / mockapi 的假数据：这是公开仓库，别把你的真实会话截进去。"
fi

if [[ $DRY -eq 1 ]]; then
  echo "（--dry-run：什么都没跑）"
  exit 0
fi
if [[ $FAILED -eq 1 ]]; then
  echo "有东西没通过。修它，别改门禁。"
  exit 1
fi
if [[ $UNVERIFIED -eq 1 ]]; then
  echo "有层没验成。别声称全绿：在 PR body 里写清哪层没跑、为什么。"
  exit 2
fi
echo "全绿。"
exit 0
