import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Events } from '@wailsio/runtime'
import {
  LauncherService,
  type LauncherHotkeyVO,
  type LauncherItem,
} from '../bindings/github.com/monsterxx03/tachi/desktop'
import { imeActive, listNav, toLocalAsset, useFilterInput } from './lib'

// The launcher panel — the page the second window loads (main.tsx picks it by its query string).
//
// It is a list, a box and three keys. The rules that make it feel like a launcher rather than a
// dialog live here, not in the styling:
//
//   - every open STARTS OVER: the panel is remounted (see Launcher below), so a query from last
//     time, a scroll position and a highlight cannot survive into the next press;
//   - the IME owns the keyboard while a composition is up — Enter that confirms a candidate must
//     never also open an app (the same guard the ⌘K palette uses, see palette.tsx);
//   - the list keeps its own selection: ↑↓ / Ctrl+N·P walk it, and the row the reader sees
//     highlighted is the row Enter acts on.

// Panel geometry, in the same numbers the CSS uses. The Go side clamps what it is given (see
// launcher.resize); these are what the page asks for.
const ROW_HEIGHT = 44
const PANEL_CHROME = 58 + 34 // the input row plus the hint row
const MAX_VISIBLE_ROWS = 8

// What the panel is when nothing is typed yet: the rows a person reaches for most, which is what
// the catalog's own order is (usage first, then the alphabet).
export default function Launcher() {
  const [generation, setGeneration] = useState(0)

  useEffect(() => {
    const off = Events.On('launcher:open', () => setGeneration((n) => n + 1))
    return () => off()
  }, [])

  // The key forces a fresh box — and with it an empty query, a zeroed highlight and an input that
  // has just been focused.
  return <LauncherBox key={generation} />
}

function LauncherBox() {
  const filter = useFilterInput()
  const [items, setItems] = useState<LauncherItem[]>([])
  const [selected, setSelected] = useState(0)
  const [hotkey, setHotkey] = useState<LauncherHotkeyVO>({ shortcut: '' })
  // The catalog is refreshed in the background (see launcher.go's refreshAsync): icons get drawn,
  // an app installed a minute ago appears. A bump re-lists — without it a panel opened in the first
  // seconds of a run would keep its letter tiles, and a freshly installed app would be missing until
  // the next press.
  const [catalogVersion, setCatalogVersion] = useState(0)
  // What went wrong with the last Enter. It is the ONLY feedback a failed launch can give: the panel
  // stays open precisely so this line has somewhere to be said (see launcher.go's launch).
  const [error, setError] = useState('')
  const inputRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)
  // Every query is answered asynchronously, and a fast typist can have three in flight: only the
  // newest answer may land, or the list would settle on a prefix of what the box says.
  const answeredRef = useRef(0)

  useEffect(() => {
    inputRef.current?.focus()
    LauncherService.Hotkey()
      .then(setHotkey)
      .catch(() => setHotkey({ shortcut: '' }))
    const off = Events.On('launcher:changed', () => setCatalogVersion((n) => n + 1))
    return () => off()
  }, [])

  // The panel is shown and hidden many times over one process's life, and the webview keeps its
  // focus state across that: when the window becomes key again the box must already own the
  // keyboard, or the first thing typed after a press goes to whatever was focused before.
  useEffect(() => {
    const refocus = () => inputRef.current?.focus()
    window.addEventListener('focus', refocus)
    return () => window.removeEventListener('focus', refocus)
  }, [])

  // A new query starts at the top of the list; a re-list of the SAME query (icons arriving) must
  // not move the highlight out from under the reader's finger.
  const lastQueryRef = useRef(filter.committed)

  useEffect(() => {
    const asked = ++answeredRef.current
    const query = filter.committed
    LauncherService.List(query)
      .then((rows) => {
        if (asked !== answeredRef.current) return
        const next = rows ?? []
        setItems(next)
        if (query !== lastQueryRef.current) {
          lastQueryRef.current = query
          setSelected(0)
          // A fresh query is a fresh attempt: yesterday's failure is not about what is being typed.
          setError('')
        } else {
          setSelected((current) => Math.min(current, Math.max(0, next.length - 1)))
        }
      })
      .catch(() => {
        if (asked === answeredRef.current) setItems([])
      })
  }, [filter.committed, catalogVersion])

  // The window follows the list: a query that leaves two matches must not leave a tall empty
  // panel behind, and a full list must not be a window nobody can scroll.
  useEffect(() => {
    const rows = Math.min(items.length, MAX_VISIBLE_ROWS)
    LauncherService.Resize(PANEL_CHROME + rows * ROW_HEIGHT).catch(() => {})
  }, [items.length])

  useEffect(() => {
    const row = listRef.current?.querySelector<HTMLElement>(`[data-index="${selected}"]`)
    row?.scrollIntoView({ block: 'nearest' })
  }, [selected])

  const launch = useCallback((item: LauncherItem | undefined) => {
    if (!item) return
    LauncherService.Launch(item.path).catch((err: unknown) => {
      // The backend starts the app and only then closes the panel, so a rejection means the panel
      // is still up and this is the moment to say why nothing happened.
      setError(describeLaunchError(err))
      // The row is probably gone (uninstalled/moved) — re-list so it stops offering itself.
      setCatalogVersion((n) => n + 1)
    })
  }, [])

  const onKeyDown = (e: React.KeyboardEvent) => {
    // A composing IME owns these keys until its candidate list lands: Enter that picks 微 must not
    // also open the first row, and ↑↓ belong to the candidate list. Esc is deliberately left to it.
    if (filter.composing() || imeActive(e)) {
      if (e.key === 'Enter' || e.key.startsWith('Arrow') || e.key === 'Home' || e.key === 'End') {
        e.preventDefault()
      }
      return
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      LauncherService.Hide().catch(() => {})
      return
    }
    if (e.key === 'Enter') {
      e.preventDefault()
      launch(items[selected] ?? items[0])
      return
    }
    const step = listNav(e)
    if (step !== 0) {
      e.preventDefault()
      setSelected((current) => {
        if (items.length === 0) return 0
        const next = current + step
        if (next < 0) return items.length - 1
        if (next >= items.length) return 0
        return next
      })
    }
  }

  // The bundle's location only earns its place when two rows share a name — otherwise every row
  // would carry 「/Applications」 as noise.
  const duplicatedNames = useMemo(() => {
    const seen = new Map<string, number>()
    for (const item of items) seen.set(item.name, (seen.get(item.name) ?? 0) + 1)
    return new Set([...seen].filter(([, n]) => n > 1).map(([name]) => name))
  }, [items])

  return (
    <div className="launcher-panel" onKeyDown={onKeyDown}>
      <div className="launcher-input-row">
        <span className="launcher-glyph" aria-hidden>
          ⌘
        </span>
        <input
          ref={inputRef}
          className="launcher-input"
          placeholder="启动应用"
          autoComplete="off"
          autoCorrect="off"
          autoCapitalize="off"
          {...filter.inputProps}
        />
      </div>

      <div
        className="launcher-list"
        ref={listRef}
        role="listbox"
        // Clicking the empty part of the list dismisses the panel, the same as clicking outside it
        // or pressing Esc: a launcher whose blank area swallows clicks feels stuck.
        onMouseDown={(e) => {
          if (e.target === e.currentTarget) LauncherService.Hide().catch(() => {})
        }}
      >
        {items.map((item, index) => (
          <div
            key={item.path}
            data-index={index}
            role="option"
            aria-selected={index === selected}
            className="launcher-row"
            data-selected={index === selected}
            onMouseMove={() => setSelected(index)}
            onMouseDown={(e) => {
              // mousedown, not click: the panel hides on focus loss, and a click that starts a
              // focus change would take the row away before the click landed.
              e.preventDefault()
              launch(item)
            }}
          >
            <AppTile item={item} />
            <span className="launcher-name">{item.name}</span>
            {duplicatedNames.has(item.name) && item.detail && (
              <span className="launcher-detail">{item.detail}</span>
            )}
          </div>
        ))}
        {items.length === 0 && <div className="launcher-empty">没有匹配的应用</div>}
      </div>

      <div className="launcher-hint">
        {error ? (
          <span className="launcher-error" role="status">
            {error}
          </span>
        ) : (
          <span>
            <kbd>↑</kbd>
            <kbd>↓</kbd> 选择 <kbd>⏎</kbd> 打开 <kbd>esc</kbd> 关闭
          </span>
        )}
        <span className="launcher-hotkey">
          {hotkey.error ? `${hotkey.shortcut} 被占用` : hotkey.shortcut}
        </span>
      </div>
    </div>
  )
}

// describeLaunchError turns what the binding rejected with into the sentence the hint row shows.
// The backend's message is already a plain sentence (it knows the app's name); this only strips the
// wrapper the binding layer puts in front of it.
function describeLaunchError(err: unknown): string {
  const raw = err instanceof Error ? err.message : String(err ?? '')
  return raw.replace(/^Error:\s*/, '') || '这个应用没能启动'
}

// AppTile is an app's picture — the bundle's real icon when the backend has drawn one, else a
// letter tile. The icon is a FILE the panel fetches over /local (see launcher.go's generateIcons),
// so it can fail for reasons the list cannot know; on failure the row keeps its letter rather than
// showing a broken image.
function AppTile({ item }: { item: LauncherItem }) {
  const [failed, setFailed] = useState(false)
  if (item.icon && !failed) {
    return (
      <img
        className="launcher-icon"
        src={toLocalAsset(item.icon, '')}
        alt=""
        draggable={false}
        onError={() => setFailed(true)}
      />
    )
  }
  return (
    <span className="launcher-tile" style={tileHue(item.name)} aria-hidden>
      {tileGlyph(item.name)}
    </span>
  )
}

// tileGlyph is the letter on an app's tile: its first character, upper-cased for the Latin ones.
// It is what a row draws until its icon exists (icons are drawn in the background, see
// launcher.go's generateIcons).
function tileGlyph(name: string): string {
  const first = Array.from(name.trim())[0] ?? '?'
  return /[a-z]/i.test(first) ? first.toUpperCase() : first
}

// tileHue gives every app a stable colour out of its own name — same row, same tint, every open.
function tileHue(name: string): React.CSSProperties {
  let hash = 0
  for (const ch of name) {
    hash = (hash * 31 + (ch.codePointAt(0) ?? 0)) % 360
  }
  return { ['--tile-hue' as string]: String(hash) } as React.CSSProperties
}
