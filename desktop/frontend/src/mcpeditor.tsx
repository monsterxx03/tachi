import { useCallback, useEffect, useMemo, useState } from 'react'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import type { MCPServerEntryVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { MCPTransportType, type MCPServerConfig } from '../bindings/github.com/monsterxx03/tachi/config'
import { useMCPAuth } from './agentEvents'

// The MCP servers editor — the one block on the settings page that is NOT a config.yaml leaf.
//
// MCP servers live in mcp.json (`config.Config.MCPServers` is `yaml:"-"`), so this cannot be a row:
// it has no leaf to hang off, it does not go through SaveSettings, and it writes its own file. Two
// consequences run through the code below:
//
//   - SAVING IS PER ENTRY AND IMMEDIATE. The page's 保存 button carries the config.yaml batch, and
//     folding a second file into that transaction would mean two files with one button and no way
//     to say which half failed. Each entry therefore has its own 保存, and the block says so.
//   - THE FILE IS THE TRUTH, IN THE FORM THE USER WROTE IT. The backend hands back the RAW entry
//     (no defaults filled in), so an untouched field stays absent instead of becoming this
//     process's default the moment someone presses save. See config.LoadMCPConfigRaw.
//
// The status-bar MCP panel stays as it is: that one is a per-session shortcut (toggle, authorize,
// load a tool), and the two are allowed to overlap.

/** NEW is the key an entry being added is edited under. A name cannot collide with it: the editor
 *  refuses an empty name before it writes, and every real entry has one. */
const NEW = '\u0000new'

/** What the form holds. Every value is a STRING (or a switch) rather than a parsed one, for the
 *  page's own reason: a half-typed "30" on the way to "30s" has to survive the keystroke. */
type Draft = {
  /** base is the entry as the backend handed it over. The save starts from it, so a field this
   *  form does not show (always_load_tools, whitelist, …) is written back as it was found. */
  base: MCPServerConfig
  profile: string
  name: string
  type: string
  command: string
  args: string
  env: string
  url: string
  headers: string
  proxy: string
  timeout: string
  enabled: boolean
  oauthOn: boolean
  oauthClientID: string
  oauthClientSecret: string
  oauthScopes: string
  oauthMetadataURL: string
}

const lines = (s: string) => s.split('\n').map((x) => x.trim()).filter((x) => x !== '')

/** pairsOf parses the one-per-line "KEY=VALUE" / "Name: value" blocks. The separator is the one
 *  the field is written with (env uses `=`, headers use `:`), and a line without it is kept as a
 *  value-less key rather than dropped — a header the user typed half of must not vanish silently. */
function pairsOf(s: string, sep: string): Record<string, string> | undefined {
  const out: Record<string, string> = {}
  for (const line of lines(s)) {
    const at = line.indexOf(sep)
    if (at < 0) out[line] = ''
    else out[line.slice(0, at).trim()] = line.slice(at + 1).trim()
  }
  return Object.keys(out).length > 0 ? out : undefined
}

const block = (m?: { [k: string]: string | undefined } | null, sep = '=') =>
  Object.entries(m ?? {}).map(([k, v]) => `${k}${sep}${v}`).join('\n')

function draftOf(entry: MCPServerEntryVO): Draft {
  const s = entry.server
  return {
    base: s,
    profile: entry.profile ?? '',
    name: s.name,
    type: s.type || MCPTransportType.MCPTransportStdio,
    command: s.command ?? '',
    args: (s.args ?? []).join('\n'),
    env: block(s.env, '='),
    url: s.url ?? '',
    headers: block(s.headers, ': '),
    proxy: s.proxy ?? '',
    timeout: typeof s.timeout === 'string' ? s.timeout : '',
    enabled: s.enabled !== false,
    oauthOn: !!s.oauth,
    oauthClientID: s.oauth?.client_id ?? '',
    oauthClientSecret: s.oauth?.client_secret ?? '',
    oauthScopes: (s.oauth?.scopes ?? []).join(' '),
    oauthMetadataURL: s.oauth?.auth_server_metadata_url ?? '',
  }
}

/** toServer turns the form back into a server entry. It starts from the entry the backend handed
 *  over and overwrites what the form owns, so the fields this form does not show survive a save.
 *
 *  A field the user CLEARED is dropped rather than written empty: `"proxy": ""` is not what an
 *  absent proxy means (the loader's default is the absence), and an entry that keeps a stale empty
 *  string is how a config file fills up with keys nobody chose. `enabled` is the one exception
 *  that is written down — false always, true only when the file already said it — because the
 *  key's ABSENCE means enabled, and the panel's toggle writes its choice down for the same reason
 *  (see config.SetServerEnabledInFile). */
function toServer(d: Draft): MCPServerConfig {
  const s: MCPServerConfig = { ...d.base, name: d.name.trim(), type: d.type as MCPTransportType }
  const setOrDrop = (key: keyof MCPServerConfig, value: unknown) => {
    if (value === undefined || value === null || value === '') delete s[key]
    else (s as unknown as Record<string, unknown>)[key] = value
  }
  setOrDrop('command', d.command.trim())
  setOrDrop('args', lines(d.args))
  setOrDrop('env', pairsOf(d.env, '='))
  setOrDrop('url', d.url.trim())
  setOrDrop('headers', pairsOf(d.headers, ':'))
  setOrDrop('proxy', d.proxy.trim())
  setOrDrop('timeout', d.timeout.trim())
  if (!d.enabled || d.base.enabled !== undefined) s.enabled = d.enabled
  else delete s.enabled
  if (d.type === MCPTransportType.MCPTransportHTTP && d.oauthOn) {
    s.oauth = {
      client_id: d.oauthClientID.trim() || undefined,
      client_secret: d.oauthClientSecret.trim() || undefined,
      scopes: d.oauthScopes.split(/[\s,]+/).filter(Boolean),
      auth_server_metadata_url: d.oauthMetadataURL.trim() || undefined,
    }
  } else {
    delete s.oauth
  }
  return s
}

const sourceLabel = (entry: MCPServerEntryVO) =>
  entry.profile ? `mcp.${entry.profile}.json` : 'mcp.json'

export function MCPServersEditor() {
  const [entries, setEntries] = useState<MCPServerEntryVO[]>([])
  const [prof, setProf] = useState<{ active: string; available: string[] }>({ active: '', available: [] })
  const [open, setOpen] = useState<string | null>(null)
  const [draft, setDraft] = useState<Draft | null>(null)
  const [notice, setNotice] = useState<{ bad: boolean; text: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const [loadError, setLoadError] = useState('')

  const load = useCallback(async () => {
    const [list, profiles] = await Promise.all([
      AgentService.ListMCPServerEntries().catch((e) => { setLoadError(String(e)); return null }),
      AgentService.ListMCPProfiles().catch(() => null),
    ])
    if (list) {
      setLoadError('')
      setEntries(list)
    }
    if (profiles) setProf({ active: profiles.active ?? '', available: profiles.available ?? [] })
  }, [])

  useEffect(() => { void load() }, [load])

  // A server that finished authorizing has a connection (and tools) it did not have a moment ago,
  // and this list holds its state by value — the panel re-reads for the same reason.
  const refresh = useCallback(() => { void load() }, [load])
  const { auth } = useMCPAuth(refresh)

  const openEntry = (key: string, next: Draft) => {
    setOpen(key)
    setDraft(next)
    setNotice(null)
  }

  const close = () => {
    setOpen(null)
    setDraft(null)
  }

  const save = useCallback(async () => {
    if (!draft) return
    setBusy(true)
    const original = entries.find((e) => e.server.name === draft.base.name)
    const res = await AgentService.SaveMCPServerEntry({
      server: toServer(draft),
      profile: draft.profile,
      // A rename has to REMOVE the old entry, or the file ends up defining one server twice —
      // which the loader reads as a duplicate name and refuses outright.
      previousName: original && original.server.name !== draft.name.trim() ? original.server.name : '',
    }).catch((e) => String(e))
    setBusy(false)
    // 「已写入配置，但…」 is a save that WORKED: the file is what the user asked for and the
    // sentence after the 但 is a warning about the running process. Painting it red would say the
    // opposite of what happened. The evaluate happens once, here, so the notice and the reload
    // cannot disagree about whether it landed.
    const ok = res === 'ok' || res.startsWith('已生效')
    setNotice({ bad: !ok, text: res || '保存没有返回结果' })
    if (ok) {
      close()
      await load()
    }
  }, [draft, entries, load])

  const remove = useCallback(async (entry: MCPServerEntryVO) => {
    setBusy(true)
    const res = await AgentService.DeleteMCPServerEntry(entry.server.name, entry.profile ?? '')
      .catch((e) => String(e))
    setBusy(false)
    const ok = res === 'ok' || res.startsWith('已生效')
    setNotice({ bad: !ok, text: res || '删除没有返回结果' })
    if (open === entry.server.name) close()
    await load()
  }, [load, open])

  const pending = useMemo(() => (open === NEW ? '新 server' : `编辑「${open}」`), [open])

  return (
    <div className="set-section mcp-servers">
      <div className="set-section-head">
        MCP servers
        <span className="mcp-servers-file">
          ~/.tachi/mcp.json{prof.active ? ` + mcp.${prof.active}.json` : ''}
        </span>
      </div>
      {/* The page's contract is "nothing is written until 保存", and this block is the exception.
          Saying so once, here, is cheaper than a reader discovering it per entry. */}
      <div className="mcp-servers-hint">
        这一块直接写 mcp.json：每条的「保存」立即生效，与页面底部的「保存」无关。
      </div>

      {loadError ? <div className="settings-error">读不到 mcp.json：{loadError}</div> : null}

      <div className="mcp-servers-list">
        {entries.length === 0 && !loadError ? (
          <div className="set-add-empty">还没有 MCP server。</div>
        ) : null}
        {entries.map((entry) => {
          const key = entry.server.name
          const state = auth[key]
          return (
            <div className={`mcp-entry${open === key ? ' is-open' : ''}`} key={key}>
              <div className="mcp-entry-head">
                <span className="mcp-entry-name">{key}</span>
                <span className="mcp-entry-type">{entry.server.type || 'stdio'}</span>
                <span className="mcp-entry-src" title={sourceLabel(entry)}>{sourceLabel(entry)}</span>
                {entry.server.enabled === false ? <span className="mcp-entry-off">已禁用</span> : null}
                {/* The authorization button lives HERE as well as in the panel: this is the page
                    where an oauth server gets configured, and a server that cannot connect until a
                    browser round trip is a row that has to offer it. */}
                {entry.server.type === MCPTransportType.MCPTransportHTTP && entry.server.oauth ? (
                  <button className="btn ghost" disabled={busy}
                    onClick={() => { void AgentService.AuthorizeMCPServer(key).catch((e) => setNotice({ bad: true, text: String(e) })) }}>
                    授权
                  </button>
                ) : null}
                <button className="btn ghost" disabled={busy}
                  onClick={() => openEntry(key, draftOf(entry))}>编辑</button>
                <button className="btn ghost" disabled={busy}
                  onClick={() => void remove(entry)}>删除</button>
              </div>
              {state?.message ? <div className="mcp-entry-auth">{state.message}</div> : null}
              {open === key && draft ? <EntryForm d={draft} set={setDraft} profiles={prof.available}
                busy={busy} onSave={() => void save()} onCancel={close} /> : null}
            </div>
          )
        })}
      </div>

      {open === NEW && draft ? (
        <div className="mcp-entry is-open">
          <div className="mcp-entry-head"><span className="mcp-entry-name">新 server</span></div>
          <EntryForm d={draft} set={setDraft} profiles={prof.available} isNew
            busy={busy} onSave={() => void save()} onCancel={close} />
        </div>
      ) : (
        <div className="set-add">
          <button className="btn" disabled={busy} onClick={() => openEntry(NEW, draftOf({
            server: { name: '', type: MCPTransportType.MCPTransportStdio },
            profile: '',
          }))}>＋ 添加 server</button>
        </div>
      )}

      {notice ? (
        <div className={`settings-notice${notice.bad ? ' is-bad' : ''}`}>
          {pending ? `${pending}：` : ''}{notice.text}
        </div>
      ) : null}
    </div>
  )
}

/** EntryForm is the fields of ONE server. It is deliberately a form with its own 保存 rather than
 *  inputs bound to the page's drafts: what it writes is not config.yaml, and a page-wide save that
 *  carried half of a batch it cannot report on is worse than a second button. */
function EntryForm({ d, set, profiles, busy, isNew, onSave, onCancel }: {
  d: Draft
  set: (d: Draft) => void
  profiles: string[]
  busy: boolean
  isNew?: boolean
  onSave: () => void
  onCancel: () => void
}) {
  const patch = (p: Partial<Draft>) => set({ ...d, ...p })
  const http = d.type === MCPTransportType.MCPTransportHTTP
  // Each control carries `data-field` (the draft key it edits). The page's own rows identify
  // themselves by their config path, and this form has no paths — a driver that had to find
  // "the third input" would break the moment a field moved, and the smoke suite drives this form.
  const text = (label: string, key: keyof Draft, placeholder?: string) => (
    <label className="mcp-field">
      <span className="mcp-field-label">{label}</span>
      <input className="set-input" data-field={key} value={String(d[key] ?? '')} placeholder={placeholder}
        onChange={(e) => patch({ [key]: e.target.value } as Partial<Draft>)} />
    </label>
  )
  const area = (label: string, key: keyof Draft, placeholder: string) => (
    <label className="mcp-field">
      <span className="mcp-field-label">{label}</span>
      <textarea className="set-input set-textarea" data-field={key} rows={3} value={String(d[key] ?? '')}
        placeholder={placeholder} onChange={(e) => patch({ [key]: e.target.value } as Partial<Draft>)} />
    </label>
  )

  return (
    <div className="mcp-form">
      <div className="mcp-form-grid">
        {text('名字', 'name', 'github')}
        <label className="mcp-field">
          <span className="mcp-field-label">类型</span>
          <select className="set-input" data-field="type" value={d.type}
            onChange={(e) => patch({ type: e.target.value })}>
            <option value={MCPTransportType.MCPTransportStdio}>stdio（本地进程）</option>
            <option value={MCPTransportType.MCPTransportHTTP}>http（远程地址）</option>
          </select>
        </label>
        <label className="mcp-field">
          <span className="mcp-field-label">写在哪个文件</span>
          {/* A NEW server is the only one that can choose: an existing entry is edited where it
              lives, because the same name in the other file is a DIFFERENT entry — moving one
              would have to delete the other, and "编辑" is not where that belongs. */}
          <select className="set-input" data-field="profile" value={d.profile} disabled={!isNew}
            title={isNew ? '' : '已存在的 server 写回它来自的文件；要换文件就先删再加'}
            onChange={(e) => patch({ profile: e.target.value })}>
            <option value="">mcp.json（base，所有 profile 都加载）</option>
            {profiles.map((p) => <option key={p} value={p}>{`mcp.${p}.json（profile ${p}）`}</option>)}
          </select>
        </label>
      </div>

      {http ? (
        <div className="mcp-form-grid">
          {text('url', 'url', 'https://example.com/mcp')}
          {text('proxy', 'proxy', 'socks5://127.0.0.1:1080（可空）')}
        </div>
      ) : (
        <div className="mcp-form-grid">
          {text('command', 'command', '/usr/local/bin/mcp-server')}
        </div>
      )}

      <div className="mcp-form-grid">
        {!http ? area('args（每行一个）', 'args', '--flag\nvalue') : null}
        {area('env（每行 KEY=VALUE）', 'env', 'API_KEY=xxx')}
        {http ? area('headers（每行 Name: value）', 'headers', 'Authorization: Bearer xxx') : null}
      </div>

      <div className="mcp-form-grid">
        {text('timeout', 'timeout', '30s（留空 = 默认 10s）')}
        <label className="mcp-field mcp-check">
          <input type="checkbox" data-field="enabled" checked={d.enabled}
            onChange={(e) => patch({ enabled: e.target.checked })} />
          <span>启用</span>
        </label>
      </div>

      {http ? (
        <div className="mcp-form-grid">
          <label className="mcp-field mcp-check">
            <input type="checkbox" data-field="oauthOn" checked={d.oauthOn}
              onChange={(e) => patch({ oauthOn: e.target.checked })} />
            <span>需要 OAuth2（保存后在这一行点「授权」）</span>
          </label>
          {d.oauthOn ? (
            <>
              {text('client_id', 'oauthClientID', '留空 = 自动注册（DCR）')}
              {text('client_secret', 'oauthClientSecret', '可空')}
              {text('scopes', 'oauthScopes', '空格或逗号分隔')}
              {text('auth server metadata url', 'oauthMetadataURL', '留空 = 自动发现')}
            </>
          ) : null}
        </div>
      ) : null}

      <div className="mcp-form-actions">
        <button className="btn" onClick={onSave} disabled={busy}>{busy ? '保存中…' : '保存'}</button>
        <button className="btn ghost" onClick={onCancel} disabled={busy}>取消</button>
      </div>
    </div>
  )
}
