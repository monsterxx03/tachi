// The attachment card: a file the agent handed over with SendFile, rendered
// wherever that call happened — the live turn and reloaded history alike, since
// it is rebuilt from the recorded tool call (fileFromSendFileArgs).
//
// Preview is deliberately two-stage. Mounting the card asks the backend for
// METADATA only (kind, size, whether a preview exists at all): that is what draws
// "README.md · 12.4 KB" and decides whether there is a 预览 button. The content
// is fetched when the user expands it, and for text kinds only — images and HTML
// are loaded from disk by the /local asset route, so a 20MB screenshot never
// travels through the IPC channel.

import { memo, useCallback, useEffect, useRef, useState } from 'react'
import { AgentService, type FilePreviewVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { fmtBytes, toLocalAsset, toLocalPath } from './lib'
import { revealFileInTree } from './filereveal'
import { REVEAL_LABEL } from './platform'
import { MarkdownBlock } from './markdown'
import { SelectionAsk } from './selask'
import { CloseButton, ImageViewer, ViewerOverlay } from './viewer'
import type { AttachmentInfo } from './types'

// KIND_ICON is the glyph a kind leads with. The card shows one before the backend has answered, so
// the plain-document icon is also the fallback — and the file tree draws the SAME map, which is what
// keeps two columns from spelling one file type two ways.
export const KIND_ICON: Record<string, string> = {
  image: '🖼',
  markdown: '📝',
  html: '🌐',
  pdf: '📕',
  csv: '📊',
  text: '📄',
  binary: '📦',
  dir: '📁',
  missing: '⚠️',
}

// TEXT_HIGHLIGHT_MAX is where a text preview stops being tokenised: highlight.js
// is linear but not free (roughly a second per megabyte), and a wall of colour is
// not what a 1MB log is for. Past this the same content is shown as plain,
// copyable text.
const TEXT_HIGHLIGHT_MAX = 256 * 1024

// attachmentPath resolves the path the way the SendFile tool did: an absolute one
// as-is, a relative one against the session's working directory — the model may
// well have written "report.md", and the card has to point at the same file the
// tool sent.
function attachmentPath(path: string, workDir: string): string {
  if (path.startsWith('/') || !workDir) return path
  return `${workDir.replace(/\/+$/, '')}/${path}`
}

// dirOf is the directory a previewed document resolves its relative references
// against (a README's ![](shot.png)): the file's own directory, which is where
// whoever wrote it meant them to point.
function dirOf(path: string): string {
  const i = path.lastIndexOf('/')
  return i > 0 ? path.slice(0, i) : ''
}

// fenced wraps file content in a markdown fence, so a text preview goes through
// the same pipeline as code in a reply: rehype-highlight tokenises it and PreBlock
// adds the copy button. The fence is one backtick longer than the longest run in
// the content — a fence is closed by a run at least as long as its opener, so no
// content can end the block early however it is written.
function fenced(text: string, language: string): string {
  let longest = 0
  for (const run of text.match(/`+/g) || []) {
    if (run.length > longest) longest = run.length
  }
  const fence = '`'.repeat(Math.max(3, longest + 1))
  return `${fence}${language}\n${text.replace(/\n$/, '')}\n${fence}\n`
}

// needsText reports which kinds the card has to fetch content for. The others
// render from disk through /local (images, HTML, PDF) or are not previewed.
function needsText(kind: string): boolean {
  return kind === 'markdown' || kind === 'text' || kind === 'csv'
}

// FileBodyVariant is WHICH surface is rendering the file. It decides nothing about the file and
// everything about the frame around it: a card in the transcript, the full-screen viewer, or the
// file view that takes the content column.
export type FileBodyVariant = 'card' | 'viewer' | 'view'

// The classes each surface gives the two things a previewed file needs: the paper column a
// DOCUMENT sits on, and the frame the /local kinds are drawn in. Documents are exactly the
// needsText set — which kinds those are is answered once, above.
const BODY_FRAMES: Record<FileBodyVariant, { doc: string; frame: string; img: string; table: string }> = {
  card: { doc: 'file-preview-doc', frame: 'file-preview-frame', img: 'file-preview-img', table: '' },
  viewer: { doc: 'viewer-doc', frame: 'viewer-frame', img: 'viewer-image', table: 'is-table' },
  view: { doc: 'fileview-doc', frame: 'fileview-frame', img: 'fileview-img', table: 'is-table' },
}

// FileBody renders one previewed file's CONTENT: the one place where a kind decides how a file is
// drawn. The attachment card, the full-screen viewer and the file view all render this, so a kind
// cannot work in one of them and be missing in another — and preview.go's tables stay the single
// answer to what a file IS (the tree's icon hint is only a hint; this is where it is settled).
//
// What each surface adds around it stays with that surface: the card's 预览/收起 button, the
// viewer's own controls and findability, the file view's header and width. `onExpand` is the one
// optional gesture a content kind may offer outwards (the card's ⤢, the file view's lightbox).
export function FileBody({ meta, path, name, variant = 'card', onExpand }: {
  meta: FilePreviewVO
  path: string
  name: string
  variant?: FileBodyVariant
  onExpand?: () => void
}) {
  const frame = BODY_FRAMES[variant]
  const note = meta.truncated ? <TruncatedNotice meta={meta} /> : null
  const expand = onExpand ? <ExpandButton onFull={onExpand} /> : null
  if (meta.error) return <div className="file-preview-note">无法读取：{meta.error}</div>

  switch (meta.kind) {
    case 'image':
      return (
        <>
          <img className={frame.img} src={toLocalAsset(path, '')} alt={name}
            title="点击全屏查看" onClick={onExpand} />
          {expand}
        </>
      )
    case 'html':
      return (
        <>
          {/* Sandboxed with scripts allowed but WITHOUT allow-same-origin: a
              generated report's charts run, while the document itself sits in an
              opaque origin and can reach neither this page, the Wails bridge, nor
              read anything back out of /local. */}
          <iframe className={frame.frame} src={toLocalPath(path)} title={name} sandbox="allow-scripts" />
          {expand}
        </>
      )
    case 'pdf':
      return (
        <>
          {/* No sandbox: the frame IS the webview's own PDF viewer (PDFKit), which
              needs the frame unrestricted to lay the pages out. A PDF cannot run
              script here — WebKit's viewer has no script engine — so the frame has
              nothing to isolate it from. */}
          <iframe className={`${frame.frame} is-pdf`} src={toLocalPath(path)} title={name} />
          {expand}
        </>
      )
    case 'csv':
      return (
        <div className={`${frame.doc}${frame.table ? ' ' + frame.table : ''}`.trim()}>
          {note}
          <CSVTable meta={meta} />
          {expand}
        </div>
      )
    case 'markdown':
    case 'text':
      return (
        <div className={frame.doc}>
          {note}
          <AttachmentText path={path} meta={meta} />
          {expand}
        </div>
      )
    default:
      return <div className="file-preview-note">该类型不支持预览</div>
  }
}

// FileCard is the attachment itself. `workDir` is the session's working
// directory, used to resolve a relative SendFile path.
export const FileCard = memo(function FileCard({ file, workDir }: { file: AttachmentInfo; workDir: string }) {
  const path = attachmentPath(file.path, workDir)
  const [meta, setMeta] = useState<FilePreviewVO | null>(null)
  const [body, setBody] = useState<FilePreviewVO | null>(null)
  const [open, setOpen] = useState(false)
  const [full, setFull] = useState(false)
  const [busy, setBusy] = useState(false)

  // One cheap call per card (a stat + an 8KB probe): it is what turns the card
  // from "some file" into "README.md · 12.4 KB, previewable".
  useEffect(() => {
    let alive = true
    setMeta(null)
    setBody(null)
    setOpen(false)
    setFull(false)
    AgentService.PreviewFile(path, false)
      .then((vo) => { if (alive) setMeta(vo) })
      .catch(() => { /* no backend (simulated mode): the card stays on its fallback icon */ })
    return () => { alive = false }
  }, [path])

  const toggle = useCallback(() => {
    const next = !open
    setOpen(next)
    // Opening the preview is also the moment to show the reader where this file lives, if they have the
    // tree open (filereveal.ts). It is a select and an expand, never a view switch.
    if (next) revealFileInTree(path)
    if (!next || body || !meta || !needsText(meta.kind)) return
    setBusy(true)
    AgentService.PreviewFile(path, true)
      .then((vo) => setBody(vo || null))
      .catch(() => setBody({ ...meta, error: '读取失败' }))
      .finally(() => setBusy(false))
  }, [open, body, meta, path])

  const shown = body || meta
  const note = !meta ? '' : meta.kind === 'missing'
    ? '文件已不存在（可能已被移动或删除）'
    : meta.error ? `无法读取：${meta.error}`
      : !meta.previewable ? '该类型不支持预览，可用「打开」交给系统应用'
        : ''

  return (
    <div className="file-card">
      <div className="file-head">
        <span className="file-ico" aria-hidden="true">{KIND_ICON[meta?.kind || ''] || '📄'}</span>
        <span className="file-name" title={path}>{file.name}</span>
        {meta && meta.size > 0 ? <span className="file-size">{fmtBytes(meta.size)}</span> : null}
        <span className="file-actions">
          {meta?.previewable ? <button className="file-btn" onClick={toggle}>{open ? '收起' : '预览'}</button> : null}
          {meta && meta.kind !== 'missing' ? (
            <>
              <button className="file-btn" onClick={() => { AgentService.OpenPath(path).catch(() => {}) }}>打开</button>
              <button className="file-btn" onClick={() => { AgentService.RevealPath(path).catch(() => {}) }}>{REVEAL_LABEL}</button>
            </>
          ) : null}
        </span>
      </div>
      {note ? <div className="file-note">{note}</div> : null}
      {open && shown ? <FilePreviewBody file={file} path={path} meta={shown} busy={busy} onFull={() => setFull(true)} /> : null}
      {full && shown ? <FileFullscreen file={file} path={path} meta={shown} onClose={() => setFull(false)} /> : null}
    </div>
  )
})

// FilePreviewBody is the card's inline preview: the content through FileBody, plus the one state
// that is about the READ rather than the file (a fetch still in flight). Every kind is bounded in
// height by CSS and scrolls inside the card; the ⤢ button (or clicking an image) opens the same
// content in the lightbox.
function FilePreviewBody({ file, path, meta, busy, onFull }: {
  file: AttachmentInfo
  path: string
  meta: FilePreviewVO
  busy: boolean
  onFull: () => void
}) {
  // 选中文本的菜单（selask.tsx）挂在这个盒子上。ref 必须声明在下面那次提前返回之前：hook
  // 不能是条件性的，而「读取中」那一帧就直接返回了。
  const bodyRef = useRef<HTMLDivElement>(null)
  if (busy) return <div className="file-preview-note">读取中…</div>
  return (
    // The card's own box: the border the preview hangs under, the plane it sits on, and the
    // positioned ancestor the ⤢ button anchors to.
    <div className="file-preview-body" ref={bodyRef}>
      <FileBody meta={meta} path={path} name={file.name} variant="card" onExpand={onFull} />
      {/* 来源是绝对路径：它要写进发出去的那条消息里，模型据此去读整个文件；同时它也是收起
          这张卡时「哪些待提问跟着走」的依据（见 selask.tsx）。会话里的卡片预览和文件视图给
          的是同一句话，因为它们是一回事。 */}
      <SelectionAsk hostRef={bodyRef} source={path} />
    </div>
  )
}

// FileFullscreen shows one attachment in the lightbox. Images and HTML get the
// whole viewport; the text kinds sit on a paper card with a reading measure,
// because a README in the full width of a 27" display is unreadable.
//
// The content itself comes from FileBody (variant 'viewer'), so the lightbox and the file view
// cannot disagree about how a kind is drawn — only about the frame around it.
function FileFullscreen({ file, path, meta, onClose }: {
  file: AttachmentInfo
  path: string
  meta: FilePreviewVO
  onClose: () => void
}) {
  if (meta.kind === 'image') {
    return <ImageViewer src={toLocalAsset(path, '')} alt={file.name} onClose={onClose} />
  }
  if (meta.kind === 'html' || meta.kind === 'pdf') {
    return (
      <ViewerOverlay label={`${file.name} — ${meta.kind === 'pdf' ? 'PDF' : 'HTML'} 预览`} onClose={onClose} stageClass="is-doc" controls={<>
        <button className="viewer-btn" title="用系统默认应用打开" onClick={() => { AgentService.OpenPath(path).catch(() => {}) }}>打开</button>
        <CloseButton onClose={onClose} />
      </>}>
        <FileBody meta={meta} path={path} name={file.name} variant="viewer" />
      </ViewerOverlay>
    )
  }
  if (meta.kind === 'csv') {
    return (
      // A table is text this page owns, so it gets the select-text menu too — a row copied out of a
      // spreadsheet is the other reason to open one here.
      <ViewerOverlay label={`${file.name} — 表格`} onClose={onClose} stageClass="is-doc" findable
        selectionSource={path} controls={<CloseButton onClose={onClose} />}>
        <FileBody meta={meta} path={path} name={file.name} variant="viewer" />
      </ViewerOverlay>
    )
  }
  // The text kinds are the last ones that are OURS to search: an HTML or PDF viewer draws
  // inside an iframe, a different document this one cannot reach into (see FileViewer above).
  // Same reason it is the only attachment overlay that offers the select-text menu (复制 / 提问):
  // a selection inside that iframe belongs to the iframe's document, and what this page can read
  // back is nothing.
  return (
    <ViewerOverlay label={file.name} onClose={onClose} stageClass="is-doc" findable
      selectionSource={path} controls={<CloseButton onClose={onClose} />}>
      <FileBody meta={meta} path={path} name={file.name} variant="viewer" />
    </ViewerOverlay>
  )
}

// AttachmentText renders the text kinds: markdown as markdown (mermaid fences
// drawn — a diagram in a README is usually the point), everything else as
// highlighted source. Shared by the inline preview and the lightbox so the two
// cannot drift apart.
function AttachmentText({ path, meta }: { path: string; meta: FilePreviewVO }) {
  const text = meta.text || ''
  if (meta.kind === 'markdown') return <MarkdownBlock text={text} workDir={dirOf(path)} />
  const language = text.length > TEXT_HIGHLIGHT_MAX ? '' : meta.language
  return <MarkdownBlock text={fenced(text, language)} workDir={dirOf(path)} />
}

// CSVTable renders the table the backend parsed: row 0 is the header, the body
// scrolls inside its container, and the header stays put while it does — a
// 200-row export is unreadable once you have lost the column names.
//
// Short rows are padded rather than stretched: a CSV with trailing empty cells
// (what most exporters write) and one that simply omits them must look the same.
function CSVTable({ meta }: { meta: FilePreviewVO }) {
  // The generated binding types every row as possibly null (Go's [][]string), so
  // one pass normalises them: a row is a list of cells, empty when the file has
  // an empty line.
  const rows = (meta.rows || []).map((r) => r || [])
  if (rows.length === 0) return <div className="file-preview-note">空文件</div>

  const [head, ...body] = rows
  const cols = Math.max(head.length, ...body.map((r) => r.length))
  const cell = (r: string[], i: number) => r[i] ?? ''

  return (
    <div className="csv-scroll">
      <table className="csv-table">
        <thead>
          <tr>{Array.from({ length: cols }, (_, i) => <th key={i} title={cell(head, i)}>{cell(head, i)}</th>)}</tr>
        </thead>
        <tbody>
          {body.map((r, ri) => (
            <tr key={ri}>
              {Array.from({ length: cols }, (_, ci) => {
                const v = cell(r, ci)
                return <td key={ci} title={v} className={isNumericCell(v) ? 'is-num' : ''}>{v}</td>
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// isNumericCell marks a cell for right alignment: numbers read as columns when
// their digits line up, and this is the one bit of spreadsheet convention that a
// plain table needs.
function isNumericCell(v: string): boolean {
  const s = v.trim()
  return s !== '' && /^[-+]?[\d,]*\.?\d+%?$/.test(s)
}

// ExpandButton is the card's way into the lightbox. An explicit button rather
// than "click the content": an iframe swallows clicks on its own area, and every
// kind should offer the same affordance.
function ExpandButton({ onFull }: { onFull: () => void }) {
  return <button className="file-expand" title="全屏查看" aria-label="全屏查看" onClick={onFull}>⤢</button>
}

// TruncatedNotice says the preview is a prefix. The size cap itself is the
// backend's (desktop/preview.go) and is not repeated here — a hard-coded number
// would drift the moment the cap moves.
function TruncatedNotice({ meta }: { meta: FilePreviewVO }) {
  return <div className="file-preview-note">文件较大，此处只显示开头部分（共 {fmtBytes(meta.size)}）</div>
}

// FilePreviewOverlay opens the attachment preview for an arbitrary local path — the
// same viewers an attachment card uses (markdown with its mermaid, HTML sandbox, image
// lightbox, highlighted source, CSV table). It exists so "look at this file" means one
// thing in this app wherever it is asked from: the SendFile card or the diff panel.
//
// Unlike FileCard this fetches eagerly: the caller has already decided to show the file,
// so there is no card to render first.
export function FilePreviewOverlay({ path, name, onClose }: { path: string; name?: string; onClose: () => void }) {
  const [meta, setMeta] = useState<FilePreviewVO | null>(null)
  const base = name || path.split('/').pop() || path

  useEffect(() => {
    let alive = true
    AgentService.PreviewFile(path, true)
      .then((vo) => { if (alive) setMeta(vo) })
      .catch(() => { if (alive) setMeta(null) })
    return () => { alive = false }
  }, [path])

  if (!meta || !meta.previewable) {
    return (
      <ViewerOverlay label={base} onClose={onClose} stageClass="is-doc" controls={<CloseButton onClose={onClose} />}>
        <div className="viewer-doc diff-panel-empty">
          {meta === null ? '读取中…' : meta.error ? `无法读取：${meta.error}` : '这个文件没有可预览的内容，用「打开」交给系统应用'}
        </div>
      </ViewerOverlay>
    )
  }
  return <FileFullscreen file={{ path, name: base }} path={path} meta={meta} onClose={onClose} />
}
