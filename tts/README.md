# tts-server — IndexTTS HTTP 服务（kokoro 兼容）

为 tachi 的 device channel 提供 TTS 语音合成。接口兼容
`channel/device/device.go` 中使用的 Kokoro 封装协议，可无缝替换
`tts_url` 指向的 Kokoro 服务。

## 接口

```
POST /tts
    Body: {"text": "...", "voice": "tachikoma", "speed": 1.05}
    Resp: audio/wav 原始字节（200 OK）
GET  /health -> {"status":"ok","model_loaded":true,"voices":[...]}
```

与 device.go 的兼容约定：

- `text` 中 `\n\r\t` 会被替换为空格，超过 500 字符截断
- **固定使用塔奇克马音色**（`prompts/tachikoma/ref_tachikoma_12s.wav`），请求中的 `voice` 字段被忽略
- **语速固定放慢 15%**（`duration_factor = 1.15`），请求中的 `speed` 字段被忽略

## 启动

```bash
cd ~/repos/tachi/tts
uv run python server.py
# 默认监听 0.0.0.0:8888，启动时预加载模型（约 1-2 分钟）
```

## 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `INDEX_TTS_DIR` | `~/repos/tts/index-tts` | IndexTTS 项目根目录 |
| `INDEX_TTS_MODEL_DIR` | `$INDEX_TTS_DIR/checkpoints` | 模型权重目录（含 config.yaml） |
| `TTS_PROMPTS_DIR` | `./prompts` | 参考音频目录 |
| `TTS_HOST` / `TTS_PORT` | `0.0.0.0` / `8888` | 监听地址 |
| `TTS_MAX_TEXT` | `500` | 文本长度上限 |
| `TTS_PRELOAD` | `1` | 启动时预加载模型 |

## 音色

**固定塔奇克马音色**，参考音频位于：

```
prompts/tachikoma/ref_tachikoma_12s.wav
```

请求中的 `voice` 字段被忽略。

## 语言

`lang` 可选参数可显式指定（`ZH`/`EN`/`JA`/`ES`/`AR`/`zh/en`）；
不传时自动检测：含日文假名→JA，含汉字→ZH，否则 EN。

## device channel 接入

在 tachi 配置中把 `tts_url` 指向本服务即可（音色固定塔奇克马、语速固定放慢 15%，`tts_voice`/`tts_speed` 配置会被忽略）：

```yaml
channel:
  channels:
    device:
      enabled: true
      port: 8080
      tts_url: http://127.0.0.1:8888
```

> ⚠️ 注意：device.go 的 HTTP client 超时已放宽到 **5 分钟**，但 IndexTTS 在
> M1 Pro 上生成 3-4 秒音频约需 20-40 秒，长文本更久——短回复体验最佳。

## 依赖

```bash
cd ~/repos/tachi/tts && uv sync   # 仅 fastapi/uvicorn（轻量）
```

`indextts` 不写入依赖，运行时通过 `INDEX_TTS_DIR` 环境变量定位
IndexTTS 项目（默认 `~/repos/tts/index-tts`），其依赖（torch 等）
复用该项目的 `.venv`。
