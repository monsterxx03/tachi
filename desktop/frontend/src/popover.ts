// Where a `.popover-panel` goes.
//
// The panels used to be placed by one CSS rule — absolute, `right: -6px`, `bottom: 100% + 10px` —
// which let them grow only upward and never MEASURED anything. That is fine while the window is
// tall, and wrong the moment it is not: the MCP panel is up to 560px tall and hangs off the
// composer's status row, so a short window cut its top off with nothing to report it. A trigger
// near the right edge had the same shape of problem in the other direction.
//
// What this adds, in floating-ui's terms: `offset` (the 10px gap the CSS had), `flip` (open
// downward when there is no room above), `shift` (slide back inside the window when the panel
// would cross an edge) and `size` (clamp the panel's height to the room there IS — the panel's own
// scrolling body then does the rest).
//
// Deliberately still `absolute` and `transform: false`:
//   • absolute keeps the panel inside the sidebar/composer stacking context it was designed in
//     (a `fixed` panel would escape the transforms-free assumption and start overlapping the
//     titlebar independently of what the app paints there);
//   • no transform means the coordinates are written as top/left, so the panel's own `panel-in`
//     animation (which animates transform) still runs. floating-ui's usual recommendation is a
//     transform, and it would silently win over that keyframe.

import { autoUpdate, flip, offset, shift, size, useFloating } from '@floating-ui/react'
import { useCallback, type CSSProperties } from 'react'

/** The gap the CSS had (`bottom: calc(100% + 10px)`), and the sideways nudge that lined a panel
 *  up with its trigger's right edge (`right: -6px`). */
const GAP = 10
const EDGE_NUDGE = 6
/** Room kept between a panel and the window edge when it flips or slides. */
const VIEWPORT_PADDING = 8

export type PopoverPosition = {
  /** Put on the `.popover-anchor` wrapper — the element the panel hangs off. */
  anchorRef: (el: HTMLElement | null) => void
  /** Put on the panel itself (the `.popover-panel` element). */
  panelRef: (el: HTMLElement | null) => void
  style: CSSProperties
  /** True while the click target is inside the panel — the dismissal handlers use it instead of
   *  keeping a second ref to the same element. */
  isInside: (t: Node) => boolean
}

/**
 * usePopoverPosition positions one popover. `open` is only a hint for floating-ui's own bookkeeping
 * (the panel is unmounted when closed, so there is nothing to place); callers may pass `true`.
 */
export function usePopoverPosition(open = true): PopoverPosition {
  const { refs, floatingStyles } = useFloating({
    open,
    placement: 'top-end',
    strategy: 'absolute',
    transform: false,
    middleware: [
      offset({ mainAxis: GAP, crossAxis: EDGE_NUDGE }),
      flip({ padding: VIEWPORT_PADDING }),
      shift({ padding: VIEWPORT_PADDING }),
      size({
        padding: VIEWPORT_PADDING,
        apply({ availableHeight, elements }) {
          // A CSS variable rather than `style.maxHeight`: the panel classes carry their own
          // ceiling (`.mcp-panel` is `min(70vh, 560px)`), and an inline max-height would overwrite
          // that ceiling even when the window is tall. The class keeps its limit; this only ever
          // tightens it.
          elements.floating.style.setProperty('--popover-max-h', `${Math.round(availableHeight)}px`)
        },
      }),
    ],
    whileElementsMounted: autoUpdate,
  })

  const isInside = useCallback(
    (t: Node) => !!refs.floating.current?.contains(t),
    [refs],
  )

  return {
    anchorRef: refs.setReference,
    panelRef: refs.setFloating,
    isInside,
    // right/bottom are what the old rule set; a top/left from floating-ui alongside them would
    // stretch the panel between two anchors instead of placing it.
    style: { ...floatingStyles, right: 'auto', bottom: 'auto' },
  }
}
