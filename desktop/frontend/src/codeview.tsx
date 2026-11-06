// The code reader: a text file drawn as source, not as a code block in a reply.
//
// The two are not the same object. A fenced block in an answer is a QUOTATION — bounded, framed,
// capped by the message column — because it sits inside prose. A file the reader opened is a
// document they came to read: it gets the room it is given, its own line numbers, and lines that
// are not re-broken under it. That is why this is a renderer of its own and not one more fence
// through MarkdownBlock, which is what the file view used to draw.
//
// Highlighting runs highlight.js directly rather than through rehype: the source has to be
// tokenised as a WHOLE and only then split into lines, because a string or a comment can span
// several of them — splitting first would colour each line on its own and break those tokens in
// half. `splitLines` therefore works on the highlighted HTML, re-opening whatever element a line
// break lands inside.

import { useEffect, useMemo, useState, type CSSProperties } from 'react'
import hljs from 'highlight.js/lib/common'
import { copyText } from './lib'

// TEXT_HIGHLIGHT_MAX is where a text preview stops being tokenised: highlight.js is linear but
// not free (roughly a second per megabyte), and a wall of colour is not what a 1MB log is for.
// Past this the same content is shown as plain text — still with line numbers.
export const TEXT_HIGHLIGHT_MAX = 256 * 1024

// One line of highlighted output, ready to be one row: `html` is the line's tokens with the
// elements still open across it already re-opened.
function escapeHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

function escapeAttr(s: string): string {
  return escapeHtml(s).replace(/"/g, '&quot;')
}

// splitLines turns one highlighted HTML fragment into one fragment per line, closing whatever
// element the line ends inside and re-opening it (with its attributes) on the next one — that is
// what keeps a multi-line comment or template string coloured to its last character.
function splitLines(html: string): string[] {
  const root = document.createElement('div')
  root.innerHTML = html
  const lines: string[] = []
  let cur = ''
  const walk = (node: Node, open: { name: string; attrs: string }[]) => {
    if (node.nodeType === Node.TEXT_NODE) {
      const parts = (node.nodeValue || '').split('\n')
      parts.forEach((part, i) => {
        if (i > 0) {
          cur += open.map((t) => `</${t.name}>`).join('')
          lines.push(cur)
          cur = open.map((t) => `<${t.name}${t.attrs}>`).join('')
        }
        cur += escapeHtml(part)
      })
      return
    }
    if (node.nodeType !== Node.ELEMENT_NODE) return
    const el = node as Element
    const tag = {
      name: el.tagName.toLowerCase(),
      attrs: Array.from(el.attributes).map((a) => ` ${a.name}="${escapeAttr(a.value)}"`).join(''),
    }
    cur += `<${tag.name}${tag.attrs}>`
    el.childNodes.forEach((child) => walk(child, [...open, tag]))
    cur += `</${tag.name}>`
  }
  root.childNodes.forEach((child) => walk(child, []))
  lines.push(cur)
  return lines
}

// rowsHtml is the whole body as one string. It is handed to the container as innerHTML rather than
// rendered as one React element per line: a previewed file can be 20k lines, and 20k elements is a
// lot of React for a list nobody will ever diff — one parse of the same text is cheap.
function rowsHtml(lines: string[]): string {
  return lines
    .map((line, i) =>
      `<div class="cv-line"><span class="cv-ln" data-find-skip aria-hidden="true">${i + 1}</span>` +
      `<span class="cv-src">${line}</span></div>`)
    .join('')
}

// CodeView is the reader itself. `language` is the backend's highlight.js name (preview.go's
// langByExt); anything the bundle does not know, or a file past the tokenising cap, is shown plain.
export function CodeView({ text, language }: { text: string; language: string }) {
  const [wrapped, setWrapped] = useState(false)
  // CRLF and lone CR are normalised before anything else: the split below is on \n, and a stray
  // \r would end up in the rendered text of a line.
  const source = useMemo(() => text.replace(/\r\n?/g, '\n').replace(/\n$/, ''), [text])
  const { rows, count, lang } = useMemo(() => {
    const known = language && hljs.getLanguage(language) ? language : ''
    const colour = known && source.length <= TEXT_HIGHLIGHT_MAX
    const html = colour ? hljs.highlight(source, { language: known }).value : escapeHtml(source)
    const lines = splitLines(html)
    return { rows: rowsHtml(lines), count: lines.length, lang: known }
  }, [source, language])

  return (
    <div className={`codeview${wrapped ? ' is-wrapped' : ''}`}
      style={{ '--cv-gutter': `${String(count).length}ch` } as CSSProperties}>
      <div className="cv-bar">
        <span className="cv-lang">{lang || '纯文本'}</span>
        <span className="cv-count">{count} 行</span>
        <span className="cv-tools">
          <button type="button" className="cv-tool" aria-pressed={wrapped}
            title={wrapped ? '关掉自动折行（恢复横向滚动）' : '自动折行：长行换行显示，而不是横向滚动'}
            onClick={() => setWrapped((v) => !v)}>折行</button>
          <CopyTool text={source} />
        </span>
      </div>
      <div className="cv-lines" dangerouslySetInnerHTML={{ __html: rows }} />
    </div>
  )
}

// CopyTool is the bar's copy button, with the 「已复制」 beat every copy in the app has. It is its
// own here rather than markdown.tsx's CopyButton because that one is styled for a code block's
// header — and this renderer deliberately does not go through the markdown pipeline at all.
function CopyTool({ text }: { text: string }) {
  const [done, setDone] = useState(false)
  useEffect(() => {
    if (!done) return
    const t = window.setTimeout(() => setDone(false), 1200)
    return () => window.clearTimeout(t)
  }, [done])
  return (
    <button type="button" className={`cv-tool${done ? ' is-done' : ''}`} title="复制整个文件"
      onClick={() => { copyText(text); setDone(true) }}>{done ? '已复制' : '复制'}</button>
  )
}
