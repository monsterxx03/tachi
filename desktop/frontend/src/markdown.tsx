// Markdown rendering: one pipeline, shared by the transcript's assistant replies
// and by the attachment preview (a .md the agent sent, a source file shown as
// highlighted code). Keeping it in one module is what makes "the same file looks
// the same in the chat and in the card" true by construction.

import { memo, useCallback, useEffect, useMemo, useRef, useState, type ComponentPropsWithoutRef, type ReactNode, type RefObject } from 'react'
import ReactMarkdown, { defaultUrlTransform } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import rehypeHighlight from 'rehype-highlight'
import { AgentService } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { CopyIcon } from './components'
import { copyText, actOnKey, linkTargetPath, toLocalAsset } from './lib'
import { revealFileInTree } from './filereveal'
import { FilePreviewOverlay } from './filepreview'
import { useThemeSnapshot } from './theme'
import { CloseButton, ImageViewer, ViewerOverlay, ZoomControls, useViewerZoomKeys, useZoomPan } from './viewer'

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

// MdLink is the app's ONLY way out: a link in prose hands its URL to the SYSTEM browser
// (AgentService.OpenURL) instead of to the webview.
//
// A link that names a file ON DISK is the exception, and the interesting one: it opens the same
// in-app preview the diff panel and an attachment card use, because "look at this file" is what
// the reader meant — handing a local path to the system launcher is how a reply's
// `[desktop.md](desktop.md)` used to do nothing at all (issue #49).
//
// Which of the two a click means is decided by the BACKEND, not by a shape test here: a link like
// `[state](/concepts/state)` (a site-absolute URL path, which replies do contain) is
// indistinguishable from an absolute disk path until something asks the disk. So the click first
// asks whether the path names anything — PreviewFile's own `missing` kind is that answer — and
// falls back to the browser when it does not, which leaves exactly the old behaviour for every
// link that is not a file.
//
// A plain <a href> in this page is a one-way door. The webview has no tabs and no back button —
// the window IS the app — so following one replaced the transcript, the sidebar and the composer
// with the page and left the reader with nothing to go back to (issue #48). `preventDefault` on the
// click cancels the navigation before WebKit acts on it, and that is the ONE event every form of the
// gesture goes through (a plain click, ⌘-click): the handler does the rest. The backend refuses any
// scheme that is not http/https/mailto (attach.go), so a model-written `file:` link is not a way to
// reach the machine either. (Those do not even arrive here: react-markdown drops a `file:` href
// before rendering — see keepInlineImage.)
//
// The href STAYS on the element: it is what makes the target readable on hover, what a screen
// reader announces as a link, and the only thing that says where the click goes.
//
// ONE component for both renderers — the transcript's replies (MarkdownBlock) and a review
// finding's own prose (InlineMd) — because "what a link does" is one fact, and a second renderer
// with its own idea of it is how a link becomes dead in one place and live in another. A caller
// that knows the session's working directory passes it (MarkdownBlock does); without one a
// relative path cannot be resolved, so those links keep the browser-only behaviour.
export function MdLink({ href, children, title, workDir }: ComponentPropsWithoutRef<'a'> & { workDir?: string }) {
  const [preview, setPreview] = useState<string | null>(null)
  const url = typeof href === 'string' ? href.trim() : ''
  // A malformed link (no target at all) renders as its own label: a control that cannot go
  // anywhere is worse than plain text.
  if (!url) return <>{children}</>
  const local = linkTargetPath(url, workDir || '')
  const asBrowser = () => { AgentService.OpenURL(url).catch(() => {}) }
  const onClick = (e: { preventDefault: () => void }) => {
    e.preventDefault()
    if (!local) { asBrowser(); return }
    // The file question, then the preview. A content-less PreviewFile is a plain stat, and it
    // answers for directories and unreadable files too — an overlay that explains the kind is
    // the right place for those; only "there is nothing at this path" goes to the browser.
    AgentService.PreviewFile(local, false)
      .then((vo) => {
        if (vo.kind === 'missing') { asBrowser(); return }
        setPreview(local)
        // …and if the reader has the file tree open, show them where this file lives (filereveal.ts).
        // It is the same sentence as the "open in the tree" gesture, asked from the other side.
        revealFileInTree(local)
      })
      .catch(asBrowser)
  }
  return (
    <>
      <a className="md-link" href={url} title={title || (local ? `${url}（点击预览）` : url)}
        onClick={onClick}>
        {children}
      </a>
      {/* The overlay is a portal (see viewer.tsx), so mounting it here does not escape the
          paragraph the link sits in. The import is a cycle — filepreview renders markdown, and
          markdown now opens a preview — but neither module reads the other while it is being
          evaluated (both only inside a render), which is what keeps it benign. */}
      {preview ? <FilePreviewOverlay path={preview} onClose={() => setPreview(null)} /> : null}
    </>
  )
}

// MarkdownBlock renders one markdown document. Memoized on (text, workDir),
// which is what keeps a streaming turn from re-parsing every earlier reply on
// each animation frame.
export const MarkdownBlock = memo(function MarkdownBlock({ text, workDir }: { text: string; workDir: string }) {
  const MdImg = useCallback(({ src, alt, title }: { src?: string; alt?: string; title?: string }) => (
    <MdImage src={src} alt={alt} title={title} workDir={workDir} />
  ), [workDir])
  // The link renderer needs workDir for the same reason the image one does: a relative path in a
  // reply is relative to the session's working directory. It is a bound component rather than a
  // prop on each <a> because react-markdown only hands a component the element's own attributes.
  const MdLinkHere = useCallback(
    (props: ComponentPropsWithoutRef<'a'>) => <MdLink {...props} workDir={workDir} />, [workDir])
  return (
    <div className="assistant-text">
      {/* rehypeHighlight tokenises fenced code; PreBlock turns ```mermaid into a
          diagram and leaves everything else as a plain <pre>. */}
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[rehypeHighlight]}
        urlTransform={keepInlineImage}
        components={{ img: MdImg, table: TableScroller, pre: PreBlock, a: MdLinkHere }}>
        {text}
      </ReactMarkdown>
    </div>
  )
})

// keepInlineImage is react-markdown's own URL policy plus ONE exception: `data:image/…`.
//
// The library strips every URL whose protocol is outside its allowlist (http/https/mailto/irc/xmpp),
// and `data:` is on that list — so an answer that embeds a picture inline (a small icon, a generated
// diagram) arrived as a BROKEN image, with nothing wrong with the source. The exception is narrow on
// purpose: only for an <img>, and only `data:image/`. A data URL fetches nothing over the network,
// and inside an `<img>` it cannot execute anything either — an SVG's scripts do not run in an image
// context — which is what makes this safe where a `data:` LINK would not be.
function keepInlineImage(url: string, _key: string, node: { tagName?: string } | undefined): string {
  if (node && node.tagName === 'img' && /^data:image\//i.test(url)) return url
  return defaultUrlTransform(url)
}

// MdImage is a markdown image: clicking it (or Enter, it is a real control) opens the SAME
// lightbox an attachment uses — so a screenshot the agent pasted into its answer can be read at
// full size instead of being capped by the message column, which is where a diagram or a UI shot
// with text in it stops being legible. It is a <span role="button"> rather than a wrapper
// element because markdown puts images inside a <p>, where a <div> is not allowed; the lightbox
// itself is a portal (see viewer.tsx), so nothing here escapes the paragraph.
//
// A local path that does not resolve is a broken image whose alt text is easy to read as a
// caption: it says so instead. `title` is the markdown title — kept, because the author put it
// there for the reader.
function MdImage({ src, alt, title, workDir }: { src?: string; alt?: string; title?: string; workDir: string }) {
  const [open, setOpen] = useState(false)
  const [failed, setFailed] = useState(false)
  const url = toLocalAsset(src, workDir)
  if (!src || failed) {
    return <span className="md-img-failed" title={src || ''}>图片无法加载{alt ? `：${alt}` : ''}</span>
  }
  return (
    <>
      <span className="md-img" role="button" tabIndex={0} title={title || '点击放大'}
        onClick={() => setOpen(true)} onKeyDown={actOnKey(() => setOpen(true))}>
        <img src={url} alt={alt || ''} title={title} onError={() => setFailed(true)} />
      </span>
      {open ? <ImageViewer src={url} alt={alt || ''} onClose={() => setOpen(false)} /> : null}
    </>
  )
}

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
// Links go through the SAME MdLink the transcript's replies use: a finding's `[label](url)` is
// followed by the same click, into the system browser. It used to be a dead anchor (the label was
// shown, the target sat in the tooltip, and a click did nothing) because there was no
// external-link path at all; there is now, and leaving this half inert would have meant two
// answers to "what does a link do" a few hundred lines apart.
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
    else out.push(<MdLink key={key} href={m[5]}>{m[4]}</MdLink>)
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
        // suppressErrorRendering: without it, a diagram that does not parse makes mermaid DRAW its
        // own error card ("Syntax error in text / mermaid version …") into a temporary node appended
        // to <body>, and only then throw — where this component's catch turns the reply into the
        // source below. That card is therefore outside the message, it is never removed, and it
        // outlives the conversation it came from: it floats at the bottom of the window over every
        // session opened afterwards. With the flag on, mermaid removes its temp nodes and throws, so
        // the degradation below is the only thing the reader sees.
        mermaid.initialize({
          startOnLoad: false, securityLevel: 'strict', suppressErrorRendering: true,
          theme: dark ? 'dark' : 'default',
        })
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
      <div
        className="mermaid"
        title="点击全屏查看"
        onClick={() => setOpen(true)}
        dangerouslySetInnerHTML={{ __html: svg }}
      />
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
// other block becomes a code block with its own header (see CodeBlock).
export function PreBlock(props: ComponentPropsWithoutRef<'pre'>) {
  const codeRef = useRef<HTMLPreElement>(null)
  const child = Array.isArray(props.children) ? props.children[0] : props.children
  const el = child as { props?: { className?: string; children?: ReactNode } } | null
  if ((el?.props?.className || '').includes('language-mermaid')) {
    return <MermaidDiagram code={String(el?.props?.children ?? '').replace(/\n$/, '')} />
  }
  return <CodeBlock preProps={props} codeRef={codeRef} lang={langOf(el?.props?.className)} />
}

// langOf reads the info string react-markdown turned into the code element's class
// (`language-go`), which is the language the block was TAGGED with. '' when it was not tagged.
function langOf(className: string | undefined): string {
  const m = /(?:^|\s)language-([\w+#.-]+)/.exec(className || '')
  return m ? m[1] : ''
}

// CodeBlock is a fenced code block with its own header: the language, a copy button, and a wrap
// toggle.
//
// The header replaced a button that FLOATED over the block — it sat on the first line of code, was
// invisible until the reader happened to hover, and covered the very characters it copied on a
// short block. Everything it needs now sits on a real edge above the code, and the language (which
// rehype-highlight had been tagging all along, invisibly) is readable without hovering anything.
//
// Wrapping is the reader's call, not a default: a long line is usually a path or a command where
// the horizontal shape carries meaning, and a block that silently re-wraps it is harder to read
// than one that scrolls. Whoever wants it back switches it off, and the choice is per block —
// it is about THIS output (a log, a wide JSON), not a global preference.
function CodeBlock({ preProps, codeRef, lang }: {
  preProps: ComponentPropsWithoutRef<'pre'>
  codeRef: RefObject<HTMLPreElement>
  lang: string
}) {
  const [wrapped, setWrapped] = useState(false)
  return (
    <div className={`code-wrap${wrapped ? ' is-wrapped' : ''}`}>
      <div className="code-head">
        <span className="code-lang">{lang}</span>
        <span className="code-head-actions">
          <button type="button" className="code-tool" aria-pressed={wrapped}
            title={wrapped ? '关掉自动折行（恢复横向滚动）' : '自动折行：长行换行显示，而不是横向滚动'}
            onClick={() => setWrapped((v) => !v)}>折行</button>
          {/* Read the rendered text rather than walking the token tree: after
              rehype-highlight the code element is a tree of spans, and innerText
              gives back exactly what the user sees — wrapped or not (a soft wrap
              is not a newline in innerText, so the copy stays the source text). */}
          <CopyButton title="复制代码" getText={() => codeRef.current?.innerText ?? ''} />
        </span>
      </div>
      <pre {...preProps} ref={codeRef} />
    </div>
  )
}

// MermaidViewer shows one diagram in the lightbox, with a copy button for its
// source — the thing you actually want out of a diagram.
function MermaidViewer({ svg, code, onClose }: { svg: string; code: string; onClose: () => void }) {
  const stageRef = useRef<HTMLDivElement>(null)
  const paperRef = useRef<HTMLDivElement>(null)
  const { zoom, pan, fit, zoomBy, reset, onWheel } = useZoomPan(stageRef)
  useViewerZoomKeys({ fit, reset, zoomBy })
  useEffect(() => {
    // Mermaid leaves `width="100%"` on its SVG and writes the diagram's NATURAL width into its
    // own inline style (`max-width: 1128px`). Inside this max-content paper the percentage falls
    // back to the replaced-element default (300px), so the diagram was laid out at 300x17 while
    // the message showed it at 708x40 — and "1:1" meant 300. Make the natural width definite
    // BEFORE fitting, so 1:1 is the diagram's real size and fit measures something meaningful.
    const el = paperRef.current?.querySelector('svg')
    const natural = el?.style.maxWidth
    if (el && natural) el.style.width = natural
    fit()
  }, [fit, svg])

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
      <div ref={paperRef} className="viewer-zoom viewer-zoom-paper" style={{ zoom }} dangerouslySetInnerHTML={{ __html: svg }} />
    </ViewerOverlay>
  )
}
