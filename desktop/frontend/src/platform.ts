import { Flags } from '@wailsio/runtime'

/**
 * The platform's shortcut modifier, and the ONE place that fact is decided.
 *
 * Tachi's shortcuts were written on macOS, where every gesture rides ⌘ and the UI spells it
 * "⌘". On Linux (GTK/WebKit) the same gestures ride Ctrl, and "⌘" is a key nobody there has —
 * so BOTH the handler and the on-screen keycap have to follow the platform. They live together
 * here because they are one fact: a handler that reads the wrong modifier is a dead key, and a
 * label that names the wrong one sends the reader after a key that does nothing.
 *
 * The answer comes from the BACKEND — GOOS, handed over as a Wails flag (see desktop/main.go) —
 * rather than from a user-agent sniff: the process that is running is the process that knows,
 * and a webview string is a guess that ages. The sniff is only the fallback for a page with no
 * runtime behind it (a browser preview, a test page), where "which key do you have" is all we
 * need and the engine's own platform string is as good an answer as any.
 *
 * A note for whoever reads the comments elsewhere in this tree: they keep saying ⌘ / ⌘K / ⌘F.
 * That is the GESTURE's name in this codebase — the design's word for it, stable across
 * platforms — not the keycap the reader sees. Only strings rendered on screen go through
 * MOD_KEY.
 */
function detectOS(): string {
  try {
    const os = Flags.GetFlag('os')
    if (typeof os === 'string' && os) return os
  } catch {
    // No Wails runtime in this page (browser preview / test page) — fall through to the sniff.
  }
  const hint = `${navigator.platform || ''} ${navigator.userAgent || ''}`.toLowerCase()
  if (hint.includes('mac')) return 'darwin'
  if (hint.includes('linux')) return 'linux'
  return ''
}

const os = detectOS()

/**
 * The one place the platform is written down for things that CANNOT import it: the stylesheets
 * (a box that only macOS needs, e.g. the room a folded-away traffic-light strip buys back) and
 * the smoke drivers (which live inside the app's page and assert what is on screen). Stamped on
 * the document root so one attribute serves both. "unknown" is a page with no runtime and no
 * recognisable engine — treated as NOT macOS everywhere, because a browser preview has no
 * traffic lights either.
 */
if (typeof document !== 'undefined') {
  document.documentElement.dataset.platform = os || 'unknown'
}

/**
 * IS_MAC is true only on macOS. Prefer it over spelling `os === 'darwin'` at a call site: the
 * string is the BACKEND's word for the platform (GOOS), and there is exactly one place that has
 * to know it.
 */
export const IS_MAC = os === 'darwin'

/**
 * isMod reports whether THIS event carries the platform's shortcut modifier. Every shortcut
 * handler goes through it, so making a new platform work is one edit here instead of one per
 * key — and no handler can silently disagree with the keycap.
 */
export function isMod(e: { metaKey: boolean; ctrlKey: boolean }): boolean {
  return os === 'darwin' ? e.metaKey : e.ctrlKey
}

/**
 * MOD_KEY is the modifier as it is PRINTED: "⌘" on macOS, "Ctrl" elsewhere. It is for keycaps
 * and copy only — never for deciding, which is isMod's job.
 */
export const MOD_KEY = os === 'darwin' ? '⌘' : 'Ctrl'

/**
 * REVEAL_LABEL names the file manager a 「显示所在位置」 action opens — Finder on macOS, "文件管理器"
 * elsewhere. The backend already routes the action per platform (attach_*.go); this is the other
 * half, the word on the button, so the label cannot promise Finder on a machine that has none.
 */
export const REVEAL_LABEL = os === 'darwin' ? '在 Finder 中显示' : '在文件管理器中显示'

