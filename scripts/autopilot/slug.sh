#!/usr/bin/env bash
#
# 一件活的确定性 key。两台机器扫到同一件事，必须算出同一个 slug —— 分支名相同，push 就是
# 天然的互斥（第二台推不上去）；slug 由描述性文字随手起，两台就必然算出两个名字，然后各开
# 一个重复 PR。
#
#   scripts/autopilot/slug.sh issue <编号>                  → issue-123
#   scripts/autopilot/slug.sh <类别> <文件或目录>            → doc-docs-agents-mcp-md
#
# 类别只有四个，和 autopilot.md §3.1 的四类活一一对应：
#   doc     文档对齐
#   dedupe  重复消除
#   bug     小 bug / 小坑
#   test    补测试
#
# 位置写**要改的那个文件或目录**，不要写症状描述。一次要改多个文件的，用最主要的那个
# （或者它们共同的目录）—— 两台机器对「主要」的判断通常一致，对措辞不一致。
set -uo pipefail

die() { echo "slug: $*" >&2; exit 1; }

KINDS="doc dedupe bug test"

# normalize —— 路径换成 slug 片段：小写，界符统一成 '-'，压掉重复，截断。
normalize() {
  printf '%s' "$1" \
    | tr '[:upper:]' '[:lower:]' \
    | sed -E 's#[/\\. _]+#-#g; s#-+#-#g; s#^-##; s#-$##' \
    | tr -cd 'a-z0-9.-' \
    | cut -c1-60
}

case "${1:-}" in
  issue)
    n="${2:-}"
    [[ "$n" =~ ^[0-9]+$ ]] || die "用法：$0 issue <编号>"
    echo "issue-$n"
    ;;
  "")
    die "用法：$0 issue <编号> | $0 <${KINDS// /|}> <文件或目录>"
    ;;
  *)
    kind="$1"
    loc="${2:-}"
    [[ " $KINDS " == *" $kind "* ]] || die "类别只能是：${KINDS}（收到：${kind}）"
    [[ -n "$loc" ]] || die "用法：$0 $kind <文件或目录>"
    part="$(normalize "$loc")"
    [[ -n "$part" ]] || die "位置规范化后为空：$loc"
    echo "$kind-$part"
    ;;
esac
