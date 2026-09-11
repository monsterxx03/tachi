// The preview lightbox: one full-viewport surface behind every "show me this
// bigger" — mermaid diagrams in a reply, attachment images, attachment
// documents. A shared shell (dimmed backdrop, a centred stage, a floating
// control pill) and one set of zoom/pan interactions, so all of them behave the
// same way.
//
// It renders through a portal into document.body: inside the transcript some
// ancestor establishes a containing block for `position: fixed` (a transform or
// a filter), which would otherwise pin the overlay to a message box.

import { useCallback, useEffect, useRef, useState, type ComponentPropsWithoutRef, type ReactNode, type RefObject } from 'react'
import { createPortal } from 'react-dom'

// Zoom bounds (0.2x … 6x covers "放大看细节" and "整体扫一眼" without letting the
// content become unusable).
export const VIEWER_MIN_ZOOM = 0.2
export const VIEWER_MAX_ZOOM = 6
export const VIEWER_ZOOM_STEP = 1.25

export function clampZoom(z: number): number {
  return Math.min(VIEWER_MAX_ZOOM, Math.max(VIEWER_MIN_ZOOM, z))
}

// useDragPan turns a scroll container into a grab-and-drag surface. While the
// content is zoomed, dragging is what people reach for (the scrollbar is thin and
// the pointer is already on the content), so panning moves the container's scroll
// offsets under the cursor.
export function useDragPan(ref: RefObject<HTMLElement | null>) {
  const [dragging, setDragging] = useState(false)
  const [pannable, setPannable] = useState(false)
  const origin = useRef({ x: 0, y: 0, left: 0, top: 0 })

  const sync = useCallback(() => {
    const el = ref.current
    if (!el) return
    setPannable(el.scrollWidth > el.clientWidth + 1 || el.scrollHeight > el.clientHeight + 1)
  }, [ref])

  const onPointerDown = (e: React.PointerEvent) => {
    const el = ref.current
    if (!el || e.button !== 0) return
    if (el.scrollWidth <= el.clientWidth && el.scrollHeight <= el.clientHeight) return // nothing to pan
    e.preventDefault()
    origin.current = { x: e.clientX, y: e.clientY, left: el.scrollLeft, top: el.scrollTop }
    setDragging(true)
    el.setPointerCapture(e.pointerId)
  }
  const onPointerMove = (e: React.PointerEvent) => {
    const el = ref.current
    if (!el || !dragging) return
    el.scrollLeft = origin.current.left - (e.clientX - origin.current.x)
    el.scrollTop = origin.current.top - (e.clientY - origin.current.y)
  }
  const end = (e: React.PointerEvent) => {
    const el = ref.current
    if (!el || !dragging) return
    setDragging(false)
    try { el.releasePointerCapture(e.pointerId) } catch { /* already released */ }
  }

  return {
    dragging,
    pannable,
    sync,
    handlers: { onPointerDown, onPointerMove, onPointerUp: end, onPointerCancel: end },
  }
}

// useZoomPan is the zoom half of the lightbox: it scales the stage's single
// scalable child and wires wheel, buttons and drag together.
//
// fit() measures that child at zoom 1 (its own layout size) and scales it to the
// stage — one implementation for a diagram, a screenshot or any other block that
// should open "as large as it fits" rather than at an arbitrary 100%.
export function useZoomPan(stageRef: RefObject<HTMLElement | null>) {
  const [zoom, setZoom] = useState(1)
  const pan = useDragPan(stageRef)

  const fit = useCallback(() => {
    const stage = stageRef.current
    const el = stage?.firstElementChild as HTMLElement | null
    if (!stage || !el) return
    setZoom(1)
    // Measure once the browser has applied zoom 1, or the rect is still the
    // zoomed one.
    requestAnimationFrame(() => {
      const r = el.getBoundingClientRect()
      if (!r.width || !r.height || !stage.clientWidth || !stage.clientHeight) return
      setZoom(clampZoom(Math.min((stage.clientWidth - VIEWER_FIT_MARGIN) / r.width, (stage.clientHeight - VIEWER_FIT_MARGIN) / r.height)))
    })
  }, [stageRef])

  const zoomBy = useCallback((factor: number) => setZoom((z) => clampZoom(z * factor)), [])
  const reset = useCallback(() => setZoom(1), [])

  // Zooming with ctrl/⌘+wheel only: a plain wheel is how the stage scrolls.
  const onWheel = useCallback((e: React.WheelEvent) => {
    if (!e.ctrlKey && !e.metaKey) return
    e.preventDefault()
    zoomBy(e.deltaY < 0 ? VIEWER_ZOOM_STEP : 1 / VIEWER_ZOOM_STEP)
  }, [zoomBy])

  // Re-check scrollability whenever the zoom changes: the "grab" cursor should
  // only promise something the user can actually do.
  useEffect(() => { pan.sync() }, [zoom, pan])

  return { zoom, pan, fit, zoomBy, reset, onWheel, clamp: clampZoom }
}

// VIEWER_FIT_MARGIN is the breathing room fit() leaves around the content, so a
// diagram fitted to the window is not glued to the backdrop.
const VIEWER_FIT_MARGIN = 40

// useViewerZoomKeys binds the lightbox zoom keys: +/−, 0 for 1:1 and 1 to fit.
// Registered in the capture phase and stopped there, so a viewer sitting over the
// composer keeps those keys from reaching the composer. Esc is NOT here: it closes
// from the shell (ViewerOverlay), so every viewer has it whether or not it zooms.
export function useViewerZoomKeys({ fit, reset, zoomBy }: {
  fit: () => void
  reset: () => void
  zoomBy: (factor: number) => void
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === '+' || e.key === '=') { e.preventDefault(); zoomBy(VIEWER_ZOOM_STEP) }
      else if (e.key === '-' || e.key === '_') { e.preventDefault(); zoomBy(1 / VIEWER_ZOOM_STEP) }
      else if (e.key === '0') { e.preventDefault(); reset() }
      else if (e.key === '1') { e.preventDefault(); fit() }
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [fit, reset, zoomBy])
}

// ViewerOverlay is the shell: backdrop, stage, control pill — and the dismissal
// contract every viewer shares: Esc, or a click on empty space.
//
// "Empty space" is judged by the click TARGET, not its position: a click that lands
// on the stage element itself (the dim margin around the content) closes, while one
// that lands on a wrapper inside it (a page, a table, a diagram) is a click on the
// content and must not — those are how a document is read and its text selected. The
// shell also closes on the backdrop outside the stage (the overlay's own padding).
//
// A click only counts when it IS one: dragging to pan a zoomed image ends on the
// stage and would otherwise read as a click on empty space, dismissing the very
// viewer the user was dragging. Any movement with a button held marks the gesture as
// a drag, and the click it produces is ignored.
//
// Esc is handled here rather than per viewer for the same reason: a viewer that
// forgot to bind it would silently keep a modal surface open (which is exactly what
// happened to the document viewers). It runs in the capture phase and stops there,
// so a viewer over the composer cannot have its Esc swallowed by, or leak into, the
// composer's own Esc handling. One case it cannot cover: once focus is inside the
// previewed iframe (HTML, PDF), its keys belong to the iframe's document and never
// reach this handler — the ✕ is always there for that.
export function ViewerOverlay({ label, stageRef, stageClass, stageProps, controls, onClose, children }: {
  label: string
  stageRef?: RefObject<HTMLDivElement>
  stageClass?: string
  stageProps?: ComponentPropsWithoutRef<'div'>
  controls: ReactNode
  onClose: () => void
  children: ReactNode
}) {
  const dragged = useRef(false)

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.preventDefault()
      e.stopPropagation()
      onClose()
    }
    // Window-level (capture) so the gesture is tracked wherever the pointer goes,
    // including outside the stage and inside a pan handler that stops propagation.
    const onPointerDown = () => { dragged.current = false }
    const onPointerMove = (e: PointerEvent) => { if (e.buttons !== 0) dragged.current = true }
    window.addEventListener('keydown', onKey, true)
    window.addEventListener('pointerdown', onPointerDown, true)
    window.addEventListener('pointermove', onPointerMove, true)
    return () => {
      window.removeEventListener('keydown', onKey, true)
      window.removeEventListener('pointerdown', onPointerDown, true)
      window.removeEventListener('pointermove', onPointerMove, true)
    }
  }, [onClose])

  return createPortal(
    <div className="viewer-overlay" onClick={onClose} role="dialog" aria-modal="true" aria-label={label}>
      <div ref={stageRef} className={`viewer-stage${stageClass ? ' ' + stageClass : ''}`}
        {...stageProps}
        onClick={(e) => {
          if (dragged.current) { e.stopPropagation(); return }
          if (e.target === e.currentTarget) { onClose(); return }
          e.stopPropagation()
        }}>
        {children}
      </div>
      <div className="viewer-controls" onClick={(e) => e.stopPropagation()}>{controls}</div>
    </div>,
    document.body,
  )
}

// ZoomControls is the shared zoom cluster (minus a viewer's own extras): minus,
// the current percentage, plus — then 1:1 and fit.
export function ZoomControls({ zoom, zoomBy, reset, fit }: {
  zoom: number
  zoomBy: (factor: number) => void
  reset: () => void
  fit: () => void
}) {
  return (
    <>
      <button className="viewer-btn" title="缩小（-）" onClick={() => zoomBy(1 / VIEWER_ZOOM_STEP)}>−</button>
      <span className="viewer-zoom-label">{Math.round(zoom * 100)}%</span>
      <button className="viewer-btn" title="放大（+）" onClick={() => zoomBy(VIEWER_ZOOM_STEP)}>+</button>
      <span className="viewer-sep" />
      <button className="viewer-btn" title="实际大小（0）" onClick={reset}>100%</button>
      <button className="viewer-btn" title="适应窗口（1）" onClick={fit}>适应窗口</button>
    </>
  )
}

// CloseButton is the pill's last button in every viewer.
export function CloseButton({ onClose }: { onClose: () => void }) {
  return <button className="viewer-btn" title="关闭（Esc）" onClick={onClose}>✕</button>
}

// ImageViewer shows an attachment image full-screen. Same zoom/pan surface as a
// diagram, minus the white panel: a screenshot has its own edges and should not
// get a second frame drawn around it.
export function ImageViewer({ src, alt, onClose }: { src: string; alt: string; onClose: () => void }) {
  const stageRef = useRef<HTMLDivElement>(null)
  const { zoom, pan, fit, zoomBy, reset, onWheel } = useZoomPan(stageRef)
  useViewerZoomKeys({ fit, reset, zoomBy })
  // fit() once the image has a size: on mount it may still be loading, and then
  // the rect it would measure is empty.
  useEffect(() => { fit() }, [fit])

  return (
    <ViewerOverlay label={alt || '图片预览'} stageRef={stageRef} onClose={onClose}
      stageClass={`${pan.dragging ? 'is-dragging' : ''}${pan.pannable ? ' is-pannable' : ''}`.trim()}
      stageProps={{ ...pan.handlers, onWheel }}
      controls={<>
        <ZoomControls zoom={zoom} zoomBy={zoomBy} reset={reset} fit={fit} />
        <CloseButton onClose={onClose} />
      </>}>
      <div className="viewer-zoom" style={{ zoom }}>
        <img className="viewer-image" src={src} alt={alt} draggable={false} onLoad={fit} />
      </div>
    </ViewerOverlay>
  )
}
