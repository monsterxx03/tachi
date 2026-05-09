# Weixin iLink 协议说明

> 本文档基于 `openclaw-weixin` 源码分析，描述微信 iLink Bot 协议的完整流程，涵盖认证、消息轮询（长轮询）、消息发送、媒体分发（CDN）、会话管理及错误处理。编程语言无关，可用于后续重写/移植开发。

---

## 目录

1. [协议概览](#1-协议概览)
2. [API 基础](#2-api-基础)
3. [认证登录（QR 码）](#3-认证登录qr-码)
4. [消息接收（长轮询）](#4-消息接收长轮询)
5. [消息发送（下行）](#5-消息发送下行)
6. [媒体文件上传（CDN）](#6-媒体文件上传cdn)
7. [媒体文件下载（CDN）](#7-媒体文件下载cdn)
8. [消息类型与结构](#8-消息类型与结构)
9. [contextToken 机制](#9-contexttoken-机制)
10. [状态管理与会话](#10-状态管理与会话)
11. [错误处理与重试](#11-错误处理与重试)
12. [发送限流与分片](#12-发送限流与分片)
13. [附录：关键 HTTP 头与元数据](#13-附录关键-http-头与元数据)

---

## 1. 协议概览

微信 iLink Bot 协议是一套基于 HTTP/HTTPS 的长轮询消息协议，用于让第三方 Bot（机器人）与微信用户进行双向通信。

### 核心特征

- **传输层**：纯 HTTP/HTTPS，JSON 序列化
- **消息模式**：长轮询 (`long-polling`)，Bot 保持 HTTP 连接等待新消息
- **认证方式**：`Bearer Token`（扫码登录后获取的 bot_token）
- **消息加密**：媒体文件（图片、视频、文件、语音）使用 AES-128-ECB 加密传输
- **状态同步**：`get_updates_buf` 机制维护消息同步状态

### 整体架构

```
┌──────────────┐      HTTP/JSON       ┌──────────────┐
│  微信客户端   │ ◄──────────────────► │  iLink 服务端  │
│  (WeChat)    │                      │  ilinkai.*   │
└──────────────┘                      └──────┬───────┘
                                             │ HTTP/JSON
                                             ▼
                                    ┌──────────────────┐
                                    │   Bot 客户端      │
                                    │  (你的实现)       │
                                    └──────────────────┘
```

Bot 端主动发起的操作：
1. 发起 QR 码登录获取 `bot_token`
2. 长轮询 `getUpdates` 接收消息
3. 调用 `sendMessage` 发送消息
4. 调用 `getUploadUrl` 获取 CDN 上传凭证
5. 调用 `getConfig` 获取 typing_ticket
6. 调用 `sendTyping` 发送输入状态

---

## 2. API 基础

### 2.1 基础 URL

| 环境 | URL |
|------|-----|
| API 基础 | `https://ilinkai.weixin.qq.com` |
| CDN（上传/下载） | `https://novac2c.cdn.weixin.qq.com/c2c` |

> 注意：登录过程中可能发生 IDC 重定向（`scaned_but_redirect`），此时需切换 API 基础 URL。

### 2.2 固定的 HTTP 请求头

每个 POST/PASS 请求都携带以下公共头：

```
Content-Type: application/json
AuthorizationType: ilink_bot_token
Authorization: Bearer <bot_token>
X-WECHAT-UIN: <base64(random_uint32_decimal_string)>
Content-Length: <body_byte_length>
iLink-App-Id: bot
iLink-App-ClientVersion: <uint32: 0x00MMNNPP>
Content-Type: application/json
```

- **X-WECHAT-UIN**：随机生成，每次请求不同。生成方式：生成随机 4 字节 uint32 → 转十进制字符串 → base64 编码
- **iLink-App-Id**：固定为 `"bot"`
- **iLink-App-ClientVersion**：编码格式 `0x00MMNNPP`，M=主版本号, N=次版本号, P=补丁号。例如 `"2.1.7"` → `0x00020107` = `131335`
- **SKRouteTag**：可选，从配置中读取，用于路由

### 2.3 BaseInfo

每个 API 请求的 JSON body 中携带 `base_info` 字段：

```json
{
  "base_info": {
    "channel_version": "2.1.7"
  }
}
```

### 2.4 API 端点汇总

| 端点 | 方法 | 用途 | 默认超时 |
|------|------|------|----------|
| `ilink/bot/get_bot_qrcode` | GET | 获取登录 QR 码 | - |
| `ilink/bot/get_qrcode_status` | GET | 长轮询 QR 码扫描状态 | 35s |
| `ilink/bot/getupdates` | POST | 长轮询接收消息 | 35s |
| `ilink/bot/sendmessage` | POST | 发送消息 | 15s |
| `ilink/bot/getuploadurl` | POST | 获取媒体上传凭证 | 15s |
| `ilink/bot/getconfig` | POST | 获取 bot 配置（含 typing_ticket） | 10s |
| `ilink/bot/sendtyping` | POST | 发送输入状态 | 10s |


---

## 3. 认证登录（QR 码）

认证流程分为两步：获取 QR 码 → 长轮询扫码结果。

### 3.1 流程总览

```
Bot                           iLink Server                 微信客户端
│                                │                            │
│  GET get_bot_qrcode            │                            │
│  ?bot_type=3                   │                            │
│ ─────────────────────────────► │                            │
│ ◄───────────────────────────── │                            │
│  { qrcode, qrcode_img_content }│                            │
│                                │                            │
│  展示 QR 码二维码               │                            │
│ ──────────────────────────────────────────────────────────► │
│                                │                            │
│  GET get_qrcode_status         │                            │
│  ?qrcode=xxx                   │                            │
│  (长轮询, 最长 35s)             │                            │
│ ─────────────────────────────► │                            │
│  ◄── { status: "wait" } ────  │    ← 用户扫码              │
│  ◄── { status:"scaned" } ──── │ ◄───────────────────────── │
│                                │    ← 用户在手机上确认      │
│  ◄── { status:"confirmed",    │ ◄───────────────────────── │
│         bot_token,             │                            │
│         ilink_bot_id,          │                            │
│         ilink_user_id } ────── │                            │
│                                │                            │
│  存储 bot_token                 │                            │
│  存储 ilink_bot_id → accountId │                            │
│  存储 ilink_user_id → userId   │                            │
└────────────────────────────────┘                            └
```

### 3.2 获取 QR 码

**请求**：
```
GET https://ilinkai.weixin.qq.com/ilink/bot/get_bot_qrcode?bot_type=3
```

**响应**：
```json
{
  "qrcode": "<string: QR 加密标识>",
  "qrcode_img_content": "<string: 二维码内容 URL，可直接展示>"
}
```

- `qrcode`：后续轮询时需要的标识符
- `qrcode_img_content`：二维码图片 URL，可显示为二维码或文本链接

### 3.3 长轮询 QR 码状态

**请求**：
```
GET https://ilinkai.weixin.qq.com/ilink/bot/get_qrcode_status?qrcode=<encoded_qrcode>
```

**响应状态机**（`status` 字段）：

| 状态 | 含义 | 后续行为 |
|------|------|----------|
| `wait` | 等待扫码 | 继续轮询 |
| `scaned` | 已扫码，等待手机确认 | 继续轮询 |
| `confirmed` | 已在手机确认登录完成 | 流程结束，取 bot_token |
| `expired` | 二维码过期 | 重新获取 QR 码（最多 3 次） |
| `scaned_but_redirect` | 已扫码，但需要切换服务端 | 更新 API baseUrl 继续轮询 |

**`confirmed` 状态响应包含**：
```json
{
  "status": "confirmed",
  "bot_token": "<string: 后续 API 认证用的 Bearer token>",
  "ilink_bot_id": "<string: 格式 xxx@im.bot, 作为 accountId>",
  "ilink_user_id": "<string: 扫码用户的微信 ID>",
  "baseurl": "<string: 可选的 API base URL 覆盖>"
}
```

### 3.4 关键参数

| 参数 | 说明 |
|------|------|
| `bot_type` | Bot 类型，固定为 `"3"` |
| 超时 | 整体登录超时 480s (8 分钟) |
| QR 刷新 | 最多自动刷新 3 次过期二维码 |
| 长轮询超时 | 每次 GET 请求最长 35s，超时返回 `wait` |

### 3.5 IDC 重定向

当收到 `scaned_but_redirect` 状态且附带 `redirect_host` 字段时，需要将后续轮询的 base URL 切换到 `https://<redirect_host>` 地址。

---

## 4. 消息接收（长轮询）

### 4.1 请求格式

```
POST https://ilinkai.weixin.qq.com/ilink/bot/getupdates
Authorization: Bearer <bot_token>
Content-Type: application/json

{
  "get_updates_buf": "<string: 上一个响应返回的 buf，首次请求传空字符串>",
  "base_info": { "channel_version": "2.1.7" }
}
```

### 4.2 响应格式

```json
{
  "ret": 0,
  "msgs": [
    { /* WeixinMessage */ }
  ],
  "get_updates_buf": "<string: 新的同步状态，需缓存并在下次请求时发送>",
  "longpolling_timeout_ms": 35000
}
```

### 4.3 核心机制

#### get_updates_buf（同步缓存）

- **作用**：类似消息同步的 checkpoint / cursor，保证消息不丢失不重复
- **首次请求**：传空字符串 `""`
- **后续请求**：传上次响应中的 `get_updates_buf` 值
- **持久化**：需持久化到磁盘（`<accountId>.sync.json`），以便重启后恢复

#### 长轮询超时

- 请求发送后，服务端会保持连接直到有新消息或超时（默认约 35s）
- 超时返回空列表 `{ ret: 0, msgs: [] }`
- 响应中的 `longpolling_timeout_ms` 可指定下次请求的超时值

### 4.4 错误码处理

| errcode | 含义 | 处理方式 |
|---------|------|----------|
| -14 | 会话过期（Session Expired） | 暂停所有操作 1 小时 |
| 其他非 0 | API 错误 | 重试，最多 3 次后退避 30s |

### 4.5 轮询循环伪代码

```
buf = loadFromDisk() or ""
while not aborted:
    resp = POST getUpdates(buf, timeoutMs=nextTimeout)
    if resp.longpolling_timeout_ms > 0:
        nextTimeout = resp.longpolling_timeout_ms
    if resp.errcode == -14:
        pause_session(1 hour)
        sleep(pause_remaining)
        continue
    if resp.ret != 0:
        retry with backoff (up to 3 failures, then 30s pause)
        continue
    saveToDisk(resp.get_updates_buf)
    buf = resp.get_updates_buf
    for msg in resp.msgs:
        processMessage(msg)
```

---

## 5. 消息发送（下行）

### 5.1 发送文本消息

```
POST https://ilinkai.weixin.qq.com/ilink/bot/sendmessage
Authorization: Bearer <bot_token>

{
  "msg": {
    "from_user_id": "",
    "to_user_id": "<目标用户微信 ID，格式 xxx@im.wechat>",
    "client_id": "<客户端生成的唯一 ID>",
    "message_type": 2,
    "message_state": 2,
    "item_list": [
      {
        "type": 1,
        "text_item": { "text": "消息内容" }
      }
    ],
    "context_token": "<从入站消息中获取的 token>"
  },
  "base_info": { "channel_version": "2.1.7" }
}
```

### 5.2 消息体字段

| 字段 | 类型 | 说明 |
|------|------|------|
| `from_user_id` | string | Bot ID，发送时可传空字符串 |
| `to_user_id` | string | **必填**，目标用户 ID（格式 `xxx@im.wechat`） |
| `client_id` | string | **必填**，客户端生成的消息唯一 ID（用于去重） |
| `message_type` | int | 2 = Bot 消息 |
| `message_state` | int | 2 = 已完成 |
| `item_list` | array | 消息内容列表 |
| `context_token` | string | **重要**，从入站消息中获取的上下文 token |

### 5.3 消息类型常量

#### MessageType
| 值 | 含义 |
|----|------|
| 0 | 无 |
| 1 | 用户消息 |
| 2 | Bot 消息 |

#### MessageItemType
| 值 | 含义 |
|----|------|
| 0 | 无 |
| 1 | 文本 (TEXT) |
| 2 | 图片 (IMAGE) |
| 3 | 语音 (VOICE) |
| 4 | 文件 (FILE) |
| 5 | 视频 (VIDEO) |

#### MessageState
| 值 | 含义 |
|----|------|
| 0 | 新消息 |
| 1 | 生成中 |
| 2 | 已完成 |

### 5.4 消息发送流程

1. **单条消息含多个 Item**：发送消息时 `item_list` 可包含多个 item（如文字 + 图片）。但对于媒体消息，当前做法是每个 item 单独发送一个 sendMessage 请求（确保 item_list 只有一项）。

2. **文本+媒体一并发送**：先发送文本 item，再发送媒体 item，每个独立请求。

3. **client_id**：每次发送生成新 ID（使用时间戳 + 随机后缀），用于去重和日志追踪。

### 5.5 contextToken 的重要性

每条入站消息携带 `context_token`。在回复时必须将该 token 原样带回 `sendMessage` 请求中。`context_token` 按（accountId, userId）映射缓存到内存和磁盘。

如果不带 context_token 回复，消息仍然能够送达，但会丢失会话上下文关联。


---

## 6. 媒体文件上传（CDN）

### 6.1 完整上传流程

发送图片、视频、文件等媒体时，分四步：

```
1. 准备文件
   └── 计算明文大小 (rawsize) 和 MD5 (rawfilemd5)
   └── 生成随机 AES-128 密钥 (aeskey, 16字节)
   └── 生成随机 filekey (16字节 hex)
   └── 计算加密后大小 (filesize = ceil((rawsize+1)/16)*16)

2. getUploadUrl
   POST /ilink/bot/getuploadurl
   └── 服务端返回 upload_full_url 或 upload_param

3. CDN Upload
   POST <upload_full_url>
   └── body: AES-128-ECB 加密后的密文
   └── Content-Type: application/octet-stream
   └── 服务端返回 x-encrypted-param (下载加密参数)

4. sendMessage (类型为 IMAGE/VIDEO/FILE)
   └── 填入 downloadEncryptedQueryParam、aes_key、密文大小
```

### 6.2 getUploadUrl 请求

```
POST https://ilinkai.weixin.qq.com/ilink/bot/getuploadurl

{
  "filekey": "<16字节hex>",
  "media_type": 1,
  "to_user_id": "<目标用户ID>",
  "rawsize": 12345,
  "rawfilemd5": "<明文MD5 hex>",
  "filesize": 12352,
  "no_need_thumb": true,
  "aeskey": "<aeskey的hex字符串>",
  "base_info": { "channel_version": "2.1.7" }
}
```

**字段说明**：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `filekey` | string | 是 | 随机 16 字节 hex 作为文件标识 |
| `media_type` | int | 是 | 1=IMAGE, 2=VIDEO, 3=FILE, 4=VOICE |
| `to_user_id` | string | 是 | 接收用户 ID |
| `rawsize` | int | 是 | 原始明文文件字节数 |
| `rawfilemd5` | string | 是 | 明文文件 MD5（hex） |
| `filesize` | int | 是 | AES 加密后的文件大小（到 16 的倍数） |
| `thumb_rawsize` | int | 否 | 缩略图明文大小（图片/视频时推荐） |
| `thumb_rawfilemd5` | string | 否 | 缩略图明文 MD5 |
| `thumb_filesize` | int | 否 | 缩略图密文大小 |
| `no_need_thumb` | bool | 否 | 不需要缩略图（默认 false） |
| `aeskey` | string | 是 | AES-128 密钥的 hex 表示 |

### 6.3 getUploadUrl 响应

```json
{
  "upload_param": "<string: CDN 上传加密参数>",
  "thumb_upload_param": "<string: 缩略图上传参数>",
  "upload_full_url": "<string: 完整上传 URL>"
}
```

- `upload_full_url` 优先使用，不为空时直接 POST 到这个 URL
- 否则用 `upload_param` 拼接 URL：`{cdnBaseUrl}/upload?encrypted_query_param={upload_param}&filekey={filekey}`

### 6.4 CDN 上传

```
POST <upload_full_url>
Content-Type: application/octet-stream

<AES-128-ECB 加密后的二进制密文>
```

**响应头**：
```
x-encrypted-param: <下载加密参数，需存储用于 sendMessage>
```

**重试机制**：
- 最多重试 3 次
- 4xx 错误（客户端错误）直接抛出，不重试
- 5xx 或网络错误重试

### 6.5 AES-128-ECB 加密

```typescript
// 加密
function encryptAesEcb(plaintext: Buffer, key: Buffer): Buffer {
    // Node.js: createCipheriv("aes-128-ecb", key, null)
    // 默认使用 PKCS7 填充
}

// 计算加密后大小
function aesEcbPaddedSize(plaintextSize: number): number {
    return Math.ceil((plaintextSize + 1) / 16) * 16;
    // 注意：+1 是因为 PKCS7 至少填充 1 字节
}
```

> **重要**：加密必须在发送给 CDN 之前完成，CDN 存储的是密文。

### 6.6 发送媒体消息

以图片为例，sendMessage 的 item_list:

```json
{
  "msg": {
    "to_user_id": "<target>",
    "client_id": "<unique>",
    "message_type": 2,
    "message_state": 2,
    "item_list": [
      {
        "type": 2,
        "image_item": {
          "media": {
            "encrypt_query_param": "<从CDN返回的x-encrypted-param>",
            "aes_key": "<aeskey的base64编码>",
            "encrypt_type": 1
          },
          "mid_size": <加密后文件大小>
        }
      }
    ],
    "context_token": "<从入站消息获取>"
  }
}
```

**不同媒体类型的 MessageItem**：

**图片 (type=2)**：
```json
{
  "type": 2,
  "image_item": {
    "media": {
      "encrypt_query_param": "...",
      "aes_key": "<aeskey base64>",
      "encrypt_type": 1
    },
    "mid_size": <ciphertext_size>,
    "aeskey": "<aeskey hex: 优先于 media.aes_key>"
  }
}
```

**视频 (type=5)**：
```json
{
  "type": 5,
  "video_item": {
    "media": {
      "encrypt_query_param": "...",
      "aes_key": "<aeskey base64>",
      "encrypt_type": 1
    },
    "video_size": <ciphertext_size>
  }
}
```

**文件 (type=4)**：
```json
{
  "type": 4,
  "file_item": {
    "media": {
      "encrypt_query_param": "...",
      "aes_key": "<aeskey base64>",
      "encrypt_type": 1
    },
    "file_name": "document.pdf",
    "len": "<明文大小（字符串）>"
  }
}
```

---

## 7. 媒体文件下载（CDN）

### 7.1 下载流程

收到入站消息时，如果存在媒体附件，需下载并解密：

```
入站消息中的媒体引用:
  - encrypt_query_param (或 full_url)
  - aes_key (base64 编码)
  - encrypt_type

↓

构建 CDN 下载 URL:
  - 优先使用 full_url（响应中的字段）
  - 否则拼接: {cdnBaseUrl}/download?encrypted_query_param={param}

↓

GET <CDN URL> → 获取加密的二进制数据

↓

AES-128-ECB 解密 → 获取明文数据

↓

保存到本地（通过框架持久化接口）
```

### 7.2 aes_key 编码兼容性

`aes_key` 有两种编码方式，都需要处理：

| 格式 | 说明 | 解码方式 |
|------|------|----------|
| base64(16字节密钥) | 直接编码密钥 | base64 解码后直接是 16 字节密钥 |
| base64(hex(16字节密钥)) | 密钥先转 hex 字符串再 base64 | base64 解码 → 32 字节 ASCII hex → hex 解析 → 16 字节密钥 |

检测逻辑：
```python
decoded = base64_decode(aes_key)
if len(decoded) == 16:
    key = decoded                              # 直接使用
elif len(decoded) == 32 and is_hex(decoded):
    key = bytes.fromhex(decoded.decode('ascii')) # hex 解码
else:
    raise Error("unexpected aes_key format")
```

### 7.3 CDN URL 结构

```
下载: {cdnBaseUrl}/download?encrypted_query_param={url_encoded_param}
上传: {cdnBaseUrl}/upload?encrypted_query_param={param}&filekey={filekey}
```

CDN 基础 URL: `https://novac2c.cdn.weixin.qq.com/c2c`

### 7.4 语音特殊处理

语音消息（type=3）解密后会得到 SILK 编码的音频数据。如果需要播放，可能需要转码为 WAV 格式（可选依赖 silk-wasm）。

```json
{
  "type": 3,
  "voice_item": {
    "media": { "encrypt_query_param": "...", "aes_key": "..." },
    "encode_type": 6,
    "playtime": 3000,
    "text": "语音转文字内容（可选）"
  }
}
```

- `encode_type`: 1=pcm, 2=adpcm, 3=feature, 4=speex, 5=amr, **6=silk**, 7=mp3, 8=ogg-speex
- 如果 `voice_item.text` 有值，直接使用该文本作为消息（无需下载语音）
- 否则需下载解密后 SILK → WAV 转码


---

## 8. 消息类型与结构

### 8.1 完整 WeixinMessage 结构

```json
{
  "seq": 1,
  "message_id": 100001,
  "from_user_id": "wx_user@im.wechat",
  "to_user_id": "bot@im.bot",
  "client_id": "...",
  "create_time_ms": 1700000000000,
  "update_time_ms": 1700000000000,
  "delete_time_ms": 0,
  "session_id": "...",
  "group_id": "...",
  "message_type": 1,
  "message_state": 0,
  "item_list": [ ... ],
  "context_token": "abc123..."
}
```

### 8.2 item_list 内各类型详解

#### 文本 (type=1)

```json
{
  "type": 1,
  "create_time_ms": 1700000000000,
  "update_time_ms": 1700000000000,
  "is_completed": true,
  "msg_id": "...",
  "ref_msg": {                // 可选：引用消息
    "message_item": { /* 被引用的消息 item */ },
    "title": "摘要文字"
  },
  "text_item": {
    "text": "消息正文"
  }
}
```

**引用消息处理**：当文本消息带有 `ref_msg` 时，格式化为 `[引用: title | 被引用内容]\n当前文本`。

#### 图片 (type=2)

```json
{
  "type": 2,
  "image_item": {
    "media": {
      "encrypt_query_param": "...",
      "aes_key": "<base64>",
      "encrypt_type": 1,
      "full_url": "https://..."
    },
    "thumb_media": {
      "encrypt_query_param": "...",
      "aes_key": "<base64>",
      "encrypt_type": 1
    },
    "aeskey": "<hex: 用于解密，优先于 media.aes_key>",
    "url": "...",
    "mid_size": 123456,
    "thumb_size": 1234,
    "thumb_height": 240,
    "thumb_width": 320,
    "hd_size": 999999
  }
}
```

#### 语音 (type=3)

```json
{
  "type": 3,
  "voice_item": {
    "media": {
      "encrypt_query_param": "...",
      "aes_key": "<base64>",
      "full_url": "https://..."
    },
    "encode_type": 6,
    "bits_per_sample": 16,
    "sample_rate": 24000,
    "playtime": 3000,
    "text": "语音转文字（可选）"
  }
}
```

#### 文件 (type=4)

```json
{
  "type": 4,
  "file_item": {
    "media": {
      "encrypt_query_param": "...",
      "aes_key": "<base64>",
      "full_url": "https://..."
    },
    "file_name": "document.pdf",
    "md5": "文件的MD5",
    "len": "123456"
  }
}
```

#### 视频 (type=5)

```json
{
  "type": 5,
  "video_item": {
    "media": {
      "encrypt_query_param": "...",
      "aes_key": "<base64>",
      "full_url": "https://..."
    },
    "video_size": 123456,
    "play_length": 30000,
    "video_md5": "...",
    "thumb_media": {
      "encrypt_query_param": "...",
      "aes_key": "<base64>"
    },
    "thumb_size": 1234,
    "thumb_height": 240,
    "thumb_width": 320
  }
}
```

### 8.3 入站消息处理优先级

1. 如果消息有文本 item（type=1），取文本内容作为消息正文
2. 如果文本 item 带有 `ref_msg`，则格式化为引用格式
3. 如果消息是语音且有 `voice_item.text`（语音转文字），直接使用该文本
4. 媒体下载优先级：图片 > 视频 > 文件 > 语音
5. 如果主 item_list 中没有可下载媒体，尝试从引用消息中获取媒体

---

## 9. contextToken 机制

### 9.1 作用

`context_token` 是微信服务器分配的对话上下文令牌，用于关联同一对话的入站和出站消息。

### 9.2 生命周期

```
1. 入站消息携带 context_token
       ↓
2. 存储 (accountId, userId) → contextToken 映射
       ↓
3. 回复时在 sendMessage 中携带相同的 context_token
       ↓
4. 持久化到磁盘（JSON 文件），重启后恢复
```

### 9.3 存储结构

内存：`Map<"accountId:userId", contextToken>`
磁盘：`<stateDir>/openclaw-weixin/accounts/<accountId>.context-tokens.json`

```json
{
  "wx_user_1@im.wechat": "abc...",
  "wx_user_2@im.wechat": "def..."
}
```

### 9.4 多账号路由

当有多个 Bot 账号时，`context_token` 存储用于确定回复时使用哪个账号：
- 根据目标用户 ID 查找哪个账号有条目
- 唯一匹配时自动使用该账号
- 多个匹配或没有匹配时需指定 accountId

---

## 10. 状态管理与会话

### 10.1 同步缓存（get_updates_buf）

**持久化位置**：`<stateDir>/openclaw-weixin/accounts/<accountId>.sync.json`
**格式**：
```json
{ "get_updates_buf": "<base64-ish string>" }
```

- 首次启动传空字符串
- 每次 getUpdates 响应更新后持久化到磁盘
- 重启后从磁盘加载恢复

### 10.2 会话暂停（Session Guard）

| 触发条件 | errcode -14 (会话过期) |
|----------|----------------------|
| 暂停时长 | 1 小时 |
| 暂停范围 | 该 accountId 的所有入站/出站 API 调用 |
| 恢复方式 | 自动计时到期后恢复 |

### 10.3 账号数据存储

**注册账号列表**：`<stateDir>/openclaw-weixin/accounts.json`
```json
["abc-im-bot", "def-im-bot"]
```

**单个账号凭证**：`<stateDir>/openclaw-weixin/accounts/<accountId>.json`
```json
{
  "token": "bot_token_xxx",
  "savedAt": "2026-04-29T...",
  "baseUrl": "https://ilinkai.weixin.qq.com",
  "userId": "wx_user@im.wechat"
}
```

**授权用户列表**：`<credDir>/openclaw-weixin-<accountId>-allowFrom.json`
```json
{
  "version": 1,
  "allowFrom": ["wx_user@im.wechat"]
}
```

### 10.4 accountId 规范化

原始 ID 格式如 `a1b2c3d4@im.bot`，包含 `@` 和 `.` 字符，在文件系统中不安全。

规范化规则：
```
"a1b2c3d4@im.bot"    → "a1b2c3d4-im-bot"
"wx_user@im.wechat"  → "wx_user-im-wechat"
```

兼容性：位置存储时同时尝试规范化和原始 ID 对应的文件路径。

---

## 11. 错误处理与重试

### 11.1 getUpdates 长轮询错误

| 错误类型 | 处理方式 |
|----------|----------|
| 客户端超时 (AbortError) | 返回空响应继续轮询（正常现象） |
| 网络/网关错误（如 524） | 返回 wait 状态继续轮询 |
| ret != 0 或 errcode != 0 | 错误计数 +1 |
| errcode == -14 | 会话过期，暂停 1 小时 |
| 连续 3 次失败 | 退避 30 秒后继续 |

### 11.2 CDN 上传错误

| HTTP 状态 | 处理方式 |
|-----------|----------|
| 4xx | 客户端错误，直接抛出，不重试 |
| 5xx / 网络错误 | 最多重试 3 次，间隔递增 |
| 成功但缺 x-encrypted-param 头 | 重试（最多 3 次） |

### 11.3 API 超时

| API | 默认超时 | 说明 |
|-----|----------|------|
| getUpdates | 35s | 客户端超时正常，返回空继续 |
| sendMessage | 15s | - |
| getUploadUrl | 15s | - |
| getConfig | 10s | - |
| sendTyping | 10s | - |

### 11.4 总体重试配置

- **最长持续错误**：3 次连续失败
- **退避策略**：失败后等待 2s 重试；3 次后等待 30s
- **会话过期**：暂停所有请求 60 分钟


---

## 12. 输入状态指示（Typing Indicator）

Bot 可以发送"正在输入"状态给用户，提升交互体验。

### 12.1 获取 typing_ticket

```
POST https://ilinkai.weixin.qq.com/ilink/bot/getconfig

{
  "ilink_user_id": "<用户ID>",
  "context_token": "<可选上下文令牌>",
  "base_info": { "channel_version": "2.1.7" }
}
```

**响应**：
```json
{
  "ret": 0,
  "errmsg": "",
  "typing_ticket": "<base64 编码的 typing ticket>"
}
```

- `typing_ticket` 是后续调用 sendTyping 的凭证
- 缓存有效期：24 小时（随机时间刷新）
- 获取失败时静默重试（指数退避：2s → 4s → 8s … 最多 1 小时）

### 12.2 发送输入状态

```
POST https://ilinkai.weixin.qq.com/ilink/bot/sendtyping

{
  "ilink_user_id": "<用户ID>",
  "typing_ticket": "<从 getConfig 获取的 ticket>",
  "status": 1,
  "base_info": { "channel_version": "2.1.7" }
}
```

**status 值**：
| 值 | 含义 |
|----|------|
| 1 | 正在输入 (TYPING) |
| 2 | 取消输入 (CANCEL) |

**保活机制**：每 5 秒发送一次 typing 状态，直到消息发送完成。

---

## 13. CDN URL 构造

### 13.1 URL 模板

```
// 上传（当 upload_full_url 未返回时）
{cdnBaseUrl}/upload?encrypted_query_param={uploadParam}&filekey={filekey}

// 下载（当 full_url 未返回时）
{cdnBaseUrl}/download?encrypted_query_param={encryptedQueryParam}
```

### 13.2 URL 优先级

**上传**：
1. `upload_full_url`（服务端返回的完整 URL）→ 直接 POST
2. `upload_param` → 拼接构造

**下载**：
1. `media.full_url`（媒体字段中的完整 URL）→ 直接 GET
2. `encrypt_query_param` → 拼接构造

---

## 14. 配置与账号管理

### 14.1 账号配置来源

Bot 账号的配置来自两个层面：

1. **持久化凭证**（QR 登录后写入）：
   - `token`（bot_token）
   - `baseUrl`（API 基础 URL，可选覆盖）
   - `userId`（扫码用户的微信 ID）

2. **运行时配置**（openclaw.json）：
   - `routeTag`：路由标签
   - `cdnBaseUrl`：CDN URL 覆盖
   - `enabled`：账号启用开关

### 14.2 多账号支持

- 支持多个 Bot 账号同时在线
- 每个账号独立存储凭证、同步缓存、contextToken
- 多账号时回复消息需根据 contextToken 确定使用哪个账号

### 14.3 账号清理

当同一微信用户重复扫码登录时：
1. 新账号生成新的 accountId
2. 检查所有已有账号的 userId
3. 如果存在同一 userId 的旧账号，将其删除（凭证、同步缓存、contextToken、allowFrom 全部清理）
4. 只保留最新登录的账号

---

## 附录 A：Markdown 过滤

由于微信消息不支持标准 Markdown 语法，出站消息需要通过状态机过滤器去除以下 Markdown 元素：

| 语法 | 处理方式 |
|------|----------|
| ``` 代码块 | 直接移除（内容保留） |
| > 引用 | 移除 > 前缀 |
| #### ~ ###### 标题 | 移除 # 前缀 |
| 水平线 `---` `***` `___` | 直接移除 |
| ` 行内代码 | 只保留内容 |
| `*斜体*` `**粗体**` `***粗斜体***` | 只保留文字 |
| `_斜体_` `__粗体__` `___粗粗斜体___` | 只保留文字 |
| `~~删除线~~` | 只保留内容 |
| `![图片](url)` | 直接移除（图片用 MEDIA 机制发送） |
| 表格 `\| ... \|` | 单元格内容以 Tab 分隔 |
| 列表 `- item` `* item` | 保留文字但不保留列表格式 |
| 缩进（空格/制表符开头） | 移除前导空白 |

过滤器按字符流处理（streaming），保证在 AI 逐步生成回复时实时过滤。

---

## 附录 B：消息处理完整流程

以下是一个入站消息从接收→处理→回复的完整链路：

```
getUpdates 响应
  │
  ▼
提取消息 + context_token
  │
  ├── 检查是否为斜杠命令（/echo、/toggle-debug）
  │   └── 是 → 直接回复，跳过 AI
  │
  ▼
消息正文提取（text_item → Body）
  │
  ▼
媒体下载（如有图片/视频/文件/语音）
  │  ├── 构建 CDN 下载 URL
  │  ├── GET 获取加密数据
  │  ├── AES-128-ECB 解密
  │  └── 保存到本地文件
  │
  ▼
鉴权 (command authorization)
  │  └── 检查 allowFrom 列表
  │  └── 决定是否允许处理
  │
  ▼
路由 (resolveAgentRoute)
  │  └── 确定由哪个 Agent 处理
  │  └── 确定 sessionKey
  │
  ▼
记录入站会话 (recordInboundSession)
  │
  ▼
存储 context_token (按 accountId:userId 映射)
  │
  ▼
获取 typing_ticket（缓存，若不存在则调用 getConfig）
  │
  ▼
调用 dispatchReplyFromConfig（AI 处理 + 回复）
  │
  ├── 启动 typing indicator（每 5s 刷新）
  │
  ├── AI 生成回复（流式）
  │   └── Markdown 过滤（流式）
  │
  ├── 回复发送
  │   ├── 纯文本 → sendMessage
  │   └── 含媒体 → getUploadUrl → CDN上传 → sendMessage
  │
  └── 停止 typing indicator
```

## 附录 C：关键常量和 ID 格式

| 项目 | 格式 | 示例 |
|------|------|------|
| Bot ID | `{hex}@im.bot` | `a1b2c3d4@im.bot` |
| 微信用户 ID | `{id}@im.wechat` | `wx_user_xxx@im.wechat` |
| bot_type | `"3"` | 固定值 |
| iLink-App-Id | `"bot"` | 固定值 |
| CDN Base URL | `https://novac2c.cdn.weixin.qq.com/c2c` | |
| API Base URL | `https://ilinkai.weixin.qq.com` | |
| 会话过期 errcode | `-14` | |
| QR 登录总超时 | 480,000 ms (8 min) | |
| QR 刷新次数上限 | 3 | |

