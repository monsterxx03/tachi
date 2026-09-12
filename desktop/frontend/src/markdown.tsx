// Markdown rendering: one pipeline, shared by the transcript's assistant replies
// and by the attachment preview (a .md the agent sent, a source file shown as
// highlighted code). Keeping it in one module is what makes "the same file looks
// the same in the chat and in the card" true by construction.

import { memo, useCallback, useEffect, useMemo, useRef, useState, type ComponentPropsWithoutRef, type ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
import { CopyIcon } from './components'
import { copyText, toLocalAsset } from './lib'
import { useThemeSnapshot } from './theme'
import { CloseButton, ViewerOverlay, ZoomControls, useViewerZoomKeys, useZoomPan } from './viewer'

// TableScroller wraps GFM tables in a horizontally scrollable container so a
// table wider than the message card scrolls inside it instead of bursting out
// of the layout. Module-level: stable identity across streaming re-renders.
function TableScroller(props: { children?: ReactNode }) {
  return (
    <div className="table-scroll">
      <table>{props.children}</table>
    </div>
  )
}

// MarkdownBlock renders one markdown document. Memoized on (text, workDir),
// which is what keeps a streaming turn from re-parsing every earlier reply on
// each animation frame.
export const MarkdownBlock = memo(function MarkdownBlock({ text, workDir }: { text: string; workDir: string }) {
  const MdImg = useCallback(({ src, alt }: { src?: string; alt?: string }) => (
    <img src={toLocalAsset(src, workDir)} alt={alt || ''} />
  ), [workDir])
  return (
    <div className="assistant-text">
      {/* rehypeHighlight tokenises fenced code; PreBlock turns ```mermaid into a
          diagram and leaves everything else as a plain <pre>. */}
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[rehypeHighlight]}
        components={{ img: MdImg, table: TableScroller, pre: PreBlock }}>
        {text}
      </ReactMarkdown>
    </div>
  )
})

// ── Inline markdown ─────────────────────────────────────────────────────────
// InlineMd renders the inline markdown ONE PIECE OF PROSE is written in: `code`, **bold**,
// *emphasis*, and [label](url). Anything else stays literal text — and that is the point:
// the grammar below IS the contract, so what a finding shows can never drift from what it
// says.
//
// Why not MarkdownBlock: that one mounts react-markdown with remark-gfm + rehype-highlight
// and wraps its output in `.assistant-text` (document typography, block margins). The
// review panel renders 30+ findings at once, each a sentence — instantiating that pipeline
// 60 times to decorate compact list rows costs far more than the rows themselves, and its
// block spacing fights the list layout.
//
// Links render as an anchor whose label is shown and whose target is in the tooltip, but a
// click does not navigate: this app has no external-link path (no OpenURL binding and no
// navigation interception), so following one would take the whole webview off the app.
// Making links live is its own change, not a side effect of rendering a finding.
const INLINE_MD = /`([^`\n]+)`|\*\*([^*\n]+)\*\*|\*([^*\n]+)\*|\[([^\]\n]+)\]\(([^)\s]+)\)/g

// inlineNodes splits text into literal chunks and inline elements, keyed by offset so a
// re-render of the same finding never remounts a node.
function inlineNodes(text: string): ReactNode[] {
  const out: ReactNode[] = []
  let last = 0
  for (const m of text.matchAll(INLINE_MD)) {
    const at = m.index ?? 0
    if (at > last) out.push(text.slice(last, at))
    const key = `md-${at}`
    if (m[1] !== undefined) out.push(<code key={key}>{m[1]}</code>)
    else if (m[2] !== undefined) out.push(<strong key={key}>{m[2]}</strong>)
    else if (m[3] !== undefined) out.push(<em key={key}>{m[3]}</em>)
    else out.push(<a key={key} className="md-link" href={m[5]} title={m[5]} onClick={(e) => e.preventDefault()}>{m[4]}</a>)
    last = at + m[0].length
  }
  if (last < text.length) out.push(text.slice(last))
  return out
}

export const InlineMd = memo(function InlineMd({ text }: { text: string }) {
  const nodes = useMemo(() => inlineNodes(text), [text])
  return <>{nodes}</>
})

// MermaidDiagram renders a ```mermaid fence as a diagram.
//
// mermaid is imported lazily, so the library only loads when a diagram actually
// appears in a reply, and rendering is debounced: while the fence is still being
// streamed the source changes every frame, which would otherwise re-parse and
// re-layout the diagram ~60x/s. Anything that does not parse (still incomplete,
// or genuinely invalid) falls back to showing the raw code instead of an empty
// box.
export const MermaidDiagram = memo(function MermaidDiagram({ code }: { code: string }) {
  const [svg, setSvg] = useState('')
  const [failed, setFailed] = useState(false)
  const [open, setOpen] = useState(false)
  // Mermaid draws its own palette, so it has to be told which theme is on
  // screen — the app theme, not the OS preference: a user who picked dark on a
  // light Mac must not get a white diagram in a dark transcript.
  const dark = useThemeSnapshot() === 'dark'

  useEffect(() => {
    let alive = true
    // Debounced: while the fence is still being streamed the source changes every
    // frame, which would otherwise re-parse and re-layout the diagram ~60x/s.
    const timer = window.setTimeout(async () => {
      try {
        const mermaid = (await import('mermaid')).default
        mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: dark ? 'dark' : 'default' })
        const { svg: out } = await mermaid.render(`mermaid-${Math.random().toString(36).slice(2, 10)}`, code)
        if (alive) { setSvg(out); setFailed(false) }
      } catch {
        if (alive) { setSvg(''); setFailed(true) }
      }
    }, 250)
    return () => { alive = false; window.clearTimeout(timer) }
  }, [code, dark])

  if (failed || !svg) {
    return (
      <div className="mermaid-wrap">
        <div className="code-actions">
          <CopyButton title="复制 mermaid 源码" getText={() => code} />
        </div>
        <pre className="mermaid-pending"><code>{code}</code></pre>
      </div>
    )
  }
  return (
    <div className="mermaid-wrap">
      <div className="code-actions">
        <CopyButton title="复制 mermaid 源码" getText={() => code} />
        <button className="code-copy" title="全屏查看" aria-label="全屏查看" onClick={() => setOpen(true)}>⤢</button>
      </div>
      {/* Clicking the diagram is the obvious gesture for "show me this bigger". */}
      <div className="mermaid" title="点击全屏查看" onClick={() => setOpen(true)} dangerouslySetInnerHTML={{ __html: svg }} />
      {open ? <MermaidViewer svg={svg} code={code} onClose={() => setOpen(false)} /> : null}
    </div>
  )
})

// CopyButton copies the text it is given and briefly confirms with "已复制".
function CopyButton({ getText, title = '复制' }: { getText: () => string; title?: string }) {
  const [done, setDone] = useState(false)
  useEffect(() => {
    if (!done) return
    const t = window.setTimeout(() => setDone(false), 1200)
    return () => window.clearTimeout(t)
  }, [done])
  return (
    <button className={`code-copy${done ? ' is-done' : ''}`} title={title} aria-label={title}
      onClick={(e) => { e.stopPropagation(); copyText(getText()); setDone(true) }}>
      {done ? <span className="code-copy-done">已复制</span> : <CopyIcon />}
    </button>
  )
}

// PreBlock intercepts fenced code blocks: ```mermaid becomes a diagram, every
// other block stays a normal <pre> (already tokenised by rehype-highlight).
// Either way the block gets a copy button — the code itself, or for a diagram
// its mermaid source.
export function PreBlock(props: ComponentPropsWithoutRef<'pre'>) {
  const ref = useRef<HTMLPreElement>(null)
  const child = Array.isArray(props.children) ? props.children[0] : props.children
  const el = child as { props?: { className?: string; children?: ReactNode } } | null
  if ((el?.props?.className || '').includes('language-mermaid')) {
    return <MermaidDiagram code={String(el?.props?.children ?? '').replace(/\n$/, '')} />
  }
  return (
    <div className="code-wrap">
      {/* Read the rendered text rather than walking the token tree: after
          rehype-highlight the code element is a tree of spans, and innerText
          gives back exactly what the user sees. */}
      <div className="code-actions">
        <CopyButton title="复制代码" getText={() => ref.current?.innerText ?? ''} />
      </div>
      <pre {...props} ref={ref} />
    </div>
  )
}

// MermaidViewer shows one diagram in the lightbox, with a copy button for its
// source — the thing you actually want out of a diagram.
function MermaidViewer({ svg, code, onClose }: { svg: string; code: string; onClose: () => void }) {
  const stageRef = useRef<HTMLDivElement>(null)
  const { zoom, pan, fit, zoomBy, reset, onWheel } = useZoomPan(stageRef)
  useViewerZoomKeys({ fit, reset, zoomBy })
  useEffect(() => { fit() }, [fit])

  return (
    <ViewerOverlay label="Mermaid 图表" stageRef={stageRef} onClose={onClose}
      stageClass={`${pan.dragging ? 'is-dragging' : ''}${pan.pannable ? ' is-pannable' : ''}`.trim()}
      stageProps={{ ...pan.handlers, onWheel }}
      controls={<>
        <ZoomControls zoom={zoom} zoomBy={zoomBy} reset={reset} fit={fit} />
        <span className="viewer-sep" />
        <CopyButton title="复制 mermaid 源码" getText={() => code} />
        <CloseButton onClose={onClose} />
      </>}>
      <div className="viewer-zoom viewer-zoom-paper" style={{ zoom }} dangerouslySetInnerHTML={{ __html: svg }} />
    </ViewerOverlay>
  )
}
