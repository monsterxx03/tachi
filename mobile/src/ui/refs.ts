// 手机输入框里的引用：`#s:<id>` 指向另一段对话。
//
// 规则和桌面那份（`desktop/frontend/src/lib.ts` 的 `atRefAt` / `hashNamespace`）、以及真正做
// 展开的 `agent/atfile` 是同一条：引用从行首或空白之后开始，到下一个空白为止；`#` 后面只有
// 认得的命名空间前缀才算引用，别的一律是普通文本（`#include`、`#123` 不会被弹窗打断）。
// 三处必须一致 —— 手机列出来的候选和桌面真正会展开的东西对不上，比没有候选更糟。
//
// 两个命名空间都认：`#s:<id>` 是另一段对话，`#a:<身份>` 是一台可以派活的节点。`#a:` 的候选
// 只列**有身份**的行（`AgentPanelRow.id`）—— 一个空的 `#a:` 看着像引用、插进去解析不了
// （agentref 只按身份解析），那比没有更糟。

import type { AgentPanelRow, SessionSummary } from '../api/types'

export const HASH_REF_MARK = '#'
export const HASH_AGENT_PREFIX = 'a:'
export const HASH_SESSION_PREFIX = 's:'

/** 引用与它前面的字之间的边界，和 `atfile.IsRefBoundary` 是同一组字符。 */
export function isRefBoundary(ch: string): boolean {
  return ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r'
}

/** 光标所在的 `#` 查询；光标不在一个引用里时是 null。 */
export function hashRefAt(value: string, caret: number): { start: number; query: string } | null {
  const end = Math.max(0, Math.min(caret, value.length))
  for (let i = end - 1; i >= 0; i--) {
    const c = value[i]
    if (c === HASH_REF_MARK) {
      // 贴在一个词后面（`C#`、邮箱）：它不是引用的开头
      if (i > 0 && !isRefBoundary(value[i - 1])) return null
      return { start: i, query: value.slice(i + 1, end) }
    }
    // 在找到 `#` 之前先撞上了空白：光标不在任何引用里
    if (isRefBoundary(c)) return null
  }
  return null
}

/**
 * 一段 `#` 之后的查询是在问什么。
 *
 * `candidates` 是「`#` 还没敲完命名空间」（空查询、或 `s` —— `s:` 的前缀）：这时的候选就是
 * 会话，因为手机这里只有这一种实体引用。`none` 是别的命名空间和普通文本 —— 不弹窗。
 */
export type HashQuery =
  | { kind: 'none' }
  | { kind: 'candidates' }
  | { kind: 'sessions'; rest: string }
  | { kind: 'agents'; rest: string }

/**
 * 一段 `#` 之后的查询在问哪个命名空间。
 *
 * 两个前缀与桌面的 `hashNamespace`、以及真正做展开的 `atfile.HashQueryNamespace` 是同一条
 * 规则：`a:` 是节点、`s:` 是会话，别的一律普通文本。没敲完前缀（空查询、`a`、`s`）时是
 * candidates —— 那时候两组都可以列。
 */
export function hashQuery(query: string): HashQuery {
  if (query.startsWith(HASH_AGENT_PREFIX)) return { kind: 'agents', rest: query.slice(HASH_AGENT_PREFIX.length) }
  if (query.startsWith(HASH_SESSION_PREFIX)) return { kind: 'sessions', rest: query.slice(HASH_SESSION_PREFIX.length) }
  for (const prefix of [HASH_AGENT_PREFIX, HASH_SESSION_PREFIX]) {
    if (prefix.startsWith(query)) return { kind: 'candidates' }
  }
  return { kind: 'none' }
}

/** 引用要插进去的文本：`#s:<id>`。 */
/** 节点引用的文本：`#a:<身份>`。 */
export function agentRef(id: string): string {
  return HASH_REF_MARK + HASH_AGENT_PREFIX + id
}

/**
 * 节点候选：**只列有身份的那几行**。
 *
 * 一个空的 `#a:` 看着像引用，插进去却解析不了（agentref 只按身份/唯一前缀解析），所以本机
 * 手里没有身份的节点在这一屏上连一行都不该有 —— 这与桌面那份选择器是同一条规矩。
 */
export function matchAgents(rows: AgentPanelRow[], rest: string, limit: number): AgentPanelRow[] {
  const q = rest.trim().toLowerCase()
  const out: AgentPanelRow[] = []
  for (const row of rows) {
    if (!row.id) continue
    const name = (row.label || row.name || row.id).toLowerCase()
    if (q && !name.includes(q) && !row.id.toLowerCase().includes(q) && !(row.note || '').toLowerCase().includes(q)) continue
    out.push(row)
    if (out.length >= limit) break
  }
  return out
}

export function sessionRef(id: string): string {
  return HASH_REF_MARK + HASH_SESSION_PREFIX + id
}

/**
 * 候选行：按标题或 id 里是否含 rest 过滤（大小写无关），当前这段对话自己不出现 ——
 * 一段对话引用自己，展开出来只是一份没有信息量的自身摘要。
 */
export function matchSessions(sessions: SessionSummary[], rest: string, currentId: string, limit: number): SessionSummary[] {
  const q = rest.trim().toLowerCase()
  const rows: SessionSummary[] = []
  for (const s of sessions) {
    if (s.id === currentId) continue
    if (q && !s.title.toLowerCase().includes(q) && !s.id.toLowerCase().includes(q)) continue
    rows.push(s)
    if (rows.length >= limit) break
  }
  return rows
}

/**
 * 把 [start, end) 换成 text，返回新的文本与光标落点。
 *
 * 插入的引用后面补一个空格（后面已经跟着空白时不补，包括文本末尾）：引用到下一个空白为止，
 * 少这个空格，读者接着敲的字会被算进引用里；而且光标落在那个空格后面，输入框里的引用查询
 * 才算结束 —— 否则刚插进去的这一条会立刻把候选面板又叫回来。
 */
export function spliceRef(value: string, start: number, end: number, text: string): { value: string; caret: number } {
  const before = value.slice(0, start)
  const after = value.slice(end)
  const tail = after.length > 0 && isRefBoundary(after[0]) ? '' : ' '
  const next = before + text + tail + after
  return { value: next, caret: before.length + text.length + tail.length }
}
