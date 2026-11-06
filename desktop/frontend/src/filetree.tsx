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
import { AgentService, type FileNodeVO, type GitStatusVO, type SessionRootsVO } from '../bindings/github.com/monsterxx03/tachi/desktop'
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

// GIT_TONE picks the colour a status letter draws in. It lives here rather than in the VO because
// the letter and its words are git's answer (gitstatus.go), while the tone is this palette's: the
// two are allowed to change for different reasons.
const GIT_TONE: Record<string, string> = {
	U: 'is-new', A: 'is-new', M: 'is-mod', D: 'is-del', R: 'is-move', C: 'is-move', '!': 'is-conflict',
}

// CHANGES_ONLY_KEY remembers the tree's 「只看改动」 switch across launches. It lives in
// localStorage rather than desktop_ui.json for the reason the diff layout does (see diff.tsx): this
// is a preference about READING, not geometry — Go never has to honour it before the window exists.
const CHANGES_ONLY_KEY = 'tachi.desktop.fileTreeChangesOnly'

function storedChangesOnly(): boolean {
	try {
		return localStorage.getItem(CHANGES_ONLY_KEY) === '1'
	} catch {
		return false // storage can be blocked; the switch still works for this session
	}
}

function storeChangesOnly(on: boolean) {
	try {
		localStorage.setItem(CHANGES_ONLY_KEY, on ? '1' : '0')
	} catch {
		/* storage blocked: this session still switches, it just will not persist */
	}
}

// insideRoot is the one containment test the change list does by hand. It is the same rule the
// backend applies when it attributes a path to a root (GetWorkspaceGitStatus asks the roots in
// order and the first answer wins), spelled once here so the tree cannot attribute a path
// differently from the table it was handed.
function insideRoot(root: string, path: string): boolean {
	return !!root && (path === root || path.startsWith(root.replace(/\/+$/, '') + '/'))
}

// changeNode is one level of the change list's own hierarchy: the statuses come as absolute PATHS,
// and the directories on them are only implied by those paths. Nothing is listed from disk here —
// a directory exists in this tree because something under it changed, which is the whole point of
// the switch.
type changeNode = { name: string; path: string; dir: boolean; children: Map<string, changeNode> }

// pushChangeRows flattens one root's statuses into rows: directories first, then files, each by
// name, exactly the order a listing gives (entries come back directories-first) so the two modes
// read the same way down.
//
// Collapsed directories are the reader's own doing — in this mode every directory starts OPEN,
// because the list exists to be read, not to be walked.
function pushChangeRows(root: string, label: string, statuses: GitStatusVO[], collapsed: Set<string>, depth: number, out: TreeRow[]) {
	const base = root.replace(/\/+$/, '')
	const head: changeNode = { name: '', path: base, dir: true, children: new Map() }
	for (const st of statuses) {
		const rel = st.path.startsWith(base + '/') ? st.path.slice(base.length + 1) : ''
		if (!rel) continue
		const parts = rel.split('/')
		let node = head
		for (const name of parts.slice(0, -1)) {
			let child = node.children.get(name)
			if (!child) {
				child = { name, path: `${node.path.replace(/\/+$/, '')}/${name}`, dir: true, children: new Map() }
				node.children.set(name, child)
			}
			node = child
		}
		node.children.set(parts[parts.length - 1], { name: parts[parts.length - 1], path: st.path, dir: false, children: new Map() })
	}
	const walk = (node: changeNode, d: number) => {
		const kids = [...node.children.values()].sort((a, b) => (a.dir === b.dir ? a.name.localeCompare(b.name) : a.dir ? -1 : 1))
		for (const k of kids) {
			out.push({
				key: k.path, path: k.path, name: k.name, isDir: k.dir, link: false, size: 0,
				// The status answer carries no kind, and a status is not worth a second listing: the
				// neutral document is what an unknown extension gets in the full tree too.
				kind: k.dir ? 'dir' : 'text', ignored: '', error: '', depth: d, root, label,
			})
			if (k.dir && !collapsed.has(k.path)) walk(k, d + 1)
		}
	}
	walk(head, depth)
}

// dirChangeTitle words a folded directory's count: the total, then the breakdown by what git called
// each one. The labels come from the VO, so this file never spells a status a second time.
function dirChangeTitle(c: { total: number; byLabel: Map<string, number> }): string {
	const parts = [...c.byLabel.entries()]
		.sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
		.map(([label, n]) => `${label} ${n}`)
	return `${c.total} 处未提交的改动：${parts.join('、')}`
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
	const { expanded, listing, toggle, dirty, selected, cursor, setCursor, revealRequest, clearReveal, reload, refresh, openFile, gitStatus, gitStatusReady } = api
	const [menu, setMenu] = useState<{ x: number; y: number; row: TreeRow } | null>(null)
	const [room, setRoom] = useState(() => panelRoom(FILE_TREE_MIN_WIDTH, FILE_TREE_MAX_WIDTH))
	// 只看改动: the tree shows the working tree's uncommitted files instead of the directory listing —
	// the column becomes a change list. Every directory starts open here (see pushChangeRows), so this
	// holds the ones the READER folded, not the ones that are open.
	const [changesOnly, setChangesOnly] = useState(storedChangesOnly)
	const [collapsed, setCollapsed] = useState<Set<string>>(() => new Set())
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
	const listingRows = useMemo<TreeRow[]>(() => {
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

	// ── 只看改动：the same column, listing the working tree's changes instead ──
	// Rows come from the STATUS table (the backend already answers for the whole workspace, not just
	// the layers on screen), so nothing here is listed from disk and nothing needs expanding to be
	// found: a change three directories down is one row, on screen. A root and each directory leading
	// to a change get a row of their own so the tree still reads as a tree, and folding one hides its
	// subtree — the same gesture, a different set of remembered paths (these directories may never
	// have been listed at all).
	const changeRows = useMemo<TreeRow[]>(() => {
		const out: TreeRow[] = []
		if (!roots?.primary) return out
		type owner = { root: string; label: string; branch?: string; detached?: boolean; exists?: boolean; statuses: GitStatusVO[] }
		const owners: owner[] = [{
			root: roots.primary, label: '', branch: roots.primaryBranch, detached: roots.primaryDetached, statuses: [],
		}]
		for (const extra of roots.additional || []) {
			owners.push({
				root: extra.path, label: extra.label || '', branch: extra.branch, detached: extra.detached,
				exists: extra.exists, statuses: [],
			})
		}
		for (const st of Object.values(gitStatus)) {
			const at = owners.find((o) => insideRoot(o.root, st.path))
			if (at) at.statuses.push(st)
		}
		for (const o of owners) {
			// A root with nothing changed is not a section of a change list.
			if (!o.statuses.length || o.exists === false) continue
			out.push({
				key: 'root:' + o.root, path: o.root, name: o.root.split('/').filter(Boolean).pop() || o.root,
				isDir: true, link: false, size: 0, kind: 'dir', ignored: '', error: '', depth: 0,
				root: o.root, label: o.label, branch: o.branch, detached: o.detached,
			})
			if (!collapsed.has(o.root)) pushChangeRows(o.root, o.label, o.statuses, collapsed, 1, out)
		}
		return out
	}, [roots, gitStatus, collapsed])

	const rows = changesOnly ? changeRows : listingRows

	// Whether a directory's children are on screen. The two modes keep different memories: the full
	// tree's (persisted per workspace, see useFileTree) and the change list's (this panel, every
	// directory open unless the reader said otherwise).
	const isOpen = useCallback((path: string, isDir: boolean) => {
		if (!isDir) return false
		return changesOnly ? !collapsed.has(path) : expanded.has(path)
	}, [changesOnly, collapsed, expanded])

	const toggleRow = useCallback((path: string) => {
		if (!changesOnly) { toggle(path); return }
		setCollapsed((prev) => {
			const next = new Set(prev)
			if (next.has(path)) next.delete(path)
			else next.add(path)
			return next
		})
	}, [changesOnly, toggle])

	// ── What a COLLAPSED directory holds ─────────────────────────────────────
	// A folded directory draws no rows under it, so a change inside one would otherwise be invisible
	// until the reader opened every folder to look. It reports the lot in one line instead, derived
	// from the SAME status table the file rows draw (the backend answers for the whole workspace, not
	// just the layers on screen) — and that covers two things a row cannot say even when expanded: a
	// DELETED file has no row at all, and a collapsed root hides its entire tree.
	//
	// Only folded directories get it. An expanded one has its own rows saying it, which is the rule
	// the sidebar's folded groups already follow: when the rows are on screen, they do the talking.
	const dirChanges = useMemo(() => {
		const counts = new Map<string, { total: number; byLabel: Map<string, number> }>()
		const allRoots = [roots?.primary, ...(roots?.additional || []).map((r) => r.path)].filter(Boolean) as string[]
		if (!allRoots.length) return counts
		const inside = (p: string) => allRoots.some((root) => p === root || p.startsWith(root.replace(/\/+$/, '') + '/'))
		for (const st of Object.values(gitStatus)) {
			let dir = st.path.replace(/\/[^/]+$/, '')
			// Walk up until the root that owns the file: above it there is nothing this tree draws.
			while (dir && dir !== '/' && inside(dir)) {
				const cur = counts.get(dir) || { total: 0, byLabel: new Map<string, number>() }
				cur.total++
				cur.byLabel.set(st.label, (cur.byLabel.get(st.label) || 0) + 1)
				counts.set(dir, cur)
				const parent = dir.replace(/\/[^/]+$/, '')
				if (parent === dir) break
				dir = parent
			}
		}
		return counts
	}, [gitStatus, roots])

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
		if (row.isDir) { toggleRow(row.path); return }
		// In the change list a file is a DIFF: the reader turned the column into a change list to
		// review what is uncommitted, so the content column follows the row they clicked. The file
		// view still offers the switch back to the file itself.
		onOpenFile({ path: row.path, name: row.name, root: row.root, label: row.label, mode: changesOnly ? 'diff' : 'preview' })
	}

	const onTreeKey = useCallback((e: React.KeyboardEvent) => {
		const row = rows.find((r) => r.key === cursor)
		if (e.key === 'ArrowDown') { e.preventDefault(); moveCursor(1); return }
		if (e.key === 'ArrowUp') { e.preventDefault(); moveCursor(-1); return }
		if (!row) return
		if (e.key === 'Enter') { e.preventDefault(); openRow(row); return }
		if (e.key === 'ArrowRight' && row.isDir) {
			e.preventDefault()
			if (!isOpen(row.path, true)) toggleRow(row.path)
			return
		}
		if (e.key === 'ArrowLeft') {
			e.preventDefault()
			if (isOpen(row.path, row.isDir)) { toggleRow(row.path); return }
			// On a file (or a collapsed directory): ← goes to the parent directory's row.
			const parent = row.path.replace(/\/[^/]+$/, '')
			focusRow(rows.find((r) => r.path === parent))
		}
	}, [rows, cursor, isOpen, toggleRow, moveCursor, focusRow])

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
				{/* 只看改动：the switch that turns the column into the working tree's change list. It
				    carries its state in aria-pressed and in the accent colour, because a toggle that
				    only looks like a button is one the reader cannot tell is on. */}
				<button type="button" className={`filetree-icon filetree-changes-toggle${changesOnly ? ' is-on' : ''}`}
					aria-pressed={changesOnly}
					title={changesOnly ? '只看改动：开（点一下看全部文件）' : '只看 git 有改动的文件'}
					onClick={() => setChangesOnly((prev) => { storeChangesOnly(!prev); return !prev })}>±</button>
				<button type="button" className="filetree-icon" title="重新读取已展开的目录" onClick={refresh}>↻</button>
				<button type="button" className="filetree-icon" title="收起文件树" onClick={onClose}>✕</button>
			</div>
			<div className="filetree-body" ref={treeRef} tabIndex={0} role="tree" aria-label="工作区文件"
				aria-activedescendant={cursorAt >= 0 ? `filetree-row-${cursorAt}` : undefined}
				onKeyDown={onTreeKey}>
				{/* 空态分两种，且都不该在"还没读到"的时候出现：没有工作目录（面板指向下方状态栏的 ⌂），
				    以及主目录本身是空的（每一层的"（空目录）"由行自己的 note 说）。只看改动时还有第三种：
				    改动列表是空的 —— 而它与"还没问到 git"必须分开说。 */}
				{!roots?.primary ? (
					<div className="filetree-empty">
						<div>这个会话还没有工作目录</div>
						{/* The way out is the workspace chip in the composer's status row (the panel that
						    owns setting a directory), so the empty state points at it rather than offering a
						    second, native directory picker that would answer differently. */}
						<div className="filetree-hint">在下方状态栏的 ⌂ 里选择工作目录</div>
					</div>
				) : null}
				{changesOnly && roots?.primary ? (
					!gitStatusReady ? <div className="filetree-empty">正在读取 git 状态…</div>
						: rows.length ? null : <div className="filetree-empty">工作树里没有未提交的改动</div>
				) : null}
				{!changesOnly && roots?.primary && listing(roots.primary) && (listing(roots.primary)?.entries || []).length === 0
					&& !listing(roots.primary)?.error
					? <div className="filetree-empty">（空目录）</div>
					: null}
				{rows.map((row, i) => {
					// A folded directory speaks for what it hides; an expanded one is spoken for by its
					// own rows (see dirChanges). In the change list both rules still hold — a folded
					// directory there is one the READER folded.
					const hidden = row.isDir && !isOpen(row.path, true) ? dirChanges.get(row.path) : undefined
					return (
						<TreeLine key={row.key} row={row} id={`filetree-row-${i}`}
							open={isOpen(row.path, row.isDir)}
							selected={!row.isDir && !!selected && selected.path === row.path}
							cursor={cursor === row.key}
							dirty={dirty.has(row.path)}
							git={row.isDir ? undefined : gitStatus[row.path]}
							dirGit={hidden ? dirChangeTitle(hidden) : undefined}
							onActivate={() => { setCursor(row.key); openRow(row) }}
							onToggle={() => { setCursor(row.key); toggleRow(row.path) }}
							onReload={() => reload(row.path)}
							onMenu={(x, y) => { setCursor(row.key); setMenu({ x, y, row }) }}
						/>
					)
				})}
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
const TreeLine = memo(function TreeLine({ row, id, open, selected, cursor, dirty, git, dirGit, onActivate, onToggle, onReload, onMenu }: {
	row: TreeRow
	id: string
	open: boolean
	selected: boolean
	cursor: boolean
	dirty: boolean
	git?: GitStatusVO
	// What a FOLDED directory hides, already worded (see dirChanges): a folded row is the only thing on
	// screen about its subtree, so it has to say that there is something in it.
	dirGit?: string
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
		git ? `未提交的改动：${git.label}` : '',
		dirGit || '',
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
			{/* git's answer about this file, right after its name: it is the one piece of metadata
			    a reader scans the column FOR (what did the agent just touch, what did I change), so
			    it sits before the labels that describe the row itself. */}
			{git ? (
				<span className={`filetree-git ${GIT_TONE[git.code] || ''}`}
					title={`未提交的改动：${git.label}`}>{git.code}</span>
			) : null}
			{/* A folded directory: one dot, and its tooltip says how many and of what kind. */}
			{dirGit ? <span className="filetree-git is-dir" title={dirGit} aria-hidden="true" /> : null}
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
