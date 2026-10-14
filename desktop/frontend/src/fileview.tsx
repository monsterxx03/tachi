// The file view: one file, read in the content column.
//
// It is the OTHER half of the file tree — the tree navigates, this reads — and it takes the column
// the conversation was in rather than a box of its own, because reading a document needs that width
// (design §4.7). The conversation stays MOUNTED underneath it (only inert): its scroll position, the
// composer's draft and a streaming reply all survive a look at a file, which is the same bargain the
// settings page makes and the reason it is not unmounted.
//
// Three rules live here rather than in the tree:
//
//   - ⌘F belongs to this view while it is up. The text on screen is the file's, not the
//     conversation's (its own find bar is closed while this view stands), and find.tsx's stack makes
//     the topmost host the owner — this one mounts last.
//   - Esc leaves. It is a view with its own two ways in and out, exactly like the settings page.
//   - THE BODY IS NEVER REWRITTEN FROM THE POLL. When the file moves on disk the reader gets a line
//     offering to re-read it; re-reading on its own would reflow a document under their eyes, in the
//     one place they chose to look (design §8.4).

import { useEffect, useRef, useState } from 'react'
import { AgentService, type FilePreviewVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { FileBody } from './filepreview'
import { FindBar, FindButton, useFind } from './find'
import { copyText, fmtBytes, toLocalAsset } from './lib'
import { REVEAL_LABEL } from './platform'
import { ImageViewer } from './viewer'
import type { FileTreeApi } from './useFileTree'

// relPath is a file's path as the header shows it: the part below its root, with the root's label in
// front of it when that root has one. The label comes from the BACKEND (rootLabels: "" is the primary
// root, otherwise the root's base name — or its full path when two roots share one), so this is only
// the arithmetic on two paths the view already holds.
function relPath(root: string, label: string, path: string): string {
	const rel = root && path.startsWith(root) ? path.slice(root.length).replace(/^\/+/, '') : path
	return label ? `${label}/${rel}` : rel
}

// stampTime renders a modification time as the header's short clock (12:03), computed from the poll's
// millisecond value rather than from a date string: the backend hands over what stat said.
function stampTime(ms: number): string {
	const d = new Date(ms)
	if (Number.isNaN(d.getTime())) return ''
	return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}

export function FileView({ api, sessionId, onBack }: {
	api: FileTreeApi
	sessionId: string
	onBack: () => void
}) {
	const selected = api.selected
	const path = selected?.path || ''
	const [meta, setMeta] = useState<FilePreviewVO | null>(null)
	const [zoom, setZoom] = useState(false)
	// What the file looked like when this view READ it. The poll's answer for the same path is
	// compared against it, which is how 「文件已更新」 knows there is something new to offer.
	const [read, setRead] = useState<{ mtimeMs: number; size: number } | null>(null)
	const [nonce, setNonce] = useState(0)
	const bodyRef = useRef<HTMLDivElement>(null)
	const find = useFind(bodyRef)

	// One read per file (and per explicit re-read). The stamp is taken AFTER the content, from the
	// same stat the poll uses, so the two are comparable — and a content read that failed still gets a
	// stamp, because "it is gone" is exactly what the header has to be able to say.
	useEffect(() => {
		if (!path) { setMeta(null); setRead(null); return }
		let alive = true
		setMeta(null)
		void (async () => {
			try {
				const vo = await AgentService.PreviewFile(path, true)
				if (!alive) return
				setMeta(vo || null)
			} catch {
				if (!alive) return
				setMeta({ path, name: selected?.name || path, kind: 'missing', size: 0, language: '', previewable: false, text: '', rows: [], truncated: false, error: '读取失败' } as FilePreviewVO)
			}
			try {
				const [st] = (await AgentService.PollWorkspacePaths(sessionId, [path])) || []
				if (!alive) return
				setRead(st && st.exists ? { mtimeMs: st.mtimeMs || 0, size: st.size || 0 } : null)
			} catch { if (alive) setRead(null) }
		})()
		return () => { alive = false }
	}, [path, sessionId, nonce, selected?.name])

	// Esc leaves the view. Claimed (preventDefault) so nothing behind it treats the same press as its
	// own dismissal — the chat is inert here but its window-level listeners are alive. A bar that is
	// OPEN has already stopped this event before it gets here (find.tsx runs in the capture phase).
	useEffect(() => {
		const onKey = (e: KeyboardEvent) => {
			if (e.key !== 'Escape' || e.defaultPrevented) return
			e.preventDefault()
			onBack()
		}
		window.addEventListener('keydown', onKey)
		return () => window.removeEventListener('keydown', onKey)
	}, [onBack])

	const current = path ? api.polledState(path) : undefined
	// Two things the poll can tell a reader who is looking at a file: what they are reading has moved
	// on, or it is not there any more. Both are OFFERS (the button re-reads; nothing happens on its
	// own), and both need the fact that the view READ the file — a file nobody read has no baseline to
	// compare against.
	const gone = !!read && !!current && !current.exists
	const moved = !!read && !!current && current.exists &&
		(current.mtimeMs !== read.mtimeMs || current.size !== read.size)
	const note = !meta ? ''
		: meta.kind === 'missing' ? '文件已不存在（可能已被移动或删除）'
			: meta.kind === 'dir' ? '目录本身不是一个可预览的文件'
				: !meta.previewable ? '该类型不支持预览，可用「打开」交给系统应用' : ''
	const target = path || ''

	return (
		<section className="file-view" aria-label="文件">
			<header className="fileview-head">
				<button type="button" className="fileview-back" onClick={onBack} title="返回会话（Esc）">← 返回会话</button>
				<span className="fileview-name" title={target}>{selected?.name || '没有打开的文件'}</span>
				{meta && meta.size > 0 ? <span className="fileview-size">{fmtBytes(meta.size)}</span> : null}
				<span className="fileview-actions">
					<FindButton find={find} />
					{target ? (
						<>
							<button type="button" className="file-btn" title="用系统默认应用打开"
								onClick={() => { AgentService.OpenPath(target).catch(() => {}) }}>打开</button>
							<button type="button" className="file-btn" title="在文件夹中显示"
								onClick={() => { AgentService.RevealPath(target).catch(() => {}) }}>{REVEAL_LABEL}</button>
							<button type="button" className="file-btn" title="复制绝对路径"
								onClick={() => copyText(target)}>复制路径</button>
						</>
					) : null}
				</span>
			</header>
			{/* The path line carries the ROOT LABEL when the file is not in the primary root: the tree's
			    position says which tree a file is in, and this line is a path stripped of the tree (two
			    checkouts of one repository are the case it exists for). The absolute path is the tooltip. */}
			<div className="fileview-path" title={target}>
				{selected ? relPath(selected.root, selected.label, selected.path) : ''}
			</div>
			{/* The stale line is an OFFER, not an action: the content changes only when the reader says
			    so (design §8.4). It replaces nothing — it sits above the text, which is still the text
			    they were reading. */}
			{moved || gone ? (
				<div className="fileview-stale">
					<span>{gone ? '文件已不存在（可能已被移动或删除）' : `文件已更新（${stampTime(current?.mtimeMs || 0)}）`}</span>
					<button type="button" className="fileview-reload" onClick={() => setNonce((n) => n + 1)}>重新读取</button>
				</div>
			) : null}
			<div className="fileview-body" ref={bodyRef}>
				{find.open ? <FindBar find={find} className="is-view" /> : null}
				{!path ? <div className="fileview-empty">在右侧文件树里点一个文件，它就在这里打开</div>
					: !meta ? <div className="fileview-empty">读取中…</div>
						: note ? (
							<div className="fileview-empty">
								<div>{note}</div>
								{meta.error ? <div className="fileview-hint">{meta.error}</div> : null}
							</div>
						) : (
							<FileBody meta={meta} path={path} name={selected?.name || ''} variant="view"
								onExpand={() => setZoom(true)} />
						)}
			</div>
			{zoom && path ? <ImageViewer src={toLocalAsset(path, '')} alt={selected?.name || ''} onClose={() => setZoom(false)} /> : null}
		</section>
	)
}
