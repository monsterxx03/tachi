#!/usr/bin/env bash
#
# 跑手机端的端到端流程。
#
# **每条 flow 之前都要把 mockapi 重启一遍**：flow 会消耗掉种子数据（main 点掉那张待答卡，
# compact 造出一个子会话），不重置的话第二次跑就找不到它了。
#
#   bash mobile/e2e/run.sh [设备序列号]
set -euo pipefail

device="${1:-emulator-5554}"
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"

# JDK 17：优先用环境里的 JAVA_HOME；否则 macOS 问 java_home，Linux 从 PATH 里的 java 反推，
# 最后认本仓库文档里那个 Linux 位置（~/.local/share/jdk/current）。都找不到就停下来说清楚。
resolve_java_home() {
  if [[ -x /usr/libexec/java_home ]]; then
    /usr/libexec/java_home -v 17
  elif command -v java >/dev/null 2>&1; then
    dirname "$(dirname "$(readlink -f "$(command -v java)")")"
  elif [[ -d $HOME/.local/share/jdk/current ]]; then
    echo "$HOME/.local/share/jdk/current"
  fi
}

export JAVA_HOME="${JAVA_HOME:-$(resolve_java_home)}"
if [[ ! -x $JAVA_HOME/bin/java ]]; then
  echo "找不到可用的 JDK 17（JAVA_HOME=${JAVA_HOME:-空}）：装一个，或 export JAVA_HOME 指过去" >&2
  exit 1
fi
# maestro 是 Java 程序，它按 PATH 里的 java 认版本；只设 JAVA_HOME 它会报「Java 17 or higher is required」
export PATH="$JAVA_HOME/bin:${ANDROID_HOME:-$HOME/Library/Android/sdk}/platform-tools:$PATH"

reset_mock() {
  echo "== 重置 mock 服务 =="
  # 只杀监听方 —— `lsof -ti:9797` 会把连着它的模拟器一起列出来
  kill "$(lsof -ti:9797 -sTCP:LISTEN)" 2>/dev/null || true
  sleep 1
  (cd "$repo" && go run ./mobile/mockapi -token dev-token > /tmp/mockapi.log 2>&1 &)
  for _ in $(seq 1 30); do
    curl -sf --max-time 1 http://127.0.0.1:9797/v1/ping >/dev/null && break
    sleep 0.5
  done
}

for flow in main.yaml compact.yaml; do
  reset_mock
  echo "== 跑 flow: $flow =="
  "$HOME/.local/maestro/bin/maestro" --device "$device" test "$here/$flow"
done
