# Tachi — Harbor Adapter for Terminal-Bench

将 Tachi 接入 [Terminal-Bench 2.0](https://www.tbench.ai) 评测框架的 Harbor adapter。

## 前置条件

| 依赖 | 说明 |
|---|---|
| [Harbor CLI](https://github.com/harbor-framework/harbor) | `pip install harbor` 或 `uv tool install harbor` |
| [Docker](https://docs.docker.com/get-docker/) | Harbor 默认用 Docker 作为运行环境 |
| Go 1.22+ | 编译 Tachi Linux 二进制 |
| API Key | Anthropic 或 OpenAI |

## 快速开始

### 1. 编译 Tachi Linux 二进制

```bash
cd /path/to/tachi
make build-linux
```

这会在当前目录生成 `tachi-linux-amd64`。

### 2. 运行 Terminal-Bench

```bash
export ANTHROPIC_API_KEY="sk-ant-..."

harbor run \
    --dataset terminal-bench@2.0 \
    --agent-import-path ./harbor_adapter/tachi_agent.py:TachiAgent \
    --model anthropic/claude-sonnet-4-20250514 \
    --n-concurrent 4
```

### 3. 只看一个任务快速验证

```bash
harbor run \
    --task-id hello-world/hello-world \
    --agent-import-path ./harbor_adapter/tachi_agent.py:TachiAgent \
    --model anthropic/claude-sonnet-4-20250514
```

## 配置选项

### 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `TACHI_BINARY_PATH` | `./tachi-linux-amd64` | 自定义二进制路径 |
| `TACHI_BINARY_URL` | — | 从 URL 下载二进制（优先级高于 `PATH`） |
| `TACHI_MAX_ITERATIONS` | `50` | 最大 agent 循环次数 |
| `TACHI_TIMEOUT` | `10m` | 单次执行超时 |

### Agent 参数（`--ak`）

```bash
harbor run ... \
    --ak max_iterations=100 \
    --ak timeout=15m
```

### 传递 API Key

```bash
harbor run ... \
    --ae ANTHROPIC_API_KEY="sk-ant-..." \
    --ae OPENAI_API_KEY="sk-..."
```

## 输出说明

Tachi 的 JSON 输出格式：

```json
{
  "exit_reason": "stop",
  "iterations_used": 3,
  "usage": {
    "input_tokens": 2393,
    "output_tokens": 128,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 3456
  },
  "response": "完成！",
  "error": null
}
```

### Exit Code 映射

| Exit Reason | Exit Code | 含义 |
|---|---|---|
| `stop` | 0 | 正常完成 |
| `error`, `cancelled` | 1 | 执行出错 |
| `budget_exhausted`, `length_exhausted` | 2 | 配额耗尽 |

## 架构

```
Host:
  harbor run --agent-import-path tachi_agent.py:TachiAgent
        │
        ▼
  ┌─── Docker Container ──────────────────────┐
  │  /usr/local/bin/tachi        ← binary     │
  │  ~/.tachi/config.yaml        ← auto-gen   │
  │                                            │
  │  tachi run --json --prompt "<task>"        │
  │       │                                    │
  │       ▼ stdout                             │
  │  {"exit_reason":"stop", ...}               │
  └────────────────────────────────────────────┘
```

## 项目结构

```
tachi/
├── harbor_adapter/
│   ├── tachi_agent.py    ← Harbor adapter (Python)
│   └── README.md         ← 本文档
├── main.go               ← Tachi 主入口（含 --json 支持）
├── agent/                ← agent 核心逻辑
└── ...
```
