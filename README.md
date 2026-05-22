# Tachi — 我的 AI Agent

纯个人使用

---

## ✨ 功能一览

| 功能 | 说明 |
|------|------|
| **交互式 TUI** | 终端界面，支持流式输出、Thinking 展开、复制模式 |
| **工具生态系统** | Bash、文件编辑、代码搜索、网页搜索/抓取等内置工具 |
| **Skill 系统** | 可复用的指令模板，按需激活，支持 LLM 自主路由和 `/skill` 命令 |
| **@ 文件引用** | 输入 `@` 触发模糊搜索，快速引用项目文件 |
| **MCP 协议** | 接入 MCP 服务器（stdio 或 HTTP），扩展工具能力 |
| **子代理（SubAgent）** | LLM 自动委派子任务给子代理并发执行，支持 Git Worktree 隔离 |
| **IM Channel** | 接入微信（iLink Bot），通过聊天窗口与 AI 交互 |
| **ACP 协议** | 作为 ACP Agent 运行，与支持 ACP 的编辑器（如 VS Code 扩展）协作 |
| **会话管理** | 自动保存和恢复对话，支持历史浏览和 HTML 报告导出 |
| **斜杠命令** | `/commit`、`/compact`、`/init`、`/model`、`/mcp`、`/sessions` 等 |
| **单次运行** | `tachi run --prompt "..."` 非交互模式，支持 JSON 输出和超时控制 |
| **记忆持久化** | 可选接入 mem9 实现跨会话记忆 |
| **定时任务** | Channel 模式下支持 Cron 定时执行 |

---

## 🚀 安装

### 方式一：下载二进制（推荐）

从 [GitHub Releases](https://github.com/monsterxx03/tachi/releases) 页面下载对应平台的二进制文件，放到 `PATH` 中即可：

```bash
# 示例：macOS / Linux
curl -fsSL -o /usr/local/bin/tachi https://github.com/monsterxx03/tachi/releases/latest/download/tachi-$(uname -s)-$(uname -m)
chmod +x /usr/local/bin/tachi
tachi --help
```

### 方式二：Homebrew（macOS / Linux）

```bash
# 添加 tap
brew tap monsterxx03/tap

# 安装
brew install tachi

# 升级
brew upgrade tachi
```
---

## ⚡ 快速开始

### 1. 配置 API Key

Tachi 需要至少一个 LLM Provider。支持的 Provider 类型：

- **Anthropic**（Claude 系列）：设置环境变量 `ANTHROPIC_API_KEY`
- **OpenAI**（GPT 系列）：设置环境变量 `OPENAI_API_KEY`

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
```

### 2. 初始化配置

```bash
tachi init
```

会在 `~/.tachi/config.yaml` 生成示例配置，编辑它填入你的 API Key 和 Provider 设置。

### 3. 启动 TUI

```bash
tachi
```

直接进入交互式终端界面，输入问题即可开始对话。

---

## ⚙️ 配置说明

配置文件位于 `~/.tachi/config.yaml`，核心结构如下：

```yaml
# 默认使用的 provider 名称（必须在 providers 列表中）
provider: my-claude

# LLM 回复语言
language: 中文

# 最大输出 Token 数
max_tokens: 128000

# 最大 Agent 循环次数（0 = 无限制）
max_iterations: 50

# Provider 定义
providers:
  - name: my-claude
    type: anthropic               # anthropic | openai
    model: claude-sonnet-4-20250514
    base_url: https://api.anthropic.com/v1
    api_key: "sk-ant-..."         # 也支持环境变量

  - name: my-deepseek
    type: anthropic
    model: deepseek-v4-pro
    base_url: https://api.deepseek.com/anthropic
    api_key: "sk-..."

# 网页搜索（可选）
web_search:
  type: brave                     # brave | serper | serpapi
  key: "<your-api-key>"
  proxy: http://127.0.0.1:7890    # 可选代理

# 网页抓取（可选）
web_fetch:
  proxy: http://127.0.0.1:7890    # 可选代理

# 子代理配置（可选）
subagent:
  max_iterations: 50
  max_concurrency: 4
  max_output_chars: 16384
  worktree: true                  # 使用 git worktree 隔离

# MCP 服务器（可选）
mcp_servers:
  - name: my-tools
    type: stdio                   # stdio | http
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "."]

# 记忆持久化（可选）
memory:
  type: mem9                      # 使用 mem9 记忆服务
  mem9:
    api_key: "<your-mem9-api-key>"

# IM Channel（可选）
channel:
  weixin:
    enabled: true
    greeting: "👋 你好！Tachi 已启动"
```

### 配置详解

#### Provider

支持多个 Provider，通过 `provider` 字段指定默认使用哪一个。每个 Provider 可以配置：

| 字段 | 说明 |
|------|------|
| `name` | Provider 名称，用于引用 |
| `type` | `anthropic` 或 `openai` |
| `model` | 模型名称 |
| `base_url` | API 地址（可选，使用默认地址则留空） |
| `api_key` | API Key（可选，优先读取环境变量） |
| `context_window` | 上下文窗口大小覆盖（可选） |
| `input_price` / `output_price` | 价格覆盖（可选，CNY/百万 Token） |

API Key 的读取优先级：环境变量 > 配置文件。环境变量名称：

| Provider 类型 | 环境变量 |
|---------------|----------|
| `anthropic` | `ANTHROPIC_API_KEY` |
| `openai` | `OPENAI_API_KEY` |

#### MCP 服务器

Tachi 支持 [Model Context Protocol (MCP)](https://modelcontextprotocol.io) 协议，可以接入各种 MCP 服务器扩展工具能力。支持 stdio 和 HTTP 两种传输模式，HTTP 模式支持 OAuth2 认证。

#### MCP Profile

可以为不同的工作环境配置不同的 MCP 服务器集合：

```yaml
mcp_profiles:
  prod:
    - name: db-tools
      type: http
      url: https://prod.example.com/mcp
  staging:
    - name: db-tools
      type: http
      url: https://staging.example.com/mcp

active_mcp_profile: prod
```

#### 子代理（SubAgent）

子代理是 LLM 自动创建的子任务执行器，可以并发执行多个独立子任务。支持通过 `worktree: true` 启用 Git Worktree 隔离，每个子代理在独立的 Git Worktree 中工作，互不干扰。

#### 记忆（Memory）

可选接入 [mem9](https://mem9.ai) 实现跨会话记忆持久化，让 AI 在不同对话之间记住关键信息。

#### Channel

目前支持微信（基于 iLink Bot 协议），配置后可以通过微信与 Tachi 对话。

---

## 🎮 使用指南

### TUI 基础操作

| 操作 | 效果 |
|------|------|
| **输入问题后回车** | 发送消息给 AI |
| **`Ctrl+O`** | 展开/收起 Thinking 视图 |
| **`Ctrl+S`** | 进入复制模式（选择文本） |
| **`Ctrl+C`** | 中断 AI 回复 |
| **`Ctrl+D`** | 退出 |
| **`Tab`** | 切换焦点（输入框 / 消息列表） |
| **`@`** | 触发文件引用模糊搜索 |

### 斜杠命令

在输入框中以 `/` 开头即可使用：

| 命令 | 说明 |
|------|------|
| `/new` | 新建对话（清空当前上下文） |
| `/model` | 切换 Provider/Model |
| `/commit` | 让 AI 分析代码变更并自动提交（含 Co-authored-by） |
| `/compact` | 压缩对话历史为摘要，开启新会话 |
| `/init` | 让 AI 分析项目生成 `.tachi.md` 项目上下文文件 |
| `/mcp list` | 列出所有 MCP 服务器状态 |
| `/mcp toggle <name>` | 启用/禁用某个 MCP 服务器 |
| `/mcp reconnect <name>` | 重新连接 MCP 服务器 |
| `/mcp auth <name>` | 触发 MCP OAuth 认证流程 |
| `/sessions` | 浏览和恢复历史会话 |
| `/skill` | 列出可用 Skill |
| `/skill <name> [args]` | 激活指定 Skill |
| `/skill reload` | 重新扫描 Skill 目录 |
| `/quit` | 退出 |

### 文件引用（@）

在输入消息时输入 `@` 会触发文件模糊搜索，选择后 AI 可以看到文件内容。支持：

- 文本文件自动内联展示
- 二进制文件（PDF、Excel 等）标注路径，AI 可通过工具读取
- 目录引用展示目录结构
- 缓存加速（30 秒刷新）

### Skill 系统

Skill 是可复用的指令模板——每个 Skill 是一个包含 `SKILL.md` 的目录，描述了特定任务的执行流程和工作规范。Agent 按需加载 Skill，将其指令注入当前对话。

**存放位置：**

- 项目级：`<项目根目录>/.tachi/skills/<skill-name>/SKILL.md`
- 全局级：`~/.tachi/skills/<skill-name>/SKILL.md`

同名时项目级优先于全局级。创建 `SKILL.md` 后即可被自动发现。

**激活方式：**

- **用户显式**：输入 `/skill <name>` 或 `/skill-name` 直接激活
- **LLM 自主路由**：LLM 在上下文中看到 Skill 列表后，会自行决定何时调用 `Skill` 工具加载指令

**SKILL.md 格式示例：**

```markdown
---
name: code-review
description: Review code changes for bugs, security issues, and code style
tags: [review, security]
---

# Code Review Skill

When the user requests a code review:

1. Identify changed files with `git diff --name-only`
2. For each file, read the full content
3. Check for: hardcoded secrets, injection risks, nil checks, error handling
4. Output: 🔴 Critical / 🟡 Warning / 🟢 Suggestion
```

### 会话与报告

查看会话列表和生成 HTML 报告：

```bash
tachi transcript list
tachi transcript show --latest
tachi transcript show --session <id>
```

---

## 🔧 运行模式

### TUI 模式（默认）

交互式终端界面，支持流式输出、斜杠命令、@文件引用、会话管理等：

```bash
tachi
tachi --resume        # 恢复上次会话
```

### Channel 模式

通过 IM 平台（目前支持微信 iLink Bot）与 Tachi 交互：

```bash
tachi channel
```

### ACP 模式

作为 ACP (Agent Client Protocol) Agent 运行，与支持 ACP 的编辑器或 IDE 协作：

```bash
tachi acp
```

**agentic.nvim 配置**

在 `~/.config/nvim/lua/plugins/agentic.lua` 中注册 Tachi 作为 ACP Agent：

```lua
return {
  "carlos-algms/agentic.nvim",
  opts = {
    provider = "tachi-acp",
    acp_providers = {
      ["tachi-acp"] = {
        name = "Tachi",
        command = "tachi",
        args = { "acp" },
      },
    },
  },
}
```

**Zed 配置**

在 `~/.config/zed/settings.json` 中注册：

```json
{
  "agent_servers": {
    "Tachi": {
      "type": "custom",
      "command": "tachi",
      "args": ["acp"]
    }
  }
}
```

### CLI 模式

非交互式单次运行，适合脚本调用和基准测试：

```bash
# 通过 --prompt 传入指令
tachi run --prompt "找到项目中所有 TODO 并列出"

# 从标准输入管道传入指令（无 --prompt 时自动读取 stdin）
echo "Rewrite the README to be more concise" | tachi run
cat task.txt | tachi run
git diff --cached | tachi run --prompt "Review this diff and suggest improvements"

# JSON 结构化输出（适用于脚本调用）
tachi run --prompt "Count lines of code" --json

# 带超时控制
tachi run --prompt "..." --timeout 5m
```

---

## 📁 数据目录

Tachi 的状态数据存储在 `~/.tachi/` 目录下：

| 路径 | 说明 |
|------|------|
| `config.yaml` | 配置文件 |
| `session/` | 对话会话存储 |
| `session/<id>/meta.json` | 会话元数据 |
| `session/<id>/messages.jsonl` | 会话消息历史 |
| `input_history` | 输入历史 |
| `mcp_tokens/` | MCP OAuth Token 存储 |
| `logs/` | 调试日志 |
| `skills/` | 自定义技能文件 |

---

## 🔗 相关资源

- [GitHub 仓库](https://github.com/monsterxx03/tachi)
- [Terminal-Bench 评测](https://www.tbench.ai) — Tachi 在真实终端任务上的基准测试
- [Harbor 评测框架](https://github.com/harbor-framework/harbor) — 运行基准测试的工具
- [MCP 协议](https://modelcontextprotocol.io) — Model Context Protocol
- [ACP 协议](https://github.com/coder/acp) — Agent Client Protocol

---

## 📝 许可

[MIT License](LICENSE)
