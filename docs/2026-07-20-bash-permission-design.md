# Bash 权限规则系统设计

> 版本: 1.0 | 日期: 2026-07-20 | 状态: 已实现（v1）
> 关联: [permission 包](../agent/permission/policy.go),
>       [策略接入](../agent/agent_permission.go),
>       [执行器注入点](../agent/tool_executor.go),
>       [配置](../config/config.go)

---

## 目录

1. [动机与目标](#1-动机与目标)
2. [威胁模型](#2-威胁模型)
3. [规则模型](#3-规则模型)
4. [复合命令解析](#4-复合命令解析)
5. [架构与接入点](#5-架构与接入点)
6. [三种运行模式的行为](#6-三种运行模式的行为)
7. [配置设计](#7-配置设计)
8. [确认响应三态化](#8-确认响应三态化)
9. [实现阶段与后续方向](#9-实现阶段与后续方向)

---

## 1. 动机与目标

`Bash` 工具标记了 `IsDestructive`，但在 TUI/auto 模式下没有任何确认或拦截机制；channel 模式（`PermissionModeSkip`）更是完全自动放行。LLM 拿到什么命令就直接执行，是自主运行场景下最大的事故面。

目标：为 Bash 增加 allow/ask/deny 规则系统，覆盖 TUI、ACP、channel 三种模式，同时不破坏现有行为（不配规则 = 一切照旧）。

## 2. 威胁模型

这是**护栏不是沙箱**。LLM 持有 WriteFile 本就能造成破坏，Bash 规则的目的是：

- 防止"手滑"类事故（`rm -rf`、强制推送、写块设备）
- 给用户明确的控制点（哪些命令必须经人确认）

不按安全边界的标准设计，但基本的绕过手段（复合命令拼接）必须防。

## 3. 规则模型

```yaml
permissions:
  bash:
    deny:    # 直接拒绝，错误反馈给 LLM（引导用户改配置）
      - "rm -rf /*"
      - "git push --force*"
    ask:     # 需人工确认（TUI 弹窗 / ACP 转发 / 非交互场景拒绝）
      - "git push*"
    allow:   # 豁免 ask（对匹配的命令段免确认）
      - "git status*"
    disable_builtin_deny: false  # 关闭内置危险规则（仅全局配置生效）
```

- 匹配语法：简单 glob，仅 `*` 通配（任意字节序列），大小写敏感
- 段级优先级：**deny > allow > ask > 默认 allow**
- 整条命令的判定：所有命令段中，任一 deny → deny；否则任一 ask → ask；否则 allow
- 段匹配前剥离 `sudo ` 前缀（`sudo rm -rf /` 命中 `rm -rf /` 规则）
- 默认不配任何规则 = 全部 allow（行为变化仅限内置 deny 规则，见下）

### 3.1 内置绝对危险 deny 规则

`permission.BuiltinDenyRules` 在装配时（`NewPermissionPolicyFromConfig`）自动 prepend 到全局 deny，无需任何配置即生效：

- **根/家目录删除**：`rm -rf /`、`rm -rf /*`、`rm -rf ~*`、`rm -rf $HOME*`（含 `-fr` 变体与 `rm -rf / *` 经典误打形式）
- **磁盘覆写**：`dd *of=/dev/{sd,hd,vd,xvd,nvme,mmcblk,disk,loop}*`、`mkfs*`、`wipefs`、重定向写磁盘设备（`> /dev/sd*` 等）
- **系统关机**：`shutdown`/`reboot`/`halt`/`poweroff`（含 `sudo`、`systemctl`、`init 0/6` 形式）

设计取舍：

- **刻意排除** `git push --force`（feature 分支有正当用途）、相对路径 `rm -rf`（日常清理）、`curl|sh`（安装脚本惯例）——这些属于用户策略，自行添加
- **设备路径精确前缀**，避免误伤 `/dev/null`（`dd of=/dev/null`、`> /dev/null` 合法）
- **逃生舱仅全局**：`permissions.bash.disable_builtin_deny: true` 可关闭，但只在全局 config 生效；项目级 `.tachi/permissions.yaml` 的同名键被忽略，防止克隆仓库削弱安全默认
- 策略引擎 `NewPolicy` 本身不含内置规则，保持"引擎 vs 默认配置"分离

## 4. 复合命令解析

朴素的前缀匹配会被 `git status && rm -rf /` 绕过。实现上使用 `mvdan.cc/sh/v3` 将命令解析为 AST，提取所有**简单命令段**（管道、`&&`/`||`/`;` 列表、`$()` 与反引号命令替换均覆盖），逐段判定；引号被正确处理（`echo "a;b"` 不会误切）。

重定向作为伪段参与匹配（规范化为 `> /path` 形式），使 `*> /dev/*` 类规则可拦截写设备。

无法解析的命令保守判为 ask（在非交互场景等于拒绝）。

## 5. 架构与接入点

```
LLM tool_call (Bash)
    ↓
executeToolCallsSequential
    ↓
checkBashPermission            ← 新增（agent/agent_permission.go）
    ├─ deny → ToolResultError（反馈 LLM，含规则名与配置指引）
    ├─ ask  → 按 PermissionMode 分发（见下）
    └─ allow → 正常 Registry.Invoke
```

- 策略独立于工具实现，位于 agent 层，天然可扩展到其他工具（如未来的 WriteFile/EditFile 路径规则）
- Bash `Parallel()=false`，永远走顺序执行路径，单一注入点即可覆盖
- `SubAgent`（Fork）共享父 agent 的 policy 指针：规则只读共享，session 级"始终允许"记忆也会回传父 agent

## 6. 三种运行模式的行为

| 模式 | deny | ask |
|------|------|-----|
| TUI（`PermissionModeTUI`） | 错误反馈 LLM | 复用确认弹窗：命令 + 命中规则；`y` 允许一次 / `a` 本会话始终允许（精确命令串记忆）/ `n` 拒绝并取消本轮 |
| ACP（`PermissionModeExternal`） | 错误反馈 LLM | 转发 `session/request_permission`，客户端原生 UI 决策 |
| channel / subagent / `tachi -p`（`PermissionModeSkip`） | 错误反馈 LLM | **拒绝**，提示用户加 allow 规则（无人值守场景不做交互） |

特例：ACP 客户端选择 "allow all" 后，agent 切到 `PermissionModeSkip` 并置 `autoApprovePolicyAsks=true`——用户显式选择了全部放行，ask 不再拒绝。该标志与 channel 等场景的 Skip 语义隔离，互不影响。

## 7. 配置设计

**全局** `~/.tachi/config.yaml`：`permissions.bash.{deny,ask,allow}`

**项目级** `<git-root>/.tachi/permissions.yaml`（可入 git 共享），结构镜像全局配置的 `permissions:` 段。

合并语义：deny/ask 两级 **union 合并**，项目永远只能收紧。两条例外守卫供应链安全：

- **项目级 allow 被忽略**（`NewPolicy` 只取全局 allow）——否则克隆的仓库可用 `allow: ["*"]` 静默中和用户全局 ask 哨兵；豁免权只属于用户全局配置（全局 allow 可豁免项目 ask，信任方向：用户 > 项目）
- **项目级 `disable_builtin_deny` 被忽略**——内置危险规则只能由全局配置关闭

## 8. 确认响应三态化

原有 `ConfirmTool(bool)` 升级为 `ConfirmTool(ConfirmResponse)`：

```go
ConfirmDeny        // 拒绝（本轮取消，与 EditFile 拒绝语义一致）
ConfirmAllowOnce   // 仅本次
ConfirmAllowAlways // 本次 + 记住精确命令串（session 级，仅 Bash ask 有语义）
```

- "始终允许"采用**精确命令串匹配**（空白归一化后），不做前缀泛化；复合命令整体记忆
- EditFile 等既有确认流程中 AllowAlways 等同 AllowOnce
- 全部调用方（TUI、main、channel、ACP stream、agent_loop、测试）已迁移

## 9. 实现阶段与后续方向

**v1（本次）**：策略包（mvdan/sh 解析 + glob 匹配）、全局 + 项目级配置、executor 注入、TUI 三键确认、ACP 转发与 allow-all 联动、channel/subagent/-p ask=deny、session 精确记忆。

**v2 候选**：

- "始终允许"落盘（写回 config 或项目文件）
- Discord ✅/❌ reaction 交互式审批（带超时与发起人校验）
- WriteFile/EditFile 路径规则（如禁改 `.env`、`*.pem`）
- 按渠道/agent 类型的策略 profile（如沙箱 subagent 更宽松）
