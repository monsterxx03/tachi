import { useState } from 'react'
import { setApiKey } from '../api/client'

/** Full-screen API key entry shown when the backend rejects requests. */
export function ApiKeyGate({ onSuccess }: { onSuccess: () => void }) {
  const [key, setKey] = useState('')
  const [saved, setSaved] = useState(false)

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    if (!key.trim()) return
    setApiKey(key.trim())
    setSaved(true)
    onSuccess()
  }

  if (saved) {
    return (
      <div className="flex items-center justify-center h-full bg-paper">
        <div className="text-sm text-inkdim">已保存，正在重新请求…</div>
      </div>
    )
  }

  return (
    <div className="flex items-center justify-center h-full bg-paper">
      <form
        onSubmit={submit}
        className="card px-8 py-8 w-[420px] flex flex-col gap-5"
      >
        <div>
          <div className="flex items-center gap-2 mb-1">
            <span className="text-xl">⚡</span>
            <h1 className="text-lg font-bold text-accent tracking-tight">
              Tachi Console
            </h1>
          </div>
          <p className="text-[13px] text-inkdim">
            后端已启用 API key 鉴权（config.yaml
            <span className="mono text-[12px]"> web.api_key</span>）。
            请输入 key 以访问会话与费用数据。
          </p>
        </div>

        <input
          type="password"
          autoFocus
          value={key}
          onChange={(e) => setKey(e.target.value)}
          placeholder="API key (X-Api-Key)"
          className="mono w-full border border-line rounded px-3 py-2 text-sm bg-paper focus:outline-none focus:border-accent"
        />

        <div className="flex items-center justify-between">
          <span className="text-[11px] text-muted">
            Key 仅保存在本地 cookie 中
          </span>
          <button type="submit" className="btn-primary" disabled={!key.trim()}>
            进入控制台
          </button>
        </div>
      </form>
    </div>
  )
}