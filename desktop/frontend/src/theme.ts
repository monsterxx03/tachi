// Theme resolution for the desktop UI.
//
// The active theme lives on <html data-theme="light|dark"> and every colour in
// the stylesheets hangs off that attribute (see base.css) — no CSS rule reads
// prefers-color-scheme, so there is exactly one definition per mode and the
// manual switch is the only thing that moves it.
//
// Three parties need to agree on the value:
//   • index.html's inline boot script — sets the attribute before the first
//     paint (a stored choice, else the system preference);
//   • this module — the runtime owner: follows the system while no choice is
//     stored, persists the user's choice when there is one;
//   • Go (desktop/uitheme.go) — owns the window colour *outside* the webview
//     (pre-paint frame) and the native appearance for the next launch. It is
//     told the user's *choice* (not the resolved theme) over the 'ui:theme'
//     event, so an untouched switch keeps the native window following the OS.
import { useCallback, useEffect, useSyncExternalStore } from 'react'
import { Events } from '@wailsio/runtime'

export type Theme = 'light' | 'dark'

// Mirrored by the inline boot script in index.html — both must stay identical.
const STORAGE_KEY = 'tachi.desktop.theme'
const SYSTEM_QUERY = '(prefers-color-scheme: dark)'
// Mirrored by Go (see uiThemeEventName in desktop/uitheme.go).
const THEME_EVENT = 'ui:theme'

/** The OS preference, used while the user has never picked a theme. */
export function systemTheme(): Theme {
  return window.matchMedia?.(SYSTEM_QUERY).matches ? 'dark' : 'light'
}

/** The user's explicit choice, or null when they follow the system. */
export function storedTheme(): Theme | null {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    return v === 'light' || v === 'dark' ? v : null
  } catch {
    return null // storage can be blocked; the app just stays session-only
  }
}

/** What is on screen right now (the boot script has already resolved it). */
export function currentTheme(): Theme {
  const attr = document.documentElement.dataset.theme
  return attr === 'dark' || attr === 'light' ? attr : systemTheme()
}

const listeners = new Set<() => void>()

/** subscribeTheme registers cb for every applied theme change. */
export function subscribeTheme(cb: () => void): () => void {
  listeners.add(cb)
  return () => {
    listeners.delete(cb)
  }
}

/** The current theme, re-rendering the caller when it changes. */
export function useThemeSnapshot(): Theme {
  return useSyncExternalStore(subscribeTheme, currentTheme, currentTheme)
}

// paintTheme applies a theme to the DOM and notifies subscribers. It remembers
// NOTHING: the stored value means "the user chose this", so a theme we merely
// followed the OS to must never be written there — storing it would quietly
// turn "follow the system" into "pin to whatever the system happened to be",
// since the only way back would be a toggle the user never asked for.
function paintTheme(theme: Theme) {
  document.documentElement.dataset.theme = theme
  listeners.forEach((cb) => cb())
}

// chooseTheme is the user's path: it paints AND remembers, so the next launch's
// boot script — and Go's window colour — resolve to the same theme.
function chooseTheme(theme: Theme) {
  try {
    localStorage.setItem(STORAGE_KEY, theme)
  } catch {
    /* storage blocked: this session still switches, it just will not persist */
  }
  paintTheme(theme)
}

/** Switches between light and dark, persisting the choice. */
export function toggleTheme() {
  chooseTheme(currentTheme() === 'dark' ? 'light' : 'dark')
}

// mirroredChoice dedupes the IPC below: several callers may ask for the same
// value (mount + change + StrictMode's double effect) and Go must hear it once.
let mirroredChoice: Theme | '' | null = null

// mirrorChoice hands Go the theme *choice* — not the theme on screen. Go reads
// it back before the window exists to pick the pre-paint colour and the native
// appearance, and an empty choice must leave that decision following the OS
// (see desktop/uitheme.go). Mirroring the resolved theme instead would pin the
// native window to whatever the system happened to be on first launch.
// Best-effort: a plain browser (vite dev outside the webview) has no runtime.
function mirrorChoice() {
  const choice: Theme | '' = storedTheme() ?? ''
  if (mirroredChoice === choice) return
  mirroredChoice = choice
  try {
    void Events.Emit(THEME_EVENT, choice).catch(() => {})
  } catch {
    /* no Wails runtime — nothing to mirror to */
  }
}

/**
 * Keeps Go's copy of the theme *choice* in step with the screen. Call once,
 * from App: it mirrors on mount (covering a launch that never toggles) and
 * again after every applied change.
 */
export function useThemeHostSync() {
  const theme = useThemeSnapshot()
  useEffect(() => {
    mirrorChoice()
  }, [theme])
}

/**
 * The titlebar switch's state: the current theme plus a toggle that persists
 * it. While no choice is stored the hook keeps following the OS (including
 * changes made while the app is running); the first toggle ends that.
 */
export function useTheme(): [Theme, () => void] {
  const theme = useThemeSnapshot()

  useEffect(() => {
    const mq = window.matchMedia?.(SYSTEM_QUERY)
    if (!mq) return
    const onSystemChange = () => {
      if (storedTheme()) return // a manual choice outranks the OS
      paintTheme(systemTheme()) // follow, without remembering (see paintTheme)
    }
    mq.addEventListener('change', onSystemChange)
    return () => mq.removeEventListener('change', onSystemChange)
  }, [])

  const toggle = useCallback(() => toggleTheme(), [])

  return [theme, toggle]
}
