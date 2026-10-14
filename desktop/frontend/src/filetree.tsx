// The file tree: the right-hand column that shows what is in the session's workspace, and the way
// into the file view (open a file, read it in the content column).
//
// It is a NAVIGATION column and nothing else — narrow, one row per entry, no editing, no terminal,
// no second reading surface. Everything it can DO with a row is a read: open it with the system's own
// app, reveal it in the folder, copy its path, paste it into the composer as an @-reference. The
// RENDERING of a file happens in the file view (fileview.tsx), deliberately elsewhere: this column's
// width is spent on file names, and a document needs the conversation's width.
//
// Design: docs/2026-10-13-desktop-file-tree-design.md §4.

import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { AgentService, type FileNodeVO, type SessionRootsVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
import { Caret } from './components'
import { onRevealFile } from './filereveal'
import { KIND_ICON } from './filepreview'
import { clampPanelWidth, copyText, fmtBytes, panelRoom } from './lib'
import { REVEAL_LABEL } from './platform'
import type { FileTreeApi, OpenFile } from './useFileTree'

// The column's width and its bounds, mirrored by Go (desktop/filetree.go's fileTreeMinWidth /
// fileTreeMaxWidth), which refuses to store a width outside them: the file is editable by hand, and a
// number the layout cannot honour is worse than the default. The ROOM it may have is the shared
// right-column rule (lib.ts), because the side-channel panel occupies the same edge.
export const FILE_TREE_DEFAULT_WIDTH = 340
export const FILE_TREE_MIN_WIDTH = 240
const FILE_TREE_MAX_WIDTH = 560
const FILE_TREE_KEY_STEP = 24

// TreeRow is one line of the tree, flattened: the roots' own rows and, under each expanded directory,
// its entries. Flattening is what lets the keyboard walk it — "the next visible row" is an index here,
// and a collapsed branch is simply not in the list.
type TreeRow = {
	key: string
	path: string
	name: string
	isDir: boolean
	link: boolean
	size: number
	kind: string
	ignored: string
	error: string
	depth: number
	// root/label are the row's workspace attribution: which root it belongs to, and how that root is
	// named ("" = the primary root). A root row carries its own.
	root: string
	label: string
	// Root-row extras: the directory is gone (removed or unmounted), and where that tree stands in git.
	missing?: boolean
	branch?: string
	detached?: boolean
	// note is the tail line of a layer that hit the backend's cap, or a reason the whole layer could
	// not be read. It is a NOTE, not a row: nothing can be opened on it.
	note?: string
}

// fileIconFor picks a row's glyph from the SAME kind table the attachment card and the file view use,
// so the three surfaces cannot disagree about what a file is. An unknown extension gets the neutral
// document (which is a hint, never a refusal: what a file really is, is settled when it is opened).
function fileIconFor(row: TreeRow): string {
	return KIND_ICON[row.kind] || '📄'
}

export function FileTreePanel({ api, roots, width, onResizeCommit, onOpenFile, onInsertRef, onClose }: {
	api: FileTreeApi
	roots: SessionRootsVO | null
	// The width the reader CHOSE (App owns it: it is persisted). What is shown is that width clamped
	// to the room the window has — see lib.ts's panelRoom.
	width: number
	onResizeCommit: (px: number) => void
	// A row was opened by the reader: select it AND put the content column on the file view.
	onOpenFile: (file: OpenFile) => void
	// Pastes a row into the composer as an @-reference — the one thing only this application can do
	// with a file row, because the reference language is its own.
	onInsertRef: (path: string) => void
	onClose: () => void
}) {
	const { expanded, listing, toggle, dirty, selected, cursor, setCursor, revealRequest, clearReveal, reload, refresh, openFile } = api
	const [menu, setMenu] = useState<{ x: number; y: number; row: TreeRow } | null>(null)
	const [room, setRoom] = useState(() => panelRoom(FILE_TREE_MIN_WIDTH, FILE_TREE_MAX_WIDTH))
	const treeRef = useRef<HTMLDivElement>(null)

	// The panel is its own Esc-free zone (it is a layout choice, like the sidebar) — EXCEPT while its
	// context menu is up, which is a popover and closes on Escape like every other one in the app. The
	// listener only exists while the menu does, so the tree itself never claims the key.
	const menuOpen = !!menu
	useEffect(() => {
		if (!menuOpen) return
		const onKey = (e: KeyboardEvent) => {
			if (e.key !== 'Escape' || e.defaultPrevented) return
			e.preventDefault()
			setMenu(null)
		}
		const onDown = (e: MouseEvent) => {
			if ((e.target as HTMLElement | null)?.closest('.ctx-menu')) return
			setMenu(null)
		}
		window.addEventListener('keydown', onKey)
		document.addEventListener('mousedown', onDown)
		return () => {
			window.removeEventListener('keydown', onKey)
			document.removeEventListener('mousedown', onDown)
		}
	}, [menuOpen])

	// The room is re-measured when the window resizes or the sidebar folds: both change how much of
	// the window the content column actually has.
	useEffect(() => {
		const measure = () => setRoom(panelRoom(FILE_TREE_MIN_WIDTH, FILE_TREE_MAX_WIDTH))
		window.addEventListener('resize', measure)
		const sidebar = document.querySelector('.sidebar')
		const ro = sidebar && typeof ResizeObserver !== 'undefined' ? new ResizeObserver(measure) : null
		if (sidebar && ro) ro.observe(sidebar)
		return () => { window.removeEventListener('resize', measure); ro?.disconnect() }
	}, [])
	const shownWidth = clampPanelWidth(width, room, FILE_TREE_MIN_WIDTH)

	// ── Flattening: the roots, then the entries of everything that is expanded ─
	// The roots are the session's own set (GetSessionRoots), primary first, additional ones in the
	// order they were declared. A root expands like any directory — same gesture, same "fetch this
	// layer" — except that the DEFAULT is the primary open and the additional ones closed: an extra
	// root is often on a mounted volume, and opening the panel must not pay for listing all of them.
	const rows = useMemo<TreeRow[]>(() => {
		const out: TreeRow[] = []
		if (!roots?.primary) return out
		const walk = (entries: FileNodeVO[], depth: number, root: string, label: string) => {
			for (const node of entries || []) {
				out.push({
					key: node.path, path: node.path, name: node.name, isDir: !!node.isDir, link: !!node.link,
					size: node.size || 0, kind: node.kind || 'unknown', ignored: node.ignored || '',
					error: node.error || '', depth, root, label,
				})
				if (!node.isDir || !expanded.has(node.path)) continue
				const sub = listing(node.path)
				// A layer that could not be read says so where its entries would have been: an expanded
				// directory that draws nothing reads as an empty directory, which is a different fact.
				if (sub?.error) {
					out.push({
						key: 'err:' + node.path, path: node.path, name: sub.error, isDir: false, link: false,
						size: 0, kind: 'none', ignored: '', error: '', depth: depth + 1, root, label, note: sub.error,
					})
					continue
				}
				walk(sub?.entries || [], depth + 1, root, label)
				if (sub && !sub.error && (sub.entries || []).length === 0) {
					// 一个展开着、又确实空着的目录：说一句比什么都不画好 —— 不画的话读者分不出
					// "空"和"还没读到"（设计 §4.6）。
					out.push({
						key: 'empty:' + node.path, path: node.path, name: '（空目录）', isDir: false, link: false,
						size: 0, kind: 'none', ignored: '', error: '', depth: depth + 1, root, label, note: 'note',
					})
				}
				if (sub?.truncated) {
					const hidden = Math.max(0, (sub.total || 0) - (sub.entries?.length || 0))
					out.push({
						key: 'tail:' + node.path, path: node.path, name: `另有 ${hidden} 项未显示`, isDir: false,
						link: false, size: 0, kind: 'none', ignored: '', error: '', depth: depth + 1, root, label, note: 'note',
					})
				}
			}
		}
		const rootRow = (path: string, label: string, exists?: boolean, branch?: string, detached?: boolean) => {
			const vo = listing(path)
			out.push({
				key: 'root:' + path, path, name: path.split('/').filter(Boolean).pop() || path,
				isDir: true, link: false, size: 0, kind: 'dir', ignored: '', error: '', depth: 0,
				root: path, label, missing: exists === false, branch, detached,
				// A root that could not be listed (a permission problem, a mount that just went away) says
				// so on its own row, which is also the only place the reader could see it: the row stays.
				note: vo?.error || (vo?.truncated ? `另有 ${Math.max(0, (vo.total || 0) - (vo.entries?.length || 0))} 项未显示` : undefined),
			})
			if (!expanded.has(path) || exists === false) return
			walk(vo?.entries || [], 1, path, label)
		}
		// The primary root's own facts come from the VO's FLAT fields (PrimaryBranch / PrimaryDetached):
		// the branch pair is kept flat there rather than nested, because Primary is a bare path everywhere
		// else that reads this VO — so this row has to pick them up explicitly rather than reading them
		// off the additional-root entry it does not have.
		rootRow(roots.primary, '', undefined, roots.primaryBranch, roots.primaryDetached)
		for (const extra of roots.additional || []) {
			rootRow(extra.path, extra.label || '', extra.exists, extra.branch, extra.detached)
		}
		return out
	}, [roots, expanded, listing])

	// One focusable container with a cursor (aria-activedescendant), rather than one tab stop per row:
	// this is the shape a tree control has, and it means the arrow keys work the moment the reader has
	// clicked anywhere in the column. The cursor is NOT the selection — walking the tree must not open
	// a file per keypress.
	const cursorAt = rows.findIndex((r) => r.key === cursor)
	const focusRow = useCallback((row: TreeRow | undefined) => {
		if (!row) return
		setCursor(row.key)
		window.requestAnimationFrame(() => {
			treeRef.current?.querySelector(`[data-row="${CSS.escape(row.key)}"]`)?.scrollIntoView({ block: 'nearest' })
		})
	}, [setCursor])

	const visible = rows.filter((r) => !r.note || r.isDir)
	const moveCursor = useCallback((delta: number) => {
		const list = visible
		if (!list.length) return
		const at = list.findIndex((r) => r.key === cursor)
		const next = at < 0
			? (delta > 0 ? 0 : list.length - 1)
			: Math.max(0, Math.min(list.length - 1, at + delta))
		focusRow(list[next])
	}, [visible, cursor, focusRow])

	const openRow = (row: TreeRow) => {
		if (row.note && !row.isDir) return
		if (row.isDir) { toggle(row.path); return }
		onOpenFile({ path: row.path, name: row.name, root: row.root, label: row.label })
	}

	const onTreeKey = useCallback((e: React.KeyboardEvent) => {
		const row = rows.find((r) => r.key === cursor)
		if (e.key === 'ArrowDown') { e.preventDefault(); moveCursor(1); return }
		if (e.key === 'ArrowUp') { e.preventDefault(); moveCursor(-1); return }
		if (!row) return
		if (e.key === 'Enter') { e.preventDefault(); openRow(row); return }
		if (e.key === 'ArrowRight' && row.isDir) {
			e.preventDefault()
			if (!expanded.has(row.path)) toggle(row.path)
			return
		}
		if (e.key === 'ArrowLeft') {
			e.preventDefault()
			if (row.isDir && expanded.has(row.path)) { toggle(row.path); return }
			// On a file (or a collapsed directory): ← goes to the parent directory's row.
			const parent = row.path.replace(/\/[^/]+$/, '')
			focusRow(rows.find((r) => r.path === parent))
		}
	}, [rows, cursor, expanded, moveCursor, toggle, focusRow])

	// The reveal request: the ancestors are already expanded by the hook, so this looks for the row
	// and — once the layer it lives in has arrived — selects it. A path that is not in the tree (a file
	// outside every root) simply ends here; the request is cleared either way, so it cannot fire twice.
	const revealedRef = useRef<string | null>(null)
	useEffect(() => {
		if (!revealRequest) return
		const at = rows.find((r) => r.path === revealRequest)
		if (!at) return
		if (revealedRef.current !== revealRequest) {
			revealedRef.current = revealRequest
			focusRow(at)
			// Selection WITHOUT the view switch: the reader opened this file in the overlay they
			// clicked, and the tree's answer is where it lives (see revealPathInTree).
			openFile({ path: at.path, name: at.name }, at.root, at.label)
		}
		clearReveal()
	}, [revealRequest, rows, focusRow, openFile, clearReveal])

	// The module-level channel (see filereveal.ts), registered while this panel is mounted — which is
	// exactly while there is a tree to reveal something in.
	const reveal = api.reveal
	useEffect(() => onRevealFile(reveal), [reveal])

	// ── Resizing ──────────────────────────────────────────────────────────────
	// The same bargain as the side-channel panel's, on the same edge: the drag listens on the WINDOW (a
	// pointer that leaves the handle must keep resizing), the width under the pointer is written
	// straight to the element — one style write per move, never a re-render of the whole tree — and the
	// number goes back to React once, on release, where it is committed and persisted.
	const [dragging, setDragging] = useState(false)
	const panelRef = useRef<HTMLElement | null>(null)
	const dragPxRef = useRef<number | null>(null)
	const dragRef = useRef<{ x: number; w: number } | null>(null)
	const detachRef = useRef<(() => void) | null>(null)
	useEffect(() => () => detachRef.current?.(), [])

	const startDrag = (e: React.PointerEvent<HTMLDivElement>) => {
		e.preventDefault()
		dragRef.current = { x: e.clientX, w: shownWidth }
		dragPxRef.current = shownWidth
		setDragging(true)
		const widthAt = (clientX: number) => {
			const d = dragRef.current
			return d ? clampPanelWidth(d.w + (d.x - clientX), room, FILE_TREE_MIN_WIDTH) : null
		}
		const move = (ev: PointerEvent) => {
			const px = widthAt(ev.clientX)
			if (px === null) return
			dragPxRef.current = px
			if (panelRef.current) panelRef.current.style.width = `${px}px`
		}
		const finish = (ev: PointerEvent) => {
			const px = widthAt(ev.clientX)
			dragRef.current = null
			dragPxRef.current = null
			detach()
			setDragging(false)
			if (px !== null) onResizeCommit(px)
		}
		const detach = () => {
			window.removeEventListener('pointermove', move)
			window.removeEventListener('pointerup', finish)
			window.removeEventListener('pointercancel', finish)
			document.body.classList.remove('is-resizing')
			detachRef.current = null
		}
		window.addEventListener('pointermove', move)
		window.addEventListener('pointerup', finish)
		window.addEventListener('pointercancel', finish)
		document.body.classList.add('is-resizing')
		detachRef.current = detach
	}
	const onHandleKey = (e: React.KeyboardEvent<HTMLDivElement>) => {
		const step = e.key === 'ArrowLeft' ? FILE_TREE_KEY_STEP : e.key === 'ArrowRight' ? -FILE_TREE_KEY_STEP : 0
		if (!step) return
		e.preventDefault()
		onResizeCommit(clampPanelWidth(shownWidth + step, room, FILE_TREE_MIN_WIDTH))
	}

	const menuAct = (row: TreeRow, what: 'open' | 'reveal' | 'copy' | 'ref') => {
		setMenu(null)
		if (what === 'open') { AgentService.OpenPath(row.path).catch(() => {}); return }
		if (what === 'reveal') { AgentService.RevealPath(row.path).catch(() => {}); return }
		if (what === 'copy') { copyText(row.path); return }
		onInsertRef(row.path)
	}

	return (
		<aside className="filetree-panel" aria-label="文件树" ref={panelRef} style={{ width: shownWidth }}>
			<div className={`filetree-resizer${dragging ? ' is-dragging' : ''}`} role="separator"
				aria-orientation="vertical" aria-label="调整文件树宽度" tabIndex={0}
				title="拖动调整宽度（← → 也可以）"
				onPointerDown={startDrag} onKeyDown={onHandleKey} />
			<div className="filetree-head">
				<span className="filetree-title">文件</span>
				<button type="button" className="filetree-icon" title="重新读取已展开的目录" onClick={refresh}>↻</button>
				<button type="button" className="filetree-icon" title="收起文件树" onClick={onClose}>✕</button>
			</div>
			<div className="filetree-body" ref={treeRef} tabIndex={0} role="tree" aria-label="工作区文件"
				aria-activedescendant={cursorAt >= 0 ? `filetree-row-${cursorAt}` : undefined}
				onKeyDown={onTreeKey}>
				{/* 空态分两种，且都不该在"还没读到"的时候出现：没有工作目录（面板指向下方状态栏的 ⌂），
				    以及主目录本身是空的（每一层的"（空目录）"由行自己的 note 说）。 */}
				{!roots?.primary ? (
					<div className="filetree-empty">
						<div>这个会话还没有工作目录</div>
						{/* The way out is the workspace chip in the composer's status row (the panel that
						    owns setting a directory), so the empty state points at it rather than offering a
						    second, native directory picker that would answer differently. */}
						<div className="filetree-hint">在下方状态栏的 ⌂ 里选择工作目录</div>
					</div>
				) : null}
				{roots?.primary && listing(roots.primary) && (listing(roots.primary)?.entries || []).length === 0
					&& !listing(roots.primary)?.error
					? <div className="filetree-empty">（空目录）</div>
					: null}
				{rows.map((row, i) => (
					<TreeLine key={row.key} row={row} id={`filetree-row-${i}`}
						open={row.isDir && expanded.has(row.path)}
						selected={!row.isDir && !!selected && selected.path === row.path}
						cursor={cursor === row.key}
						dirty={dirty.has(row.path)}
						onActivate={() => { setCursor(row.key); openRow(row) }}
						onToggle={() => { setCursor(row.key); toggle(row.path) }}
						onReload={() => reload(row.path)}
						onMenu={(x, y) => { setCursor(row.key); setMenu({ x, y, row }) }}
					/>
				))}
			</div>
			{menu ? (
				<div className="ctx-menu" role="menu" style={{ left: menu.x, top: menu.y }}>
					<button className="ctx-item" role="menuitem" onClick={() => menuAct(menu.row, 'open')}>打开</button>
					<button className="ctx-item" role="menuitem" onClick={() => menuAct(menu.row, 'reveal')}>{REVEAL_LABEL}</button>
					<button className="ctx-item" role="menuitem" onClick={() => menuAct(menu.row, 'copy')}>复制路径</button>
					{/* The insertion is the one gesture only this app can offer: the tree knows the path, the
					    composer knows how to hold a reference, and the @ language is the app's own. */}
					<button className="ctx-item" role="menuitem" title="把这个文件作为引用插入输入框"
						onClick={() => menuAct(menu.row, 'ref')}>插入 @ 引用</button>
				</div>
			) : null}
		</aside>
	)
}

// TreeLine draws one line. It is memoized on the row's own data: the tree can hold several hundred
// rows after two clicks, and a poll that changed one directory must not re-render the other nine.
const TreeLine = memo(function TreeLine({ row, id, open, selected, cursor, dirty, onActivate, onToggle, onReload, onMenu }: {
	row: TreeRow
	id: string
	open: boolean
	selected: boolean
	cursor: boolean
	dirty: boolean
	onActivate: () => void
	onToggle: () => void
	onReload: () => void
	onMenu: (x: number, y: number) => void
}) {
	// A NOTE row (「另有 N 项未显示」, or why a layer could not be read) cannot be opened, selected or
	// acted on: it is the list telling the reader something about itself.
	const isNote = !!row.note && !row.isDir
	const title = [
		row.path,
		row.link ? '符号链接' : '',
		row.ignored ? `被 git 忽略：${row.ignored}` : '',
		row.error ? `读不到：${row.error}` : '',
		row.missing ? '目录不存在（已卸载或被删除）' : '',
	].filter(Boolean).join('\n')
	return (
		<div
			id={id}
			data-row={row.key}
			role="treeitem"
			aria-selected={selected || undefined}
			aria-expanded={row.isDir ? open : undefined}
			className={`filetree-row${selected ? ' is-selected' : ''}${cursor ? ' is-cursor' : ''}` +
				`${row.missing ? ' is-missing' : ''}${row.ignored ? ' is-ignored' : ''}` +
				`${dirty ? ' is-dirty' : ''}${isNote ? ' is-note' : ''}`}
			style={{ paddingLeft: 6 + row.depth * 12 }}
			title={title || undefined}
			onClick={isNote ? undefined : onActivate}
			onContextMenu={isNote ? undefined : (e) => { e.preventDefault(); onMenu(e.clientX, e.clientY) }}>
			{row.isDir ? (
				<span className="filetree-caret" onClick={(e) => { e.stopPropagation(); onToggle() }}>
					<Caret open={open} />
				</span>
			) : <span className="filetree-caret is-none" />}
			<span className="filetree-ico" aria-hidden="true">{isNote ? '…' : fileIconFor(row)}</span>
			<span className="filetree-name">{row.name}</span>
			{row.link ? <span className="filetree-link" title="符号链接">🔗</span> : null}
			{/* The root row's own facts: its label only when it says something the name does not (a base
			    name repeated beside itself is noise; the full path is news), and its git branch (two
			    checkouts of one repository are the same row without it). */}
			{row.depth === 0 && row.label && row.label !== row.name ? <span className="filetree-label">{row.label}</span> : null}
			{row.depth === 0 && (row.branch || row.detached) ? (
				<span className="filetree-branch" title={row.detached ? `HEAD 停在 ${row.branch}` : `分支 ${row.branch}`}>
					{row.detached ? `(${row.branch})` : row.branch}
				</span>
			) : null}
			{row.missing ? <span className="filetree-label">不存在</span> : null}
			{row.error ? <span className="filetree-label" title={row.error}>读不到</span> : null}
			{row.ignored ? <span className="filetree-ignored" title={`被 git 忽略：${row.ignored}`}>忽略</span> : null}
			{!row.isDir && row.size > 0 ? <span className="filetree-size">{fmtBytes(row.size)}</span> : null}
			{dirty ? (
				<button type="button" className="filetree-dirty" title="这个目录刚有变化，点一下重新读取"
					onClick={(e) => { e.stopPropagation(); onReload() }}>●</button>
			) : null}
		</div>
	)
})
