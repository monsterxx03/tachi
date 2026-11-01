#!/usr/bin/env bash
#
# git 的凭证助手：只用一处——autopilot 的工作区往 GitHub 推的时候。
#
# 用法由 git 自己调（工作区里由 worktree.sh new/resume 配好），不需要手动执行：
#
#   git push        →  git 问「你是谁」→ 这个脚本回答 x-access-token + bot 的安装令牌
#
# 只回答 github.com 的 https 请求，别的一律沉默放行（空回答 = 我不知道），免得把令牌交给别的主机。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

action="${1:-}"
[[ "$action" == "get" ]] || exit 0

protocol="" host=""
while IFS= read -r line; do
  [[ -z "$line" ]] && break
  case "$line" in
    protocol=*) protocol="${line#protocol=}" ;;
    host=*) host="${line#host=}" ;;
  esac
done

[[ "$protocol" == "https" && "$host" == "github.com" ]] || exit 0

printf 'username=x-access-token\npassword=%s\n' "$("$HERE/bot-token.sh")"
