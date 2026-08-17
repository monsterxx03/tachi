import { NavLink } from 'react-router-dom'
import { getApiKey } from '../api/client'

const NAV = [
  { to: '/', label: '📊 概览', end: true },
  { to: '/sessions', label: '💬 Sessions' },
  { to: '/usage', label: '💰 Usage 明细' },
  { to: '/oneoffs', label: '🧩 OneOffs' },
]

/** Top bar: the single navigation layer (logo + tabs + status). */
export function TopBar() {
  const hasKey = getApiKey() !== ''
  return (
    <header className="h-[52px] shrink-0 bg-card border-b border-line flex items-center gap-3.5 px-5">
      <div className="flex items-center gap-1.5 mr-2">
        <span className="text-base font-bold text-accent tracking-tight">
          ⚡ Tachi Console
        </span>
      </div>

      <nav className="flex self-stretch">
        {NAV.map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.end}
            className={({ isActive }) =>
              `flex items-center px-3.5 text-sm border-b-2 border-transparent transition-colors ${
                isActive
                  ? 'text-accent font-semibold border-accent'
                  : 'text-inkdim hover:text-ink'
              }`
            }
          >
            {item.label}
          </NavLink>
        ))}
      </nav>

      <div className="flex-1" />

      <div className="flex items-center gap-1.5 text-xs text-inkdim mono bg-paper border border-line rounded px-2.5 py-1">
        <span
          className={`w-2 h-2 rounded-full ${hasKey ? 'bg-accent' : 'bg-warn'}`}
        />
        127.0.0.1:8787
      </div>
      <button className="btn" onClick={() => window.location.reload()}>
        刷新
      </button>
    </header>
  )
}