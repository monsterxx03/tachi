# Tachi Chrome Extension 🚀

AI 助手 — 在浏览器中选中文本即可提问、搜索、记忆。

## 功能

右键菜单（选中文本后）：

| 菜单项 | 动作 | 说明 |
|--------|------|------|
| 🤖 问 Tachi | `ask_tachi` | 完整 Agent 对话回合 |
| 📖 解释这个概念 | `explain` | 解释选中概念 |
| 🔍 搜索这个 | `search` | WebSearch 搜索 |
| 🧠 记住这个 | `remember` | 存入 Tachi 记忆 |
| 🔎 我读过这个吗？ | `recall` | 搜索已有记忆 |

## 前置条件

1. Tachi 已安装（Go 1.26+）
2. Chrome 浏览器

## 安装

### 1. 构建扩展

```bash
cd chrome-extension
make build
```

### 2. 加载扩展

1. 打开 `chrome://extensions`
2. 开启 **开发者模式**（右上角）
3. 点击 **加载已解压的扩展**
4. 选择 `chrome-extension/dist/` 目录

### 3. 安装 Native Messaging Host

Chrome 会给扩展分配一个 ID（在 `chrome://extensions` 中可以看到），用它来安装 Native Messaging host：

```bash
# 查看扩展 ID 后
tachi chrome install --extension-id=abcdefghijklmnopabcdefghijklmnop
```

### 4. 启动 Tachi 通道模式

```bash
tachi channel
```

或在配置文件中启用：

```yaml
# ~/.tachi/config.yaml
channel:
  channels:
    chrome:
      enabled: true
```

## 开发

```bash
make build    # 构建到 dist/
make install  # 构建 + 打开 chrome://extensions
make dev      # 文件变化时自动构建
make pack     # 打包为 zip
make clean    # 清理
```

## 项目结构

```
chrome-extension/
├── manifest.json       # 扩展配置
├── background.js       # Service Worker
├── content.js          # Content Script（浮动提示框）
├── toast.css           # 浮动提示框样式
├── popup.html          # 弹窗面板
├── popup.js            # 弹窗逻辑
├── sidepanel.html      # 侧边栏
├── icons/              # 图标
├── Makefile            # 构建脚本
└── README.md
```

## 状态

- ✅ MVP：右键菜单 + 浮动提示框
- 🚧 Side Panel：基础界面已完成
- 📋 后续：Page Memory、Read Later
