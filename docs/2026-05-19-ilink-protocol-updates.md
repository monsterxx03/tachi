# iLink 协议更新记录（v2.1.7 → v2.4.3）

> 本文档追踪 openclaw-weixin 从 v2.1.7 到 v2.4.3 期间 iLink 协议本身的变更。
> Tachi 的 weixin channel 基于 v2.1.7 协议版本实现，以下列出了需要关注的所有协议变动。
>
> ⚠️ 本文只记录 iLink 协议层面（API 端点、请求/响应格式、状态机、类型）的改动，
> 不包含 OpenClaw 框架内部重构、日志/测试工具等与协议无关的变更。

---

## 目录

1. [版本时间线](#1-版本时间线)
2. [新 API 端点](#2-新-api-端点)
3. [QR 码登录协议变更](#3-qr-码登录协议变更)
4. [BaseInfo 新增 bot_agent 字段](#4-baseinfo-新增-bot_agent-字段)
5. [请求头变更](#5-请求头变更)
6. [getUpdates 长轮询变更](#6-getupdates-长轮询变更)
7. [客户端版本号编码](#7-客户端版本号编码)
8. [新增协议类型](#8-新增协议类型)
9. [变更汇总表](#9-变更汇总表)

---

## 1. 版本时间线

| 版本 | 日期 | 协议变更摘要 |
|------|------|-------------|
| v2.1.7 | 2026-04-07 | 基准版本（Tachi 当前实现） |
| v2.1.8 | 2026-04-07 | 无协议变更（仅 Markdown 过滤内部改动） |
| v2.1.9 | 2026-04-20 | 无协议变更（内部 Hook 机制） |
| v2.1.10 | 2026-04-24 | **新增 `notifyStart` / `notifyStop` 端点** |
| v2.3.1 | 2026-04-28 | **`get_bot_qrcode` GET → POST；新增 `need_verifycode` / `binded_redirect` 状态；新增 `bot_agent` 字段；新增 `local_token_list`** |
| v2.4.1 | 2026-05-04 | 无协议变更（仅构建/发布配置） |
| v2.4.2 | 2026-05-07 | **移除 `Content-Length` 请求头**；运行时初始化方式变更 |
| v2.4.3 | 2026-05-08 | **`readPackageJson` 向上查找修复 `iLink-App-Id` / `iLink-App-ClientVersion` 为空的问题**；`binded_redirect` 返回 `alreadyConnected` |

---

## 2. 新 API 端点

### 2.1 `ilink/bot/msg/notifystart`

新增于 v2.1.10，在 v2.3.1 中作为正式协议端点。

**用途**：通知微信服务端当前 Bot 客户端已启动上线。服务端可据此追踪 account 在线状态。

**请求**：

```
POST https://ilinkai.weixin.qq.com/ilink/bot/msg/notifystart
Authorization: Bearer <bot_token>
Content-Type: application/json

{
  "base_info": { "channel_version": "2.4.3", "bot_agent": "..." }
}
```

**响应**：

```json
{
  "ret": 0,
  "errmsg": ""
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `ret` | int | 0 = 成功 |
| `errmsg` | string | 错误信息 |

**超时**：10s（与 getConfig/sendTyping 同级）

### 2.2 `ilink/bot/msg/notifystop`

**用途**：通知微信服务端当前 Bot 客户端正在关闭下线。

**请求**：

```
POST https://ilinkai.weixin.qq.com/ilink/bot/msg/notifystop
Authorization: Bearer <bot_token>
Content-Type: application/json

{
  "base_info": { "channel_version": "2.4.3", "bot_agent": "..." }
}
```

**响应**：

```json
{
  "ret": 0,
  "errmsg": ""
}
```

**调用时机**：在 channel stop / gateway shutdown 时调用。
**超时**：10s，使用独立超时（不受 gateway abort signal 影响），确保停止通知能送达。

---

## 3. QR 码登录协议变更

### 3.1 `get_bot_qrcode`：GET → POST

v2.3.1 起，获取 QR 码的端点从 **GET** 改为 **POST**，请求体中新增 `local_token_list` 字段。

#### v2.1.7（旧）

```
GET https://ilinkai.weixin.qq.com/ilink/bot/get_bot_qrcode?bot_type=3
```

#### v2.3.1+（新）

```
POST https://ilinkai.weixin.qq.com/ilink/bot/get_bot_qrcode?bot_type=3
Content-Type: application/json
AuthorizationType: ilink_bot_token
X-WECHAT-UIN: ...

{
  "local_token_list": ["bot_token_1", "bot_token_2", ...]
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `local_token_list` | string[] | 否 | 本地最近登录的 bot_token 列表，最多 10 个。服务端据此判断是否已有绑定关系，直接返回 `binded_redirect` |

### 3.2 新增 QR 轮询状态

`get_qrcode_status` 端点的 `status` 响应字段新增三个状态值。

#### 旧状态集合（v2.1.7）

```
wait | scaned | confirmed | expired | scaned_but_redirect
```

#### 新状态集合（v2.3.1+）

```
wait | scaned | confirmed | expired | scaned_but_redirect |
need_verifycode | verify_code_blocked | binded_redirect
```

---

#### `need_verifycode`

**含义**：扫码后服务端要求输入配对码（verify code）进行二次验证。

**行为**：
1. 服务端返回 `{ "status": "need_verifycode" }`
2. Bot 客户端需要在终端提示用户输入配对码
3. 用户输入后，在 `get_qrcode_status` 的 URL 中附加 `verify_code` 参数继续轮询

```
GET https://ilinkai.weixin.qq.com/ilink/bot/get_qrcode_status
  ?qrcode=<encoded_qrcode>
  &verify_code=<user_input>
```

**配对码错误处理**：
- 输入错误的服务端返回 `need_verifycode`（可重新输入）
- 多次错误后服务端返回 `verify_code_blocked`

---

#### `verify_code_blocked`

**含义**：配对码输入错误次数过多，已锁定。

**行为**：
1. 服务端返回 `{ "status": "verify_code_blocked" }`
2. Bot 客户端需刷新二维码重新流程
3. 最多刷新 3 次二维码

---

#### `binded_redirect`

**含义**：扫描的 Bot 账号已绑定到当前客户端（通过 `local_token_list` 识别）。

**行为**：
1. 服务端返回 `{ "status": "binded_redirect" }`
2. 不需要新的凭证，本地已有 token 仍然有效
3. Bot 客户端应将此次视为"已连接"，非错误
4. 返回 `alreadyConnected: true` 给调用方

```json
{
  "status": "binded_redirect"
}
```

### 3.3 完整登录状态机

```
获取 QR 码 (POST get_bot_qrcode + local_token_list)
  │
  ▼
轮询 get_qrcode_status
  │
  ├── wait ──────────► 继续轮询
  │
  ├── scaned ────────► 显示"已扫码"，继续轮询
  │   └── (携带 verify_code 时确认验证码正确)
  │
  ├── need_verifycode ──► 提示用户输入配对码
  │   ├── 输入正确 ──► 继续轮询 (scaned → confirmed)
  │   └── 输入错误 ──► 重新输入或触发 verify_code_blocked
  │
  ├── verify_code_blocked ──► 多次错误，刷新 QR 码重试（最多 3 次）
  │
  ├── binded_redirect ──► Bot 已绑定，返回 alreadyConnected=true
  │
  ├── expired ─────────► 刷新 QR 码（最多 3 次）
  │
  ├── scaned_but_redirect ──► 切换 baseUrl 继续轮询
  │
  └── confirmed ──────► 登录成功
      返回: bot_token, ilink_bot_id, ilink_user_id, baseurl
```

---

## 4. BaseInfo 新增 bot_agent 字段

v2.3.1 起，每个 API 请求的 `base_info` 中可携带 `bot_agent` 字段。

### v2.1.7（旧）

```json
{
  "base_info": {
    "channel_version": "2.1.7"
  }
}
```

### v2.3.1+（新）

```json
{
  "base_info": {
    "channel_version": "2.4.3",
    "bot_agent": "OpenClaw"
  }
}
```

**`bot_agent` 格式**：

类似于 HTTP `User-Agent`，语法如下：

```
bot_agent = product *( SP product )
product   = name "/" version [ SP "(" comment ")" ]
name      = 1*32( ALPHA / DIGIT / "_" / "." / "-" )
version   = 1*32( ALPHA / DIGIT / "_" / "." / "+" / "-" )
comment   = 1*64( printable ASCII minus "(" ")" )
```

**规范**：
- 多个 product 用空格分隔
- version 支持 semver build metadata（如 `1.2.0-rc.1+build.5`）
- 总长度不超过 256 字节
- 仅用于观测/监控聚合，不用于认证或路由
- 未设置时默认值为 `"OpenClaw"`

**影响范围**：所有 API 请求（getUpdates、sendMessage、getUploadUrl、getConfig、sendTyping、notifyStart、notifyStop）

---

## 5. 请求头变更

### 5.1 移除 Content-Length 头

v2.4.2 起，POST 请求不再手动设置 `Content-Length` 头。

**原因**：Node 24 的 bundled undici 拒绝预先设置的 `Content-Length`，返回 `UND_ERR_INVALID_ARG: invalid content-length header`。

**变更**：`buildHeaders()` 不再读取 `body` 参数，也不再设置 `Content-Length`。由 `fetch` API 自动计算。

### 5.2 固定请求头现状

所有 POST 请求的公共请求头：

```
Content-Type: application/json
AuthorizationType: ilink_bot_token
Authorization: Bearer <bot_token>        // 可选，有 token 时设置
X-WECHAT-UIN: <base64(random_uint32_decimal_string)>
iLink-App-Id: bot                        // 从 package.json ilink_appid 读取
iLink-App-ClientVersion: <uint32>        // 从 package.json version 计算
SKRouteTag: <string>                     // 可选，从配置读取
```

**注意**：`iLink-App-Id` 现在通过向上查找 `package.json` 读取 `ilink_appid` 字段，而非硬编码。当 `package.json` 查找失败时可能为空字符串（v2.4.3 修复了编译后布局的查找路径）。

---

## 6. getUpdates 长轮询变更

### 6.1 新增 abortSignal 参数

v2.1.8~v2.1.10（逐步完善），`getUpdates` 新增外部 `abortSignal` 参数。

```typescript
async function getUpdates(params: {
  baseUrl: string;
  token?: string;
  get_updates_buf?: string;
  timeoutMs?: number;
  abortSignal?: AbortSignal;   // 新增
}): Promise<GetUpdatesResp>
```

**作用**：当 gateway 关闭 channel 时（如配置热重载），通过外部 abort signal 立即终止长轮询请求，而不是等待 35s 超时。确保 channel stop 在 gateway 的预算时间内完成。

**行为**：
- 当 `abortSignal` 触发 abort 时，`getUpdates` 返回 `{ ret: 0, msgs: [], get_updates_buf: <原buf> }`
- 调用方应检查 `abortSignal?.aborted` 决定是否退出轮询循环

### 6.2 timeoutMs 可选

v2.3.1 起，`apiPostFetch` 的 `timeoutMs` 参数变为可选。当未设置时，不设置客户端超时（依赖 OS/TCP 栈）。

---

## 7. 客户端版本号编码

`iLink-App-ClientVersion` 的计算方式与 v2.1.7 一致，无协议变更：

```
0x00MMNNPP
M = 主版本号, N = 次版本号, P = 补丁号
"2.4.3" → 0x00020403 = 132099
"2.1.7" → 0x00020107 = 131335
```

**修复**：v2.4.3 修复了 `package.json` 查找路径。之前由于编译后路径（`dist/src/api/`）和开发路径（`src/api/`）不同，`readPackageJson` 找不到 `package.json`，导致 `version` 为 `"unknown"`，`iLink-App-ClientVersion` 为 `0`。修复后使用向上遍历查找（walk-up search）确保总能找到正确的 `package.json`。

---

## 8. 新增协议类型

### 8.1 NotifyStopReq / NotifyStopResp

```typescript
// 请求
interface NotifyStopReq {
  base_info?: BaseInfo;
}

// 响应
interface NotifyStopResp {
  ret?: number;
  errmsg?: string;
}
```

### 8.2 NotifyStartReq / NotifyStartResp

```typescript
// 请求
interface NotifyStartReq {
  base_info?: BaseInfo;
}

// 响应
interface NotifyStartResp {
  ret?: number;
  errmsg?: string;
}
```

### 8.3 BaseInfo.bot_agent

```typescript
interface BaseInfo {
  channel_version?: string;
  bot_agent?: string;  // 新增 v2.3.1
}
```

---

## 9. 变更汇总表

| 类别 | 变更内容 | 引入版本 | 协议影响 |
|------|---------|---------|---------|
| **新端点** | `POST ilink/bot/msg/notifystart` | v2.1.10 | 新增端点 |
| **新端点** | `POST ilink/bot/msg/notifystop` | v2.1.10 | 新增端点 |
| **QR 登录** | `get_bot_qrcode` GET → POST + `local_token_list` | v2.3.1 | 破坏性变更 |
| **QR 登录** | 新增 `need_verifycode` 状态 + `verify_code` 参数 | v2.3.1 | 状态机扩展 |
| **QR 登录** | 新增 `verify_code_blocked` 状态 | v2.3.1 | 状态机扩展 |
| **QR 登录** | 新增 `binded_redirect` 状态 + `alreadyConnected` | v2.3.1 | 状态机扩展 |
| **请求体** | `base_info` 新增 `bot_agent` 字段 | v2.3.1 | 向后兼容（可选字段） |
| **请求头** | 移除 `Content-Length` | v2.4.2 | 向后兼容 |
| **请求头** | `iLink-App-Id` 来源改为 `package.json` 的 `ilink_appid` 字段 | v2.4.3 | 向后兼容 |
| **长轮询** | `getUpdates` 新增 `abortSignal` 参数 | v2.1.8+ | 向后兼容（可选参数） |
| **超时** | `timeoutMs` 变为可选 | v2.3.1 | 向后兼容 |
| **类型** | 新增 `NotifyStopReq/Resp`、`NotifyStartReq/Resp` | v2.1.10 | 新增协议类型 |
| **类型** | `BaseInfo` 新增 `bot_agent` 字段 | v2.3.1 | 新增字段 |

---

## 附录：Tachi 适配建议

以下是 Tachi 的 weixin channel 需要关注的关键适配点：

### P0 — 必须适配

1. **`notifyStart` / `notifyStop`**（v2.1.10+）：启动和停止 channel 时通知服务端。如果不实现，服务端可能认为 Bot 始终离线。

2. **QR 登录改造**：
   - `get_bot_qrcode` 从 GET 改为 POST + 携带 `local_token_list`
   - 处理 `need_verifycode` / `verify_code_blocked` / `binded_redirect` 三种新状态
   - 支持 `verify_code` 参数

### P1 — 建议适配

3. **`bot_agent` 字段**：在所有 API 请求的 `base_info` 中可选携带，默认值 `"Tachi/版本号"`。

4. **移除 `Content-Length`**：如果 Tachi 的 Go 实现手动设置了 `Content-Length`，建议移除，让 HTTP 客户端自动计算。

### P2 — 可选适配

5. **`abortSignal` 支持**：如果 Tachi 支持 channel 热重启/停止，建议实现外部 abort signal 机制。

6. **`package.json` 查找**：Tachi 的 Go 编译版本号来源无需变动，此修复仅影响 openclaw-weixin 的 JS 包。
