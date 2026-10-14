// The file tree's state: which directories are expanded, what each of them holds, which file is on
// screen, and which of them moved on disk since it was listed.
//
// It is deliberately about ONE session and its workspace, not about the app: the panel that draws it
// (filetree.tsx) holds no data of its own, and the file view reads the selection from here, so the
// tree and the reading pane cannot disagree about which file is open.
//
// Three rules are the whole design, and each one has a reason:
//
//   - LAZY. Nothing below the roots is listed until a directory is expanded, and a layer is fetched
//     once per expand. A repository is tens of thousands of entries; a column is a screenful.
//   - THE MEMO IS THE BACKEND'S. Expansion state and the file being read are remembered per
//     WORKSPACE (the backend derives the key — see desktop/filetree.go), read once when a session
//     becomes the current one, and written back debounced. The frontend never builds a key, so
//     "what counts as the same workspace" has one definition.
//   - POLLING IS A HINT, NOT A GUARANTEE. Every open tree asks the backend what its visible paths
//     look like and re-lists only what actually moved. The reading pane is NEVER rewritten from it:
//     a document that reflows under the reader's eyes is the worst kind of automatic update
//     (design §8.4), so the file view shows 「文件已更新」 and waits to be asked.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { AgentService, type DirListingVO, type PathStateVO, type SessionRootsVO } from '../bindings/github.com/monsterxx03/tachi/desktop'

// FILE_TREE_POLL_MS is how often an OPEN tree asks what moved. Two seconds is fast enough to watch
// an agent work and slow enough that the cost (one stat per expanded directory) is nothing. A covered
// or hidden window has its timers clamped by the browser, so this only slows down — it cannot break.
export const FILE_TREE_POLL_MS = 2000

// MEMO_DEBOUNCE_MS is how long the expansion state settles before it is written. Expanding a few
// directories and opening a file is one gesture in a reader's hands; desktop_ui.json is not worth a
// rewrite per click.
const MEMO_DEBOUNCE_MS = 500

// DIRTY_MARK_MS is how long a directory keeps its 「有变化」 mark after it was re-listed: long enough
// to notice, short enough that it stays a signal rather than becoming a state.
const DIRTY_MARK_MS = 4000

// OpenFile is the file the content column is showing, with everything its header needs to name it
// (the root it belongs to, and how that root is labelled).
export type OpenFile = {
	path: string
	name: string
	// root is the workspace root this file belongs to and label is how that root is named ("" for
	// the session's primary root) — the backend's own rootLabels answer, carried along from the row.
	root: string
	label: string
}

// Stamp is one path's state as a poll saw it.
type Stamp = { exists: boolean; mtimeMs: number; size: number }

export type FileTreeApi = {
	// expanded is the set of directories whose children are being SHOWN. A root is expanded the same
	// way: its top level has been fetched and is on screen.
	expanded: Set<string>
	// listing is one directory's rows, or undefined while they are on their way.
	listing: (dir: string) => DirListingVO | undefined
	toggle: (dir: string) => void
	// dirty holds the directories that changed under the reader, until the mark's timer runs out.
	dirty: Set<string>
	// selected is the file the content column is showing (null while the reader is in the conversation).
	selected: OpenFile | null
	// openFile opens a row in the content column. App switches its own view in the same gesture: the
	// tree owns what is selected, the app owns which column is on screen. The shape is the two fields
	// a row actually has (a path and a name); the rest of a FileNodeVO belongs to the row, not here.
	openFile: (file: { path: string; name: string }, root: string, label: string) => void
	// cursor is the row the keyboard is on, kept apart from the selection: walking the tree with the
	// arrow keys must not open a file per keypress.
	cursor: string | null
	setCursor: (path: string) => void
	// revealRequest is a path the panel has to bring into view (a conversation link, the repeat of an
	// earlier reveal). The panel clears it once the row is on screen.
	revealRequest: string | null
	clearReveal: () => void
	// reveal expands the path's ancestors and asks the panel to select it. It is the way in from the
	// CONVERSATION rather than from a row (see filetree.tsx's revealPathInTree).
	reveal: (path: string) => void
	// reload re-lists ONE directory (a change the poll noticed); refresh re-lists everything expanded
	// (the header's ↻).
	reload: (dir: string) => void
	refresh: () => void
	// polledState is what the last poll said about one path — how the file view decides whether what
	// it is showing is still what is on disk.
	polledState: (path: string) => PathStateVO | undefined
	// memoReady is false until the remembered expansion for this workspace has been applied: writing
	// before that would replace what the reader left with the defaults.
	memoReady: boolean
}

// rootOf resolves which root a path belongs to, with the label the BACKEND gave that root ("" means
// the primary). No rule is re-implemented here: the label is rootLabels' answer, carried by
// GetSessionRoots and by every listing.
export function rootOf(roots: SessionRootsVO | null, path: string): { root: string; label: string } {
	if (!roots || !path) return { root: '', label: '' }
	const inside = (root: string) => !!root && (path === root || path.startsWith(root.replace(/\/+$/, '') + '/'))
	if (inside(roots.primary)) return { root: roots.primary, label: '' }
	for (const extra of roots.additional || []) {
		if (inside(extra.path)) return { root: extra.path, label: extra.label || extra.path.split('/').pop() || extra.path }
	}
	return { root: '', label: '' }
}

// pathChain is the ancestor chain of dir, from the outermost ROOT down to dir itself: exactly the
// directories that have to be expanded for dir to be visible. Pure string work, no stat — a
// remembered deep path may be missing its ancestors, and adding them back is a prefix computation
// (design §5.4).
export function pathChain(roots: SessionRootsVO | null, dir: string): string[] {
	if (!roots || !dir) return []
	const all = [roots.primary, ...(roots.additional || []).map((r) => r.path)].filter(Boolean)
	const root = all.find((r) => dir === r || dir.startsWith(r.replace(/\/+$/, '') + '/'))
	if (!root) return []
	const chain: string[] = [root]
	for (const part of dir.slice(root.length).split('/').filter(Boolean)) {
		chain.push(chain[chain.length - 1].replace(/\/+$/, '') + '/' + part)
	}
	return chain
}

// sameStates compares two poll results field by field. It exists to keep a two-second poll from
// re-rendering a tree that has not changed: an unchanged object means React commits nothing.
function sameStates(a: Record<string, PathStateVO>, b: Record<string, PathStateVO>): boolean {
	const keys = Object.keys(b)
	if (keys.length !== Object.keys(a).length) return false
	for (const key of keys) {
		const x = a[key], y = b[key]
		if (!x || x.exists !== y.exists || x.isDir !== y.isDir || x.size !== y.size || x.mtimeMs !== y.mtimeMs) return false
	}
	return true
}

export function useFileTree({ sessionId, roots, open, fileVisible }: {
	sessionId: string
	roots: SessionRootsVO | null
	// open is the tree COLUMN being on screen; fileVisible is the file view being on screen. Either one
	// is a reason to poll: what the poll answers is displayed in both (rows in the tree, the 「文件已更新」
	// line over the document), and neither being true means nothing on screen could show an answer —
	// which is when the design says to send no calls at all (design §8.1).
	open: boolean
	fileVisible: boolean
}): FileTreeApi {
	const [expanded, setExpanded] = useState<Set<string>>(() => new Set())
	const [listings, setListings] = useState<Record<string, DirListingVO>>({})
	const [dirty, setDirty] = useState<Set<string>>(() => new Set())
	const [selected, setSelected] = useState<OpenFile | null>(null)
	const [cursor, setCursor] = useState<string | null>(null)
	const [states, setStates] = useState<Record<string, PathStateVO>>({})
	const [revealRequest, setRevealRequest] = useState<string | null>(null)
	const [memoReady, setMemoReady] = useState(false)

	// Which session the state above belongs to: a slow answer for the session the reader just left
	// must not land in the tree of the one they are in.
	const ownerRef = useRef(sessionId)
	ownerRef.current = sessionId
	// appliedFor is the session whose remembered shape has been applied. It is a ref rather than a piece
	// of state because it decides nothing that renders — it only stops the memo read from running twice
	// for one session (the effect re-runs whenever the roots arrive, which is exactly once per session).
	const appliedFor = useRef('')
	// The baselines the poll compares against, keyed by path. Refs rather than state: nothing renders
	// them, and re-creating them per change would restart the comparison.
	const stampsRef = useRef<Record<string, Stamp>>({})
	const dirtyTimers = useRef<Record<string, number>>({})
	const memoryTimer = useRef<number | null>(null)
	const pending = useRef<{ sid: string; dirs: string[]; file: string } | null>(null)

	// load fetches one directory. Every listing in the tree comes through here, so they all came from
	// one call with one set of rules.
	const load = useCallback(async (dir: string) => {
		if (!dir) return
		const asked = sessionId
		try {
			const vo = await AgentService.ListWorkspaceDir(asked, dir)
			if (ownerRef.current !== asked) return
			setListings((prev) => ({ ...prev, [dir]: vo || { path: dir, root: '', entries: [], total: 0 } }))
		} catch {
			if (ownerRef.current !== asked) return
			// A backend that cannot answer at all (a simulated frontend, a dead binding) still gets a
			// row: 「读不出来」 is a fact the reader can see, where an eternal spinner is not.
			setListings((prev) => ({ ...prev, [dir]: { path: dir, root: '', entries: [], total: 0, error: '读取目录失败（后端不可达）' } }))
		}
	}, [sessionId])

	const flushMemo = useCallback((sid: string, dirs: string[], file: string) => {
		if (!sid) return
		AgentService.SetFileTreeMemo(sid, dirs, file).catch(() => { /* the next launch just starts fresh */ })
	}, [])

	// A session became the current one: this is a different tree. Everything resets, and the remembered
	// shape of ITS workspace is applied by the effect below — which waits for that session's ROOTS,
	// because the remembered paths are absolute and there is nothing to attribute them to before then.
	useEffect(() => {
		const sid = sessionId
		setExpanded(new Set()); setListings({}); setDirty(new Set()); setStates({}); setCursor(null); setSelected(null)
		stampsRef.current = {}
		appliedFor.current = ''
		setMemoReady(false)
		return () => {
			// Whatever the reader changed in the last half second belongs to the session being left:
			// write it now, or a switch loses it (the debounce timer below is about to be cleared).
			if (memoryTimer.current !== null) { window.clearTimeout(memoryTimer.current); memoryTimer.current = null }
			const p = pending.current
			if (p && p.sid === sid) {
				pending.current = null
				flushMemo(p.sid, p.dirs, p.file)
			}
		}
	}, [sessionId, flushMemo])

	// The remember-shape read: once for each session, and only once its roots are known. The backend
	// decides which WORKSPACE the memo belongs to, so two conversations of one project come back to the
	// same expansion (design §5.4) — while the paths themselves are resolved against THIS session's
	// roots (a project that was re-pointed since simply gets its default expansion back).
	useEffect(() => {
		if (!sessionId || !roots?.primary) return
		if (appliedFor.current === sessionId) return
		appliedFor.current = sessionId
		let alive = true
		AgentService.GetFileTreeMemo(sessionId)
			.then((memo) => {
				if (!alive) return
				setExpanded((prev) => {
					const next = new Set(prev)
					for (const dir of memo?.dirs || []) for (const step of pathChain(roots, dir)) next.add(step)
					return next
				})
				if (memo?.file) {
					const { root, label } = rootOf(roots, memo.file)
					setSelected({ path: memo.file, name: memo.file.split('/').pop() || memo.file, root, label })
				}
				setMemoReady(true)
			})
			.catch(() => { if (alive) setMemoReady(true) })
		return () => { alive = false }
	}, [sessionId, roots])

	// The roots moved (an additional directory added or removed, a project edited): keep the
	// expansion of everything that still exists, drop the listings of what does not, and make sure the
	// primary is open — a workspace whose main tree is collapsed reads as an empty panel. The
	// expansion is by absolute path, so a root that merely MOVED takes its subtree with it.
	useEffect(() => {
		if (!roots?.primary) return
		const all = [roots.primary, ...(roots.additional || []).map((r) => r.path)].filter(Boolean)
		const inside = (p: string) => all.some((root) => p === root || p.startsWith(root.replace(/\/+$/, '') + '/'))
		setExpanded((prev) => {
			const next = new Set<string>()
			for (const dir of prev) if (inside(dir)) next.add(dir)
			next.add(roots.primary)
			return next
		})
		setListings((prev) => {
			const next: Record<string, DirListingVO> = {}
			for (const [dir, vo] of Object.entries(prev)) if (inside(dir)) next[dir] = vo
			return next
		})
		stampsRef.current = {}
	}, [roots])

	// Every expanded directory has a listing: the ONLY place a layer is fetched for the first time,
	// which is what makes restoring a deep remembered path work — pathChain expanded the ancestors,
	// and their listings follow from here.
	useEffect(() => {
		for (const dir of expanded) {
			if (!(dir in listings)) void load(dir)
		}
	}, [expanded, listings, load])

	// ── The remember half ────────────────────────────────────────────────────
	// Debounced on every change, and once more the moment the panel closes.
	useEffect(() => {
		if (!memoReady || !sessionId) return
		pending.current = { sid: sessionId, dirs: [...expanded], file: selected?.path || '' }
		if (memoryTimer.current !== null) window.clearTimeout(memoryTimer.current)
		memoryTimer.current = window.setTimeout(() => {
			memoryTimer.current = null
			const p = pending.current
			if (p) flushMemo(p.sid, p.dirs, p.file)
		}, MEMO_DEBOUNCE_MS)
		return () => {
			if (memoryTimer.current !== null) { window.clearTimeout(memoryTimer.current); memoryTimer.current = null }
		}
	}, [expanded, selected, sessionId, memoReady, flushMemo])

	// The panel closed: write what is there now rather than leaving it to a debounce the reader's next
	// action may never complete (they are closing the column and walking away).
	useEffect(() => {
		if (open) return
		const p = pending.current
		if (!p) return
		if (memoryTimer.current !== null) { window.clearTimeout(memoryTimer.current); memoryTimer.current = null }
		pending.current = null
		flushMemo(p.sid, p.dirs, p.file)
	}, [open, flushMemo])

	// ── The change half ──────────────────────────────────────────────────────
	const markDirty = useCallback((dir: string) => {
		setDirty((prev) => (prev.has(dir) ? prev : new Set(prev).add(dir)))
		if (dirtyTimers.current[dir]) window.clearTimeout(dirtyTimers.current[dir])
		dirtyTimers.current[dir] = window.setTimeout(() => {
			delete dirtyTimers.current[dir]
			setDirty((prev) => {
				if (!prev.has(dir)) return prev
				const next = new Set(prev)
				next.delete(dir)
				return next
			})
		}, DIRTY_MARK_MS)
	}, [])

	useEffect(() => () => {
		for (const t of Object.values(dirtyTimers.current)) window.clearTimeout(t)
		dirtyTimers.current = {}
	}, [])

	// The poll asks about what the READER CAN SEE and nothing else: the expanded directories (their
	// rows are on screen) plus the file the reading pane is showing. A collapsed directory is not
	// polled — its rows are not on screen, and re-listing it is one click away.
	//
	// What it does with an answer is deliberately narrow: a DIRECTORY whose stat moved is re-listed
	// and marked. A file that moved is left alone here, because the pane showing it decides when to
	// re-read (design §8.4) — the poll only publishes the state it saw. The first answer for a path is
	// a BASELINE, not a change: the layer was just listed, and comparing against a stamp nobody took
	// would report a change on every single open.
	useEffect(() => {
		if ((!open && !fileVisible) || !sessionId) return
		let alive = true
		let inFlight = false
		const tick = async () => {
			if (inFlight) return // a slow answer must not stack up behind the next tick
			const paths = [...expanded]
			if (selected?.path) paths.push(selected.path)
			if (!paths.length) return
			inFlight = true
			let results: PathStateVO[] = []
			try {
				results = (await AgentService.PollWorkspacePaths(sessionId, paths)) || []
			} catch {
				inFlight = false
				return // a poll that failed is a poll that runs again in two seconds
			}
			inFlight = false
			if (!alive || ownerRef.current !== sessionId) return
			const next: Record<string, PathStateVO> = {}
			for (const st of results) {
				next[st.path] = st
				const prev = stampsRef.current[st.path]
				stampsRef.current[st.path] = { exists: st.exists, mtimeMs: st.mtimeMs || 0, size: st.size || 0 }
				if (!prev || !expanded.has(st.path)) continue
				if (!st.exists || st.mtimeMs !== prev.mtimeMs || st.size !== prev.size) {
					markDirty(st.path)
					void load(st.path)
				}
			}
			setStates((prev) => (sameStates(prev, next) ? prev : next))
		}
		void tick()
		const timer = window.setInterval(() => void tick(), FILE_TREE_POLL_MS)
		return () => { alive = false; window.clearInterval(timer) }
	}, [open, fileVisible, sessionId, expanded, selected?.path, load, markDirty])

	const toggle = useCallback((dir: string) => {
		setExpanded((prev) => {
			const next = new Set(prev)
			if (next.has(dir)) {
				next.delete(dir)
				return next
			}
			next.add(dir)
			// Opening a directory asks for its CURRENT contents: what was listed before may be from a
			// while ago, and a stale layer is exactly what the reader opened it to check.
			void load(dir)
			return next
		})
	}, [load])

	const reload = useCallback((dir: string) => { void load(dir) }, [load])

	const refresh = useCallback(() => {
		stampsRef.current = {}
		for (const dir of expanded) void load(dir)
	}, [expanded, load])

	const openFile = useCallback((file: { path: string; name: string }, root: string, label: string) => {
		setSelected({ path: file.path, name: file.name, root, label })
		setCursor(file.path)
	}, [])

	const reveal = useCallback((path: string) => {
		if (!path) return
		setExpanded((prev) => {
			const next = new Set(prev)
			for (const step of pathChain(roots, path)) next.add(step)
			return next
		})
		setRevealRequest(path)
	}, [roots])

	const clearReveal = useCallback(() => setRevealRequest(null), [])
	const polledState = useCallback((path: string) => states[path], [states])
	const listing = useCallback((dir: string) => listings[dir], [listings])

	return useMemo(() => ({
		expanded, listing, toggle, dirty, selected, openFile, cursor, setCursor,
		revealRequest, clearReveal, reveal, reload, refresh, polledState, memoReady,
	}), [expanded, listing, toggle, dirty, selected, openFile, cursor, revealRequest, clearReveal, reveal, reload, refresh, polledState, memoReady])
}
