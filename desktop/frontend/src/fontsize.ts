// The reading size: ⌘= / ⌘- / ⌘0, and the step survives a restart.
//
// It is a MULTIPLIER on base.css's reading tokens (--fs-prose / --fs-bubble), not a size of its
// own: those tokens stay the one place that says what prose is, and this says how much of it the
// reader wants. A message's own markdown (headings, code, tables) is sized in em off --fs-prose,
// so the whole block moves together instead of a bigger body growing past its own headings.
//
// Three parties need to agree on the value — read theme.ts first, this module is its shape:
//   • index.html's inline boot script applies the stored step BEFORE the first paint, so a
//     restored transcript never draws at the default size and then jumps;
//   • this module is the runtime owner: the keys, the clamp, and the mirror;
//   • Go (desktop/uitheme.go) writes the same number into desktop_ui.json, which is what makes it
//     inspectable, hand-editable, and the recovery path when this webview's storage is gone.

import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'

// Mirrored by the boot script in index.html and by readingScaleMin/MaxPercent in
// desktop/uitheme.go — all three have to stay identical, and only the Go side can reject a value
// that never went through these steps.
const STORAGE_KEY = 'tachi.desktop.fontScale'
const MIN_PERCENT = 80
const MAX_PERCENT = 160

/** One press of ⌘= or ⌘-. Ten percent is eight steps across the whole range — enough to feel at
 *  the default's 15.5px, small enough to land on the size the reader was after. */
const STEP_PERCENT = 10

/** The size the stylesheets declare (--fs-scale: 1), and what ⌘0 returns to. */
const DEFAULT_PERCENT = 100

function clampPercent(pct: number): number {
  return Math.max(MIN_PERCENT, Math.min(MAX_PERCENT, pct))
}

/** The step this webview stored, or null when it has none — a first launch, or storage blocked. */
export function storedFontScalePercent(): number | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return null
    const pct = Number.parseInt(raw, 10)
    return Number.isFinite(pct) && pct >= MIN_PERCENT && pct <= MAX_PERCENT ? pct : null
  } catch {
    return null // storage can be blocked; the module still zooms for this session
  }
}

/** What is on screen right now, in percent — read off the applied variable, so the boot script's
 *  value and everything after it are the same fact. */
export function currentFontScalePercent(): number {
  const applied = Number.parseFloat(document.documentElement.style.getPropertyValue('--fs-scale'))
  return Number.isFinite(applied) && applied > 0 ? Math.round(applied * 100) : DEFAULT_PERCENT
}

// apply paints the scale, and remembers it where the two stores can find it again: localStorage
// (the next launch's boot script, synchronously) and desktop_ui.json through Go. Best effort on
// both — a failure costs the next launch the step, never this one.
function apply(percent: number, remember: boolean) {
  document.documentElement.style.setProperty('--fs-scale', String(percent / 100))
  if (remember) {
    try {
      localStorage.setItem(STORAGE_KEY, String(percent))
    } catch {
      /* storage blocked: this session still resizes, it just will not persist */
    }
  }
  AgentService.SetReadingFontScale(percent).catch(() => {})
}

/** One press of ⌘= (steps > 0) or ⌘- (steps < 0). Stops at the bounds, and does nothing there —
 *  a no-op repaint of an unchanged value would only write the file again. */
export function bumpFontScale(steps: number): void {
  const now = currentFontScalePercent()
  const next = clampPercent(now + steps * STEP_PERCENT)
  if (next !== now) apply(next, true)
}

/** ⌘0 — back to the size the stylesheets declare. 100 is stored like any other step, so the file
 *  says what is on screen rather than "the reader once changed this". */
export function resetFontScale(): void {
  if (currentFontScalePercent() !== DEFAULT_PERCENT) apply(DEFAULT_PERCENT, true)
}

// reconcileFontScale settles the two stores once, at mount. There are two of them on purpose (one
// is what the boot frame reads, the other is the record Go keeps), so a mismatch means one was
// cleared or edited by hand:
//   • a step in this webview → it is what the reader last chose, and having the boot script paint
//     it is exactly the point; mirroring it here is also what heals a write a crash cut short;
//   • nothing here but a step in the file → this webview's storage is gone (or never existed) and
//     the file is the only thing that still remembers the choice.
export function reconcileFontScale(): void {
  const stored = storedFontScalePercent()
  if (stored !== null) {
    apply(stored, false)
    return
  }
  AgentService.GetUIState()
    .then((st) => {
      const pct = st?.readingFontScale ?? 0
      if (pct >= MIN_PERCENT && pct <= MAX_PERCENT) apply(pct, true)
    })
    .catch(() => { /* an unreadable preference just means the default size */ })
}
