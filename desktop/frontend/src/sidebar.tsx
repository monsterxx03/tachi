// The session sidebar's width: the reader drags its right edge, and the choice survives a restart.
//
// It is the same bargain the side-channel panel's width strikes (oneoff.tsx), for the same reasons —
// read that file's block first: the width the reader CHOSE is what gets stored, what is SHOWN is that
// number clamped to the room there is, and the number under the pointer during a drag is written
// straight to the element rather than into React state (a state update per pointermove laid the whole
// chat out again before the browser had even applied the new width — 「拖动时很卡」).
//
// Three things are the sidebar's own:
//   • the room to subtract is what the CONVERSATION keeps (CHAT_MIN_WIDTH / CHAT_MIN_SHARE), and the
//     sidebar is the column that gives way, not the one that takes;
//   • the width has a CSS transition (folding and unfolding animate), so a drag has to turn it off —
//     a transitioned width lags the pointer by its duration — which is what `.is-resizing` is for;
//   • a COLLAPSED sidebar has no width at all (the CSS owns that state), so the hook hands back
//     `null` and App leaves the element's style alone.

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'

/** The default, matching `.sidebar` in layout.css: a restart restores the reader's width, and this is
 *  what a reader who never dragged one gets. */
const SIDEBAR_DEFAULT_WIDTH = 280
/** Where the rows stop being readable (the floor) and a sanity ceiling for the STORED value — the
 *  same two numbers the backend refuses a hand-edited file against (desktop/uitheme.go). */
const SIDEBAR_MIN_WIDTH = 200
const SIDEBAR_MAX_WIDTH = 600
/** The conversation's own floor and share (oneoff.tsx's, verbatim): a bare floor pins the chat AT the
 *  floor and hands every extra pixel to the column above it. */
const CHAT_MIN_WIDTH = 560
const CHAT_MIN_SHARE = 0.5
/** One arrow-key press on the handle (a drag is not the only way to resize). */
const SIDEBAR_KEY_STEP = 24

/** The widest the sidebar may be SHOWN: the window less what the conversation keeps. */
function sidebarRoom(): number {
  if (typeof window === 'undefined') return SIDEBAR_MAX_WIDTH
  const keep = Math.max(CHAT_MIN_WIDTH, Math.round(window.innerWidth * CHAT_MIN_SHARE))
  return Math.max(SIDEBAR_MIN_WIDTH, Math.min(SIDEBAR_MAX_WIDTH, window.innerWidth - keep))
}

/** clampSidebarWidth limits a requested width to the room there is. DISPLAY-ONLY: what gets stored
 *  stays the width the reader chose, so a narrow window limits what is shown, not what is kept. */
function clampSidebarWidth(chosen: number, room: number): number {
  return Math.max(SIDEBAR_MIN_WIDTH, Math.min(room, chosen))
}

export type SidebarWidth = {
  /** Attach to the element the width is written to (App's `<aside>`). The drag writes through it
   *  instead of through state (see the header). */
  ref: React.RefObject<HTMLElement | null>
  /** What the element should be shown at, or null when the CSS owns it (a collapsed sidebar). */
  width: number | null
  /** True while a drag is in flight — the element's transition has to be off for its duration. */
  resizing: boolean
  /** The grab strip's own wiring, spread onto the element App renders inside the sidebar. */
  handleProps: {
    onPointerDown: (e: React.PointerEvent<HTMLElement>) => void
    onKeyDown: (e: React.KeyboardEvent<HTMLElement>) => void
    role: 'separator'
    'aria-orientation': 'vertical'
    'aria-label': string
    tabIndex: 0
    title: string
  }
}

/** useSidebarWidth owns the session list's width end to end: the stored value (read once at mount,
 *  written when a gesture ends), the room there is, and the drag itself. */
export function useSidebarWidth(collapsed: boolean): SidebarWidth {
  const [width, setWidth] = useState(SIDEBAR_DEFAULT_WIDTH)
  const [room, setRoom] = useState(() => sidebarRoom())
  // The element the drag writes to, so a pointermove does not go through React (see the header).
  const elRef = useRef<HTMLElement | null>(null)
  const dragPxRef = useRef<number | null>(null)
  const [resizing, setResizing] = useState(false)

  // Read once at mount: the render is driven by the state above, the file is only what it is
  // restored FROM. 0 = never dragged.
  useEffect(() => {
    AgentService.GetUIState()
      .then((st) => { if (st?.sidebarWidth) setWidth(st.sidebarWidth) })
      .catch(() => { /* an unreadable preference just means the default */ })
  }, [])

  // The room moves with the WINDOW and with nothing else: the sidebar is the first column, so no
  // sibling can hand it room the way folding this one hands room to the panel.
  useEffect(() => {
    const measure = () => setRoom(sidebarRoom())
    window.addEventListener('resize', measure)
    return () => window.removeEventListener('resize', measure)
  }, [])

  // A render that happens mid-drag (a streamed delta, a session list refresh, a resize re-measuring
  // the room) must not put React's older number back on screen: the drag owns the element until it
  // ends, and re-asserts its own value after every commit.
  useLayoutEffect(() => {
    if (dragPxRef.current !== null && elRef.current) {
      elRef.current.style.width = `${dragPxRef.current}px`
      elRef.current.style.minWidth = `${dragPxRef.current}px`
    }
  })

  const commit = useCallback((px: number) => {
    setWidth(px)
    // Best effort, like every other uiState write: a failure costs the next launch the width.
    AgentService.SetSidebarWidth(px).catch(() => {})
  }, [])

  const startResize = useCallback((e: React.PointerEvent<HTMLElement>) => {
    const el = elRef.current
    if (!el) return
    e.preventDefault()
    // No setPointerCapture: the moves are listened for on the WINDOW (a pointer that leaves the 6px
    // strip must keep resizing), which is also what makes a synthetic pointer work — capture refuses
    // a pointer it never saw go down. Same shape as the panel's handle.
    const startX = e.clientX
    const startW = el.getBoundingClientRect().width
    dragPxRef.current = startW
    setResizing(true)
    // …and turn the transition off HERE rather than through the class React will add: the class
    // lands on the next commit, and a pointermove that arrives before it would be animated — the
    // width lagging the pointer is exactly what the drag must not do. Cleared on release, where the
    // class (and the CSS) take over again.
    el.style.transition = 'none'
    const move = (ev: PointerEvent) => {
      // Dragging RIGHT widens the sidebar (it is the first column).
      const next = clampSidebarWidth(Math.round(startW + (ev.clientX - startX)), room)
      dragPxRef.current = next
      el.style.width = `${next}px`
      el.style.minWidth = `${next}px`
    }
    const up = () => {
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', up)
      window.removeEventListener('pointercancel', up)
      setResizing(false)
      document.body.classList.remove('is-resizing')
      const px = dragPxRef.current
      dragPxRef.current = null
      // Back to React's ownership: the inline width is re-rendered from the committed state, which
      // is the same number (or the clamped one, if the window changed under the drag).
      el.style.width = ''
      el.style.minWidth = ''
      el.style.transition = ''
      if (px !== null) commit(px)
    }
    // While a drag is in flight the pointer is over the transcript: no text selection, and the
    // col-resize cursor stays even where the strip is not under the pointer (body.is-resizing).
    document.body.classList.add('is-resizing')
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', up)
    window.addEventListener('pointercancel', up)
  }, [room, commit])

  const onResizeKey = useCallback((e: React.KeyboardEvent<HTMLElement>) => {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return
    e.preventDefault()
    const now = clampSidebarWidth(width, room)
    commit(clampSidebarWidth(now + (e.key === 'ArrowRight' ? SIDEBAR_KEY_STEP : -SIDEBAR_KEY_STEP), room))
  }, [width, room, commit])

  return {
    ref: elRef,
    width: collapsed ? null : clampSidebarWidth(width, room),
    resizing,
    handleProps: {
      onPointerDown: startResize,
      onKeyDown: onResizeKey,
      role: 'separator',
      'aria-orientation': 'vertical',
      'aria-label': '调整会话列表宽度',
      tabIndex: 0,
      title: '拖动调整会话列表宽度（← → 也可以）',
    },
  }
}
