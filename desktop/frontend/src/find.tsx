// ⌘F inside a SURFACE: one bar, one highlight pass, and one place that decides who owns the key.
//
// The surfaces that host it are the ones showing a long document the reader did not type — a
// diff, a previewed file, a review's report. Each host searches ITS OWN subtree rather than the
// page: ⌘F in a preview must not scroll the transcript behind it, and the side panel shows one
// pane at a time (过程 / 意见+diff / 报告), so a single host there covers all three.
//
// Matches are painted with the CSS Custom Highlight API (`::highlight()`), not by wrapping text
// in <mark>: these panes are React trees that re-render while the reader looks at them (a live
// run's record is re-read every second, a report fills in when it lands), so a DOM mutation
// would be thrown away by the next render — or become the thing that render diffs against. A
// highlight is a Range, so React never sees it; a re-render only invalidates the ranges, and the
// MutationObserver below collects them again.
//
// Key ownership follows the app's Esc contract (`defaultPrevented` as the handshake), with one
// addition. A find bar is the INNERMOST surface that can want these keys, but it may mount after
// the overlay it sits inside — and among capture-phase listeners on the same target, the one
// added FIRST runs first. So the owner is installed once, at module load, before any component's
// effect; a key it consumes is stopped with stopImmediatePropagation, which is what lets Esc
// close the bar instead of the overlay behind it.

import { useCallback, useEffect, useMemo, useRef, useState, type RefObject } from 'react'

// The two highlight names. They are registered on `CSS.highlights` (a process-wide registry keyed
// by name), so the CSS that paints them lives in base.css next to the colour tokens it reads.
const HL_ALL = 'tachi-find'
const HL_CURRENT = 'tachi-find-current'

// The bar — and everything inside it — is the one part of a searched surface that must NOT match:
// it holds the query in its own text nodes, and a hit on the word being typed is noise.
const BAR_SELECTOR = '.find-bar'

// findRanges is one Range per occurrence of `query` under `root`, in document order. The match is
// case-insensitive and stays within a single text node: a hit spanning inline markup (half a word
// inside a <code> span) is missed, the trade every find UI without a text extractor makes. Worth
// revisiting only if a real document needs it — the panes here are prose and code lines, where a
// query lands inside one node.
function findRanges(root: HTMLElement, query: string): Range[] {
  const needle = query.toLowerCase()
  if (!needle) return []
  const out: Range[] = []
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT)
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    const text = node as Text
    if (!text.data || !text.parentElement || text.parentElement.closest(BAR_SELECTOR)) continue
    const hay = text.data.toLowerCase()
    let at = hay.indexOf(needle)
    while (at >= 0) {
      const range = document.createRange()
      range.setStart(text, at)
      range.setEnd(text, at + needle.length)
      out.push(range)
      at = hay.indexOf(needle, at + needle.length)
    }
  }
  return out
}

// scrollRangeIntoView brings one hit to the middle of its surface's scroller (the panel's body,
// a viewer's stage) — never the window.
function scrollRangeIntoView(range: Range) {
  range.startContainer.parentElement?.scrollIntoView({ block: 'center', inline: 'nearest' })
}

// The hosts, innermost LAST: a surface that mounts later sits ON TOP of one already there (a
// preview over the panel), and the top one owns ⌘F. Closing an overlay hands the key back.
type FindHandle = {
  // claimed reports whether this host's bar is showing, so Esc goes to it rather than past it.
  claimed: () => boolean
  close: () => void
  // call opens the bar — or re-focuses and selects the field when it is already up, which is what
  // pressing ⌘F twice does natively.
  call: () => void
}

const hosts: FindHandle[] = []

if (typeof window !== 'undefined') {
  window.addEventListener('keydown', (e) => {
    const top = hosts[hosts.length - 1]
    if (!top) return
    if (e.key === 'Escape') {
      if (!top.claimed()) return
      e.preventDefault()
      e.stopImmediatePropagation() // the overlay behind the bar must not also close on this key
      top.close()
      return
    }
    if (e.metaKey && !e.altKey && !e.ctrlKey && e.key.toLowerCase() === 'f') {
      e.preventDefault()
      top.call()
    }
  }, true)
}

// Find is what a host renders its bar with; useFind owns one surface's search.
export type Find = {
  open: boolean
  query: string
  // count is how many occurrences the CURRENT content holds; index is the one the reader is on
  // (0-based). A non-empty query with count === 0 is 没有匹配, which the bar says in words.
  count: number
  index: number
  barRef: RefObject<HTMLDivElement>
  inputRef: RefObject<HTMLInputElement>
  setQuery: (q: string) => void
  openBar: () => void
  next: () => void
  prev: () => void
  close: () => void
}

export function useFind(rootRef: RefObject<HTMLElement | null> | null): Find {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [ranges, setRanges] = useState<Range[]>([])
  const [index, setIndex] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)
  const barRef = useRef<HTMLDivElement>(null)
  // The same ranges for the callbacks, so `step` can walk the current list without being
  // re-created (and re-registered) on every keystroke.
  const rangesRef = useRef<Range[]>([])
  rangesRef.current = ranges

  // The query changed (or the bar opened): collect, start at the first hit, and show it. A
  // reader who types expects to be taken to what they typed.
  useEffect(() => {
    const root = rootRef?.current
    if (!root || !open || !query) {
      setRanges([])
      setIndex(0)
      return
    }
    const next = findRanges(root, query)
    setRanges(next)
    setIndex(0)
    if (next.length) window.requestAnimationFrame(() => scrollRangeIntoView(next[0]))
  }, [rootRef, open, query])

  // The CONTENT changed under a query that is already typed (a re-render, a report landing, a
  // diff arriving): collect again and keep the reader's place. Debounced by a short timer rather
  // than run per mutation — a live run rewrites its record every second and a streaming reply
  // mutates on every token, and a full text walk per token is work nobody asked for. No reveal
  // here on purpose: scrolling the pane out from under a reader who is reading would be worse
  // than a count that lags a moment.
  useEffect(() => {
    const root = rootRef?.current
    if (!root || !open || !query) return
    let timer = 0
    const collect = () => {
      const next = findRanges(root, query)
      setRanges(next)
      setIndex((i) => (i < next.length ? i : 0))
    }
    const observer = new MutationObserver(() => {
      window.clearTimeout(timer)
      timer = window.setTimeout(collect, 120)
    })
    observer.observe(root, { childList: true, subtree: true, characterData: true })
    return () => {
      window.clearTimeout(timer)
      observer.disconnect()
    }
  }, [rootRef, open, query])

  // Paint the hits. The current one is a second highlight: the registry paints in registration
  // order, so the later name wins wherever the ranges overlap.
  useEffect(() => {
    if (typeof CSS === 'undefined' || !CSS.highlights) return
    if (!open || !ranges.length) {
      CSS.highlights.delete(HL_ALL)
      CSS.highlights.delete(HL_CURRENT)
      return
    }
    CSS.highlights.set(HL_ALL, new Highlight(...ranges))
    const current = ranges[index]
    if (current) CSS.highlights.set(HL_CURRENT, new Highlight(current))
    else CSS.highlights.delete(HL_CURRENT)
    return () => {
      CSS.highlights.delete(HL_ALL)
      CSS.highlights.delete(HL_CURRENT)
    }
  }, [open, ranges, index])

  const step = useCallback((delta: number) => {
    const total = rangesRef.current.length
    if (!total) return
    setIndex((i) => {
      const next = ((i + delta) % total + total) % total
      const range = rangesRef.current[next]
      if (range) window.requestAnimationFrame(() => scrollRangeIntoView(range))
      return next
    })
  }, [])

  const setQueryAndJump = useCallback((q: string) => {
    setQuery(q)
    setIndex(0)
  }, [])

  const openBar = useCallback(() => {
    setOpen(true)
    // Focus after the field exists: this runs from a key handler or a button click, before React
    // has rendered the bar.
    window.requestAnimationFrame(() => {
      inputRef.current?.focus()
      inputRef.current?.select()
    })
  }, [])

  const close = useCallback(() => setOpen(false), [])

  // The module-level owner's view of this host. Refs + a stable identity: that owner outlives
  // every render and must always see the current bar. A host with no searchable root (an image
  // or PDF viewer) still OWNS the key — it is the surface on top, and letting ⌘F through would
  // open the bar of the panel dimmed behind it — it just has nothing to do with it.
  const handle = useMemo<FindHandle>(() => ({
    claimed: () => !!barRef.current,
    close,
    // Always through openBar: it shows the bar (a no-op when it is already up) and focuses the
    // field with its text selected, which is what pressing ⌘F twice does natively. Branching on
    // "the field already exists" is the trap here — a bar that is MOUNTED but closed would look
    // open to this handle, and the state behind it would never start collecting.
    call: () => {
      if (!rootRef) return
      openBar()
    },
  }), [close, openBar, rootRef])

  useEffect(() => {
    hosts.push(handle)
    return () => {
      const at = hosts.indexOf(handle)
      if (at >= 0) hosts.splice(at, 1)
    }
  }, [handle])

  return {
    open, query, count: ranges.length, index, barRef, inputRef,
    setQuery: setQueryAndJump, openBar, next: () => step(1), prev: () => step(-1), close,
  }
}

// FindBar is the bar: the field, where the reader is among the matches, and the two walks. A host
// renders it where the bar belongs on ITS surface (sticky inside the panel's scroller, floating
// over a viewer's stage) — the class it carries is the only difference.
export function FindBar({ find, className }: { find: Find; className?: string }) {
  const empty = !!find.query && find.count === 0
  return (
    <div ref={find.barRef} className={`find-bar${className ? ' ' + className : ''}`} role="search">
      <input ref={find.inputRef} className="find-input" value={find.query} placeholder="查找…"
        spellCheck={false} autoComplete="off" autoCorrect="off" aria-label="在本页查找"
        onChange={(e) => find.setQuery(e.target.value)}
        onKeyDown={(e) => {
          if (e.key !== 'Enter') return // Esc is consumed by the module-level owner, never reaching here
          e.preventDefault()
          if (e.shiftKey) find.prev()
          else find.next()
        }} />
      <span className={`find-count${empty ? ' is-empty' : ''}`} aria-live="polite">
        {find.query ? (find.count ? `${find.index + 1}/${find.count}` : '无匹配') : ''}
      </span>
      <button type="button" className="find-btn" title="上一个（Shift+Enter）"
        onClick={find.prev} disabled={!find.count}>↑</button>
      <button type="button" className="find-btn" title="下一个（Enter）"
        onClick={find.next} disabled={!find.count}>↓</button>
      <button type="button" className="find-btn" title="关闭（Esc）" onClick={find.close}>✕</button>
    </div>
  )
}

// FindButton is the discoverability half: ⌘F is invisible, so every surface that can be searched
// also offers the same gesture as a button — and it goes through the same entry point, so a click
// and the shortcut cannot drift apart.
export function FindButton({ find }: { find: Find }) {
  return (
    <button type="button" className="find-open" title="查找（⌘F）" onClick={find.openBar}>⌕</button>
  )
}
