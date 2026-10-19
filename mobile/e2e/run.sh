#!/usr/bin/env bash
#
# 跑手机端的端到端流程。
#
# 每次都要把 mockapi 重启一遍：flow 会消耗掉种子数据里的那张待答卡（点「允许一次」），
# 不重置的话第二次跑就找不到它了。
#
#   bash mobile/e2e/run.sh [设备序列号]
set -euo pipefail

device="${1:-emulator-5554}"
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"

export JAVA_HOME="${JAVA_HOME:-$(/usr/libexec/java_home -v 17)}"
export PATH="$HOME/Library/Android/sdk/platform-tools:$PATH"

echo "== 重置 mock 服务 =="
# 只杀监听方 —— `lsof -ti:9797` 会把连着它的模拟器一起列出来
kill "$(lsof -ti:9797 -sTCP:LISTEN)" 2>/dev/null || true
sleep 1
(cd "$repo" && go run ./mobile/mockapi -token dev-token > /tmp/mockapi.log 2>&1 &)
for _ in $(seq 1 30); do
  curl -sf --max-time 1 http://127.0.0.1:9797/v1/ping >/dev/null && break
  sleep 0.5
done

echo "== 跑 flow =="
"$HOME/.local/maestro/bin/maestro" --device "$device" test "$here/main.yaml"
