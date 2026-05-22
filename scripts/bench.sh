#!/usr/bin/env bash
#
# bench.sh — Run Terminal-Bench 2.1 tasks against Tachi
#
# Usage:
#   ./scripts/bench.sh <test-name> [test-name2 ...]
#
#   ./scripts/bench.sh bn-fit-modify
#   ./scripts/bench.sh bn-fit-modify chess-best-move dna-assembly
#   ./scripts/bench.sh --model claude-sonnet-4-20250514 --n-concurrent 2 regex-log
#
# Options (must come BEFORE test names):
#   --model, -m           Model name (e.g. deepseek-v4-pro, claude-sonnet-4-20250514)
#   --n-concurrent, -n    Number of concurrent trials (default: 4)
#   --timeout             Agent timeout per task (default: 15m)
#   --max-iterations      Max iterations per task (default: 50)
#   --no-build            Skip building the Linux binary
#   --delete              Delete containers after run (default: keep)
#   --yes                 Auto-confirm prompts
#   --list, -l            List all available test names
#   --all, -a             Run ALL tests in terminal-bench-2-1/
#
# Examples:
#   # Run a single test with defaults
#   ./scripts/bench.sh bn-fit-modify
#
#   # Run multiple tests with Claude Sonnet 4, 2 concurrent
#   ./scripts/bench.sh -m claude-sonnet-4-20250514 -n 2 regex-log chess-best-move
#
#   # List all available tests
#   ./scripts/bench.sh --list
#
#   # Run ALL tests (warning: many tasks!)
#   ./scripts/bench.sh --all
#
#   # Custom timeout and iterations
#   ./scripts/bench.sh --timeout 30m --max-iterations 100 large-scale-text-editing

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BENCH_DIR="$PROJECT_DIR/terminal-bench-2-1"

# ── Defaults ──────────────────────────────────────────────────────────

MODEL="${TACHI_BENCH_MODEL:-deepseek-v4-pro}"
N_CONCURRENT="${TACHI_N_CONCURRENT:-4}"
TIMEOUT="${TACHI_TIMEOUT:-15m}"
MAX_ITERATIONS="${TACHI_MAX_ITERATIONS:-50}"
SKIP_BUILD=false
DELETE_CONTAINERS=false
AUTO_YES=false
SHOW_LIST=false
RUN_ALL=false

# ── Parse flags ───────────────────────────────────────────────────────

TESTS=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --model|-m)
            MODEL="$2"; shift 2 ;;
        --n-concurrent|-n)
            N_CONCURRENT="$2"; shift 2 ;;
        --timeout)
            TIMEOUT="$2"; shift 2 ;;
        --max-iterations)
            MAX_ITERATIONS="$2"; shift 2 ;;
        --no-build)
            SKIP_BUILD=true; shift ;;
        --delete)
            DELETE_CONTAINERS=true; shift ;;
        --yes|-y)
            AUTO_YES=true; shift ;;
        --list|-l)
            SHOW_LIST=true; shift ;;
        --all|-a)
            RUN_ALL=true; shift ;;
        --help|-h)
            head -30 "$0" | grep '^#' | sed 's/^# \?//'
            exit 0 ;;
        --*)
            echo "Unknown option: $1" >&2; exit 1 ;;
        -*)
            echo "Unknown option: $1" >&2; exit 1 ;;
        *)
            TESTS+=("$1"); shift ;;
    esac
done

# ── List mode ─────────────────────────────────────────────────────────

if $SHOW_LIST; then
    echo "Available tests in terminal-bench-2-1/"
    echo "========================================="
    for d in "$BENCH_DIR"/*/; do
        name="$(basename "$d")"
        # Show first line of README as description
        readme="$d/README.md"
        if [[ -f "$readme" ]]; then
            desc="$(head -1 "$readme" | sed 's/^# //')"
            printf "  %-40s %s\n" "$name" "$desc"
        else
            echo "  $name"
        fi
    done
    exit 0
fi

# ── All mode ──────────────────────────────────────────────────────────

if $RUN_ALL; then
    if [[ ${#TESTS[@]} -gt 0 ]]; then
        echo "Error: Can't use --all with explicit test names." >&2
        exit 1
    fi
fi

# ── Validate ──────────────────────────────────────────────────────────

if ! $RUN_ALL && [[ ${#TESTS[@]} -eq 0 ]]; then
    echo "Error: No test names specified." >&2
    echo "Usage: $0 [options] <test-name> [test-name2 ...]" >&2
    echo "Try '$0 --list' to see available tests." >&2
    exit 1
fi

# Validate each test exists (skip with --all)
if ! $RUN_ALL; then
    for t in "${TESTS[@]}"; do
        if [[ ! -d "$BENCH_DIR/$t" ]]; then
            echo "Error: Test '$t' not found in $BENCH_DIR/" >&2
            echo "Try '$0 --list' to see available tests." >&2
            exit 1
        fi
    done
fi

# ── Build ─────────────────────────────────────────────────────────────

BINARY="$PROJECT_DIR/tachi-linux-amd64"

if $SKIP_BUILD; then
    if [[ ! -f "$BINARY" ]]; then
        echo "Error: --no-build specified but binary not found at $BINARY" >&2
        echo "Run 'make build-linux' first or remove --no-build." >&2
        exit 1
    fi
    echo "[bench] Using existing binary: $BINARY"
else
    echo "[bench] Building Linux binary..."
    cd "$PROJECT_DIR"
    make build-linux
    echo "[bench] Build complete: $BINARY"
fi

# ── Validate binary ───────────────────────────────────────────────────

if [[ ! -f "$BINARY" ]]; then
    echo "Error: Binary not found at $BINARY" >&2
    exit 1
fi

# ── Export env vars for the adapter ───────────────────────────────────

export TACHI_BINARY_PATH="$BINARY"
export TACHI_MAX_ITERATIONS="$MAX_ITERATIONS"

# Model → provider API key mapping
if [[ "$MODEL" == deepseek* ]] || [[ "$MODEL" == deepseek/* ]]; then
    if [[ -z "${DEEPSEEK_API_KEY:-}" ]]; then
        echo "Error: DEEPSEEK_API_KEY not set (required for model '$MODEL')" >&2
        exit 1
    fi
elif [[ "$MODEL" == claude* ]] || [[ "$MODEL" == anthropic/* ]]; then
    if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
        echo "Error: ANTHROPIC_API_KEY not set (required for model '$MODEL')" >&2
        exit 1
    fi
fi

# ── Build harbor command ──────────────────────────────────────────────

# Base flags — always applied
BASE_FLAGS=(
    --agent-import-path "harbor_adapter.tachi_agent:TachiAgent"
    --model "$MODEL"
    -n "$N_CONCURRENT"
    --ak "timeout=$TIMEOUT"
    --ek "keep_containers=True"
)

# Conditional flags
if ! $DELETE_CONTAINERS; then
    BASE_FLAGS+=(--no-delete)
fi

if $AUTO_YES; then
    BASE_FLAGS+=(--yes)
fi

# Path flags — one per test, or just the base dir for --all
PATH_FLAGS=()
if $RUN_ALL; then
    PATH_FLAGS+=(--path "$BENCH_DIR")
else
    for t in "${TESTS[@]}"; do
        PATH_FLAGS+=(--path "terminal-bench-2-1/$t")
    done
fi

# ── Print summary ─────────────────────────────────────────────────────

echo ""
echo "╔══════════════════════════════════════════════════════════════╗"
echo "║  Terminal-Bench 2.1 — Tachi                                 ║"
echo "╠══════════════════════════════════════════════════════════════╣"
printf "║  Model:        %-45s ║\n" "$MODEL"
printf "║  Concurrent:   %-45s ║\n" "$N_CONCURRENT"
printf "║  Timeout:      %-45s ║\n" "$TIMEOUT"
printf "║  Max iters:    %-45s ║\n" "$MAX_ITERATIONS"
printf "║  Delete after: %-45s ║\n" "$($DELETE_CONTAINERS && echo 'yes' || echo 'no')"
if $RUN_ALL; then
    printf "║  Tests:        %-45s ║\n" "ALL (terminal-bench-2-1/)"
else
    printf "║  Tests:        %-45s ║\n" "${#TESTS[@]} task(s)"
    for t in "${TESTS[@]}"; do
        printf "║    • %-51s ║\n" "$t"
    done
fi
echo "╚══════════════════════════════════════════════════════════════╝"
echo ""

# ── Run ──────────────────────────────────────────────────────────────

echo "[bench] Running: harbor run ${BASE_FLAGS[*]} ${PATH_FLAGS[*]}"
echo ""

cd "$PROJECT_DIR"
exec harbor run "${BASE_FLAGS[@]}" "${PATH_FLAGS[@]}"
