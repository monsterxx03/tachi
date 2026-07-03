# Tachi Chrome Extension 🚀

AI 浏览器助手 — 页面总结、追问、沉浸式翻译。

## 功能

| 功能 | 触发方式 | 说明 |
|------|---------|------|
| 📝 总结页面 | 点击工具栏图标 / 自动 | 用 Tachi 提取并总结当前页面内容 |
| 💬 追问 | 侧边栏输入框 | 基于页面内容的连续对话 |
| 🌐 翻译页面 | 侧边栏 🌐 按钮 | 沉浸式逐段翻译，译文插在原文下方 |
| 🔄 切换翻译模式 | 页面浮动按钮 / 侧边栏 | 原文 / 双语 / 译文 三种模式 |

### 翻译模式

点击侧边栏的 🌐 按钮后，页面段落会被逐段翻译成中文（外语页面）或英文（中日韩页面）：

- **原文模式** 📖 — 只显示原文
- **双语模式** 🌐 — 原文 + 译文对照（默认）
- **译文模式** 🌍 — 只显示译文

页面右下角的浮动按钮可快速切换模式，侧边栏的切换按钮同步更新。

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

### 3. 配置 host_permissions

扩展通过 WebSocket 连接本地 Tachi 服务。需要在 `manifest.json` 中声明权限：

```json
{
  "host_permissions": [
    "http://127.0.0.1:18520/*",
    "ws://127.0.0.1:18520/*"
  ]
}
```

默认端口 `18520`，可以在 `~/.tachi/config.yaml` 中修改：

```yaml
channel:
  chrome:
    enabled: true
    port: 18520
```

> **注意**：不再需要安装 Native Messaging host manifest。只需确保 Tachi 在运行即可。

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
make pack     # 打包为 zip
make clean    # 清理
```

## 项目结构

```
chrome-extension/
├── manifest.json       # 扩展配置 (MV3)
├── background.js       # Service Worker — WebSocket + 消息路由
├── content.js          # Content Script — 内容提取 + 沉浸式翻译
├── sidepanel.html      # 侧边栏 UI
├── sidepanel.js        # 侧边栏逻辑
├── icons/              # 图标
├── scripts/            # 工具脚本
├── Makefile            # 构建脚本 (esbuild)
└── README.md
```

### 架构

```
用户点击工具栏图标
    ↓
background.js 打开侧边栏
    ↓
sidepanel.js 请求页面总结
    ↓
background.js → content.js (提取内容) → Tachi WebSocket (总结)
    ↓
结果返回侧边栏渲染

翻译流程:
侧边栏 🌐 按钮
    ↓
background.js → content.js (提取段落)
    ↓
background.js → Tachi WebSocket (翻译)
    ↓
background.js → content.js (注入译文)
    ↓
浮动按钮 / 侧边栏 切换模式
```

## 通信协议

与 Tachi 的 WebSocket 消息格式：

**请求：**
```json
{
  "id": "tachi_1_1712345678000",
  "action": "summarize|followup|translate",
  "threadID": "tab_42",
  "selection": { "text": "", "url": "", "title": "" },
  "content": "prompt or question"
}
```

**响应：**
```json
{
  "id": "tachi_1_1712345678000",
  "type": "result|error",
  "threadID": "tab_42",
  "content": "response text"
}
```

## 状态

- ✅ 页面总结（Readability 提取 + Tachi 总结）
- ✅ 追问（基于页面内容的连续对话）
- ✅ 沉浸式翻译（逐段翻译 + 三种模式切换）
- 📋 后续：页面记忆、稍后阅读
