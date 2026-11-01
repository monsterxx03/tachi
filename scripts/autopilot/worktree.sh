#!/usr/bin/env bash
#
# 自动工作区的开与收。
#
#   scripts/autopilot/worktree.sh new <slug>              从最新 origin/main 开区并备好依赖
#   scripts/autopilot/worktree.sh resume <slug>           接着上一轮的 PR 改（区在就用，不在就重建）
#   scripts/autopilot/worktree.sh gc [--dry-run] [--max-age <天>]
#   scripts/autopilot/worktree.sh list
#
# `new` 干两件事，第二件是省钱：
#   1. git worktree add --no-track -b auto/<slug> <root>/<slug> origin/main
#   2. 把两个前端目录的 node_modules **符号链接**到主工作树，再在本 worktree 里各跑一次
#      build。npm ci 是每轮最贵的一步（两个前端加起来近 500MB），而依赖与源码无关：lock
#      一致时它和主仓那份完全相同，共享即可。build 必须在本 worktree 做 —— 产物要反映这
#      一分支的前端源码，复制主仓的 dist 会让门禁看不见前端改动。
#   lock 不一致时不链接，让 `make` 自己 npm ci：共享一份对不上 lock 的依赖比慢更糟。
#
# `gc` 回收 `new` 开的区。判据只有一条：**没有 open PR，且最后一次提交早于 --max-age 天**。
# 有未提交/未跟踪改动的一律跳过 —— 不能因为「看起来像残留」就吞掉别人的活。
# 永远不碰主工作树，也永远不删远端分支（合并后删头分支交给 GitHub 自己）。
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# 脚本可能是在某个 worktree 里被调用的，所以主工作树从 git 的公共目录反推，不能靠 dirname。
COMMON_DIR="$(git -C "$SCRIPT_DIR" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)" \
  || { echo "不在一个 git 仓库里" >&2; exit 1; }
MAIN_WT="$(dirname "$COMMON_DIR")"
# 主工作树的兄弟目录：~/repos/tachi → ~/repos/tachi-ap
WT_ROOT="${AUTOPILOT_WT_ROOT:-$(dirname "$MAIN_WT")/tachi-ap}"
BRANCH_PREFIX="auto/"
MAX_AGE_DAYS=3
DRY=0

die() { echo "$*" >&2; exit 1; }

# ensure_exclude —— 让共享进来的 node_modules 不被当成未跟踪文件。
# 仓库的 .gitignore 写的是 `node_modules/`（带斜杠，只匹配目录），而这里放的是**符号链接**，
# 对 git 来说它是个文件，匹配不上 —— 不管的话 gc 会以为每个区都有未提交改动。
# 写在 info/exclude 里而不是 .gitignore：这是这台机器上「谁链接了依赖」的局部事实，
# info/exclude 在公共 .git 目录下，所有 worktree 共享，且不进仓库历史。
ensure_exclude() {
  local f="$COMMON_DIR/info/exclude" need=0 line
  mkdir -p "$COMMON_DIR/info"
  touch "$f"
  for line in web/frontend/node_modules desktop/frontend/node_modules; do
    grep -qxF "$line" "$f" || need=1
  done
  [[ $need -eq 1 ]] || return 0
  {
    echo ""
    echo "# autopilot: 前端依赖用符号链接共享（见 scripts/autopilot/worktree.sh）"
    echo "web/frontend/node_modules"
    echo "desktop/frontend/node_modules"
  } >> "$f"
}

# link_modules <worktree> <relpath> —— 把主仓的 node_modules 链接进来，前提是 lock 完全一致。
link_modules() {
  local wt="$1" rel="$2" src dst
  src="$MAIN_WT/$rel/node_modules"
  dst="$wt/$rel/node_modules"
  [[ -d "$src" ]] || { echo "    $rel: 主仓没有 node_modules，跳过（留给 npm ci）"; return 0; }
  if ! cmp -s "$MAIN_WT/$rel/package-lock.json" "$wt/$rel/package-lock.json"; then
    echo "    $rel: package-lock 与主仓不同，不共享依赖（让 make 自己 npm ci）"
    return 0
  fi
  rm -rf "$dst"
  ln -s "$src" "$dst"
  echo "    $rel: node_modules → 主仓（只读共享）"
}

# build_frontend <worktree> <relpath> —— 用共享依赖在本 worktree 里生成 dist。
build_frontend() {
  local wt="$1" rel="$2"
  if [[ -f "$wt/$rel/dist/index.html" ]]; then
    echo "    $rel: dist 已在"
    return 0
  fi
  [[ -d "$wt/$rel/node_modules" ]] || { echo "    $rel: 没有依赖，跳过 build（留给 web-check）"; return 0; }
  echo "    $rel: 构建 dist"
  ( cd "$wt/$rel" && npm run build >/dev/null 2>&1 ) \
    || echo "    $rel: build 失败 —— 交还给 make 去报错"
}

# prepare_bot_identity <worktree> —— 让这个区里的提交和推送都以 bot 的身份发生。
#
# 三件事都写进这个 worktree 自己的 config（extensions.worktreeConfig），主工作树不受影响：
#   1. 作者名/邮箱 = GitHub App 的 bot 账号（邮箱必须是它的 noreply 地址，GitHub 才会把提交认到它名下）
#   2. 推送走 https，不走主人的 SSH key —— 那条路一用就是主人的身份
#   3. 凭证助手只回答 github.com，答的是 App 现换的令牌（见 bot-token.sh）
#
# 拿不到 bot 身份就硬失败：宁可这一轮不开区，也不要出一批挂在主人名下、他自己都没印象的提交。
prepare_bot_identity() {
  local wt="$1" ident name email url slug
  ident="$("$SCRIPT_DIR/bot-token.sh" --identity)" \
    || die "拿不到 bot 身份（见 scripts/autopilot/bot-token.sh），拒绝开区"
  name="${ident%% <*}"; email="${ident#*<}"; email="${email%>}"

  url="$(git -C "$wt" remote get-url origin)"
  slug="$(printf '%s' "$url" | sed -E 's#^git@github\.com:##; s#^https?://github\.com/##; s#\.git$##')"
  [[ "$slug" == */* ]] || die "看不懂 origin 地址：$url"

  git -C "$MAIN_WT" config extensions.worktreeConfig true
  git -C "$wt" config --worktree user.name "$name"
  git -C "$wt" config --worktree user.email "$email"
  git -C "$wt" config --worktree remote.origin.pushurl "https://github.com/$slug.git"
  # 先清掉继承来的助手（它答的可能是主人自己的凭证），再挂上我们的。
  git -C "$wt" config --worktree --replace-all credential.helper ""
  git -C "$wt" config --worktree --add credential.helper "!$SCRIPT_DIR/git-credential.sh"
  echo "    身份：$name"
}

cmd_new() {
  local slug="${1:-}"
  [[ -n "$slug" ]] || die "用法：$0 new <slug>"
  slug="$(printf '%s' "$slug" | tr '/ ' '--' | tr -cd 'A-Za-z0-9._-')"
  [[ -n "$slug" ]] || die "slug 清理后为空"

  local path="$WT_ROOT/$slug" branch="${BRANCH_PREFIX}${slug}"
  [[ -e "$path" ]] && die "目标已存在：${path}（先 $0 gc，或换个 slug）"
  git -C "$MAIN_WT" show-ref --verify --quiet "refs/heads/$branch" && die "本地分支已存在：$branch"

  echo "== fetch origin main"
  git -C "$MAIN_WT" fetch -q origin main || die "fetch 失败"
  echo "== 开 worktree：${path}（分支 ${branch}，基于 origin/main）"
  # --no-track：起点是远端分支时 git 会把上游设成 origin/main，那会让 pull 把 main 合进来。
  git -C "$MAIN_WT" worktree add --no-track -b "$branch" "$path" origin/main >/dev/null \
    || die "worktree 创建失败"

  echo "== 复用依赖"
  ensure_exclude
  prepare_bot_identity "$path"
  link_modules "$path" web/frontend
  link_modules "$path" desktop/frontend
  build_frontend "$path" web/frontend
  build_frontend "$path" desktop/frontend

  echo ""
  echo "$path"
}

cmd_gc() {
  local force=0
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --dry-run) DRY=1; shift ;;
      --force) force=1; shift ;;
      --max-age) MAX_AGE_DAYS="${2:?}"; shift 2 ;;
      *) die "未知参数：$1" ;;
    esac
  done

  # --max-age 0 等于「删掉所有没有 open PR 的区」——包括正在被这一轮用的那个。
  # 它是个有用的开关（合并后立刻收工），但绝不该是手滑的默认。
  if [[ "$MAX_AGE_DAYS" -lt 1 && $force -eq 0 ]]; then
    die "--max-age 小于 1 天会删掉所有没有 open PR 的区（含刚建的）。确认的话加 --force。"
  fi

  [[ -d "$WT_ROOT" ]] || { echo "没有需要回收的工作区（$WT_ROOT 不存在）"; return 0; }
  ensure_exclude

  # 一次问到所有 open PR 的分支名，避免每个目录一次 gh 调用。
  local open_branches
  open_branches="$(gh pr list --state open --limit 200 --json headRefName -q '.[].headRefName' 2>/dev/null || true)"

  local now cutoff removed=0 kept=0
  now="$(date +%s)"
  cutoff=$(( now - MAX_AGE_DAYS * 86400 ))

  local path branch commit_ts age_days
  while IFS= read -r path; do
    [[ -n "$path" && -d "$path" ]] || continue
    branch="$(git -C "$path" rev-parse --abbrev-ref HEAD 2>/dev/null)" || continue
    commit_ts="$(git -C "$path" log -1 --format=%ct 2>/dev/null || echo "$now")"
    age_days=$(( (now - commit_ts) / 86400 ))

    if [[ -n "$(git -C "$path" status --porcelain 2>/dev/null)" ]]; then
      echo "  keep   ${branch}（有未提交改动，${age_days}天）"; kept=$((kept+1)); continue
    fi
    if printf '%s\n' "$open_branches" | grep -qxF "$branch"; then
      echo "  keep   ${branch}（有 open PR，${age_days}天）"; kept=$((kept+1)); continue
    fi
    if [[ "$commit_ts" -gt "$cutoff" ]]; then
      echo "  keep   ${branch}（${age_days}天，未到 $MAX_AGE_DAYS 天）"; kept=$((kept+1)); continue
    fi

    if [[ $DRY -eq 1 ]]; then
      echo "  would  remove ${branch}（${age_days}天，无 open PR）"
      continue
    fi

    echo "  remove ${branch}（${age_days}天，无 open PR）"
    # 先摘掉依赖链接：它们指向主仓，删目录时留着没有意义。用 -rf 是因为这里既可能是
    # 符号链接（共享来的，只摘链接本身），也可能是 npm ci 装出来的真目录（一并删掉）。
    rm -rf "$path/web/frontend/node_modules" "$path/desktop/frontend/node_modules"
    if ! git -C "$MAIN_WT" worktree remove "$path" >/dev/null 2>&1; then
      echo "    worktree remove 失败，跳过"; kept=$((kept+1)); continue
    fi
    git -C "$MAIN_WT" branch -D "$branch" >/dev/null 2>&1 || true
    removed=$((removed+1))
  done < <(list_worktrees_under_root)

  # 目录被手工删掉时，git 那边还留着一条记录。
  git -C "$MAIN_WT" worktree prune

  echo "回收完成：删除 ${removed}，保留 ${kept}。"
}

# list_worktrees_under_root：只输出 WT_ROOT 下的 worktree 路径（主工作树永远不在里面）。
list_worktrees_under_root() {
  git -C "$MAIN_WT" worktree list --porcelain \
    | awk '/^worktree /{p=$2} /^branch /{print p}' \
    | grep -F "$WT_ROOT/" || true
}

cmd_list() {
  git -C "$MAIN_WT" worktree list
}

# cmd_resume —— 接着上一轮的 PR 改时用它：区还在就直接用，不在就从原分支重建。
# 新一轮绝不能换个 slug 重开一个分支 —— 那就成了第二个 PR，评论里的意见全丢在原地。
cmd_resume() {
  local slug="${1:-}"
  [[ -n "$slug" ]] || die "用法：$0 resume <slug>"
  slug="$(printf '%s' "$slug" | tr '/ ' '--' | tr -cd 'A-Za-z0-9._-')"
  local path="$WT_ROOT/$slug" branch="${BRANCH_PREFIX}${slug}"

  if [[ -d "$path" ]]; then
    echo "== 区还在，直接续用"
  else
    echo "== fetch origin"
    git -C "$MAIN_WT" fetch -q origin || die "fetch 失败"
    if git -C "$MAIN_WT" show-ref --verify --quiet "refs/heads/$branch"; then
      git -C "$MAIN_WT" worktree add "$path" "$branch" >/dev/null || die "worktree 重建失败"
    elif git -C "$MAIN_WT" show-ref --verify --quiet "refs/remotes/origin/$branch"; then
      git -C "$MAIN_WT" worktree add -b "$branch" "$path" "origin/$branch" >/dev/null \
        || die "worktree 重建失败"
    else
      die "找不到分支 ${branch}（本地和 origin 都没有）"
    fi
  fi

  ensure_exclude
  prepare_bot_identity "$path"
  link_modules "$path" web/frontend
  link_modules "$path" desktop/frontend
  build_frontend "$path" web/frontend
  build_frontend "$path" desktop/frontend

  echo ""
  echo "$path"
}

case "${1:-}" in
  new)    shift; cmd_new "$@" ;;
  resume) shift; cmd_resume "$@" ;;
  gc)     shift; cmd_gc "$@" ;;
  list)   cmd_list ;;
  *)      sed -n '3,11p' "${BASH_SOURCE[0]}"; exit 1 ;;
esac
