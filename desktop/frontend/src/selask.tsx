// 选中一段文字之后浮出来的小菜单：复制，或者就这段文字问当前会话。
//
// 三个宿主共用它 —— 文件视图（从文件树里打开的文件）、会话里附件卡片展开的预览、会话全文 ——
// 因为这三处摊开的都是可以读的文字，而「选中了想问问」跟这段字是从哪儿来的无关。
//
// 提问不是一条一条发出去的，而是先收进一个**待提问队列**：读一份文档时往往是「这段 + 那段 +
// 那儿」一起问，分三条消息发出去，模型会当成三个不相干的问题分别回答。所以回车是把这一项收进
// 队列，队列上的「发送」才把整队列拼成一条消息（`askQueueMessage`）发进会话。
//
// 五条规矩住在这里，别处不复述：
//
//   - **队列只有一份**（模块级，`useAskQueue` 订阅它）：三个宿主各有自己的选中菜单，但读者心里
//     只有一份「待提问」—— 卡片预览就住在会话全文里面，两处都能选字，而两个队列会变成两个面板
//     叠在同一个角上。面板也因此只画一份（`AskQueuePanel`，挂在 App 上）。
//   - **谁管这一段，看谁在最里面**：卡片预览住在会话全文里面，两个宿主实例都会看见同一次选中，
//     弹两个菜单、发两条消息正是这里的失败方式。往里看，包着这段文字的最内层那个负责。
//   - 选中的那几行是**画**在屏幕上的（CSS Custom Highlight API，与 find.tsx 同一套），不是靠浏览器
//     自己的选区：点「提问」之后焦点进了输入框，浏览器的选区就没了，而读者问的正是屏幕上那几行。
//   - 位置。选区矩形是**视口**坐标，而宿主自己是个滚动容器，所以菜单 portal 到 body 上、用 fixed
//     定位，并在宿主一滚动时就收起来 —— 那一刻这个位置已经指到别处去了。
//   - 发出的那条消息由 `askQueueMessage` 一处组装，而真正把它送进会话的是 App 注册的那个槽：
//     这个文件既不认识「当前会话」是谁，也不该认识。
//
// 队列的生命周期是「承载它的界面」的生命周期：文件视图一关、卡片预览一收，队列跟着清掉
// （不留确认 —— 那个上下文已经离开了）。

import {
	useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore,
	type RefObject,
} from 'react'
import { createPortal } from 'react-dom'
import { copyText, imeActive } from './lib'

// DEFAULT_ASK 是提问框里那句 placeholder，也是空着直接回车时用的问题 —— 点「提问」再回车，
// 就是最常见的那个问法。
export const DEFAULT_ASK = '解释一下？'

// AskBlock 是待提问队列里的一项：选中的那几行字、它出自哪个文件、以及读者要问的问题。
export type AskBlock = { source: string; text: string; question: string }

// askQueueMessage 把整队列拼成一条让模型读的消息。
//
// 分组是这条消息的一半：**同一份文件只写一次来源**，**同一个问题也只写一次** —— 队列里的两段
// 都空着问题时，要的是「两段原文 + 一个解释一下？」，而不是把那句问话重复两遍。组与组之间空一行，
// 一组内部是「原文们（各占一段、引号包着）+ 问题」，顺序按它们在队列里第一次出现的次序。
//
// 来源是给**模型**的：它得知道这段字出自哪个文件，才能自己去读整份文件、看上下文。从对话里选中
// 的那几段没有文件来源（source 为空），那一行就不写 —— 它们本来就在这次对话的历史里。
//
// 原文用引号引起来，是为了让「哪部分是别人写的、哪部分是我问的」一眼分得开。
export function askQueueMessage(items: readonly AskBlock[]): string {
	type Ask = { question: string; texts: string[] }
	type Group = { source: string; asks: Ask[] }
	const groups: Group[] = []
	for (const it of items) {
		const question = it.question.trim() || DEFAULT_ASK
		let group = groups.find((g) => g.source === it.source)
		if (!group) {
			group = { source: it.source, asks: [] }
			groups.push(group)
		}
		let ask = group.asks.find((a) => a.question === question)
		if (!ask) {
			ask = { question, texts: [] }
			group.asks.push(ask)
		}
		ask.texts.push(it.text.trim())
	}
	return groups
		.map((g) => {
			const head = g.source ? `文件：${g.source}\n\n` : ''
			const asks = g.asks
				.map((a) => a.texts.map((t) => `“${t}”`).join('\n\n') + '\n\n' + a.question)
				.join('\n\n')
			return head + asks
		})
		.join('\n\n')
}

// askHandler 是 App 在挂载期间注册的那一个「把它送进当前会话」。
let askHandler: ((text: string) => void) | null = null

// onAskSelection 注册处理器，返回的函数据此注销（与 filereveal.ts 同一个形状）。
export function onAskSelection(fn: (text: string) => void): () => void {
	askHandler = fn
	return () => {
		if (askHandler === fn) askHandler = null
	}
}

// askAboutSelection 把组装好的那条消息交出去；没有人注册时它什么也不做。
export function askAboutSelection(text: string): void {
	if (text) askHandler?.(text)
}

// ── 那一份队列 ─────────────────────────────────────────────────────────────
//
// 一份，跨上下文：文件里选的、卡片预览里选的、对话里选的，都进这同一份（各项自己记着来源，
// 面板按来源分组，所以混着也读得懂）。谁清它只有三种情形，写在 clearAskQueue / dropAskSource
// 和卸载那一段里 —— 没有「切上下文就悄悄清空」这种事。
//
// AskItem 是队列里的一项，id 只用来在编辑/删除时认出它。
type AskItem = AskBlock & { id: number }

let askQueue: AskItem[] = []
const askListeners = new Set<() => void>()
let nextAskID = 1

// setQueue 换一个新数组：useSyncExternalStore 靠引用变化知道该重画。
function setQueue(next: AskItem[]): void {
	askQueue = next
	for (const l of askListeners) l()
}
function updateQueue(fn: (q: AskItem[]) => AskItem[]): void {
	setQueue(fn(askQueue))
}
function subscribeAsk(l: () => void): () => void {
	askListeners.add(l)
	return () => { askListeners.delete(l) }
}
function getQueue(): readonly AskItem[] {
	return askQueue
}
function newAskItem(it: AskBlock): AskItem {
	return { ...it, id: nextAskID++ }
}

// useAskQueue 订阅那份队列（面板跟着它重画）。
export function useAskQueue(): readonly AskItem[] {
	return useSyncExternalStore(subscribeAsk, getQueue, getQueue)
}

// askItemFor 是「当前这次选中」对应的那一项。
//
// 问题空着就是「用那句默认的」—— 存**空串**而不是把默认那句写进去：面板上于是只出现一次那句话
// （作为输入框的 placeholder），而不是每一行都印一遍「解释一下？」。消息拼装时再兜底
// （askQueueMessage）。
function askItemFor(source: string, text: string, prompt: string): AskBlock {
	return { source, text, question: prompt.trim() }
}

// sendAskQueue 把队列（连同 append 进来的这一项）拼成一条消息发出去；队列空着又没 append 时
// 什么也不做。
//
// 面板上的「发送」和 Shift+回车走的是同一条路：Shift+回车不是「绕过队列发送」，而是「加上这一项，
// 然后整队列发出去」—— 队列空着的时候它正好就是「就这一段直接问」。
function sendAskQueue(append?: AskBlock): void {
	const items = append ? [...askQueue, newAskItem(append)] : askQueue
	if (!items.length) return
	const message = askQueueMessage(items)
	setQueue([])
	paintSelection(null)
	askAboutSelection(message)
}

// clearAskQueue 清空整份队列 —— 面板上那个「清空」按钮。清完面板自己就没了。
function clearAskQueue(): void {
	if (askQueue.length) setQueue([])
}

// dropAskSource 扔掉某一份来源的项。用在**关掉那个预览**的时候（文件视图返回会话、卡片预览
// 收起）：那个上下文已经离开了，它那几段跟着走 —— 而会话来源的项（source 为空）不受影响，
// 它们属于会话，人在会话里还看得见。
function dropAskSource(source: string): void {
	if (!source) return
	const kept = askQueue.filter((it) => it.source !== source)
	if (kept.length !== askQueue.length) setQueue(kept)
}

// ── 高亮：这次问的是哪几行 ──────────────────────────────────────────────────
//
// 注册表是进程级的，名字只属于这个菜单。多个宿主实例共用它 —— 谁最后选谁说了算。
const ASK_HIGHLIGHT = 'tachi-ask'

function paintSelection(range: Range | null): void {
	if (typeof CSS === 'undefined' || !CSS.highlights) return
	if (range) CSS.highlights.set(ASK_HIGHLIGHT, new Highlight(range))
	else CSS.highlights.delete(ASK_HIGHLIGHT)
}

// ── 谁管这一段 ─────────────────────────────────────────────────────────────
//
// askHosts 是当前挂着的所有宿主容器。**谁管这一段，看谁在最里面**：附件卡片展开的预览就住在
// 会话全文里面，所以同一次选中会被两个宿主都看见 —— 弹两个菜单、发两条消息，是这里的失败方式。
const askHosts = new Set<HTMLElement>()

function hostFor(target: Node): HTMLElement | null {
	let deepest: HTMLElement | null = null
	for (const el of askHosts) {
		if (!el.contains(target)) continue
		if (!deepest || deepest.contains(el)) deepest = el
	}
	return deepest
}

// Spot 是一次选中：文本，以及它在视口里的落点（末尾那一行的下方）。
type Spot = { text: string; left: number; top: number }

// SelectionAsk 挂在宿主容器上（文件视图的正文、卡片的预览盒、会话全文）。hostRef 指向正文容器：
// 只有落在它里面的选中才算数，而它又是判定「这一段归谁」的那一半（见 hostFor）。source 也决定
// 这个宿主活着时它那几段的归属：`source` 非空的是预览（关掉它，它那几段的待提问跟着走），空的
// 是会话全文（关掉它不带走任何东西 —— 会话来源的项本来就属于会话）。
export function SelectionAsk({ hostRef, source }: {
	hostRef: RefObject<HTMLElement | null>
	source: string
}) {
	const [spot, setSpot] = useState<Spot | null>(null)
	const [asking, setAsking] = useState(false)
	const [prompt, setPrompt] = useState('')
	// pos 是量过菜单自身尺寸之后的落点（贴着视口四边收进来），量之前按选区先画一帧。
	const [pos, setPos] = useState<{ left: number; top: number } | null>(null)
	const boxRef = useRef<HTMLDivElement>(null)
	const inputRef = useRef<HTMLInputElement>(null)
	const rangeRef = useRef<Range | null>(null)

	const close = useCallback(() => {
		setSpot(null)
		setAsking(false)
		setPrompt('')
		setPos(null)
	}, [])

	// 高亮是「这次问的是哪几行」，所以它跟着**那次选中**走：入队（回车）之后留着 —— 那几行已经
	// 在队列里了，读者不必凭记忆确认自己选过哪一段；而一次放弃（Esc、点到别处、滚动、换了文件、
	// 在宿主里点了空白）把它一起撤掉 —— 提问框关了却还留着一块底色，会读成「这段还攥在手里」。
	const clearSelection = useCallback(() => {
		rangeRef.current = null
		paintSelection(null)
	}, [])

	// dismiss 是一次放弃：收起菜单，把那几行也放掉。
	const dismiss = useCallback(() => {
		close()
		clearSelection()
	}, [close, clearSelection])

	// 换了一份文件（文件视图换了行，或卡片换了一张预览）之后，上一次的选中不该还挂在那儿 ——
	// 那几行字属于另一个文件，而菜单上的「提问」会把它当成这一份来问。队列不动：它记着每一项
	// 自己的来源，跨文件一起问正是它存在的理由。
	useEffect(() => { dismiss() }, [source, dismiss])

	// 这个宿主走了（文件视图返回会话、卡片预览收起）：它那一份来源的待提问跟着走（见
	// dropAskSource）。source 从 ref 里读当前值而不是从依赖里 —— 换了文件再关掉视图时，清的
	// 应该是最后看的那个文件，不是历史上每个看过的。
	const sourceRef = useRef(source)
	sourceRef.current = source
	useEffect(() => () => dropAskSource(sourceRef.current), [])

	// 注册自己这一份宿主（卸载时注销）：判定「最内层那个负责」要看得到所有活着的宿主。
	useEffect(() => {
		const el = hostRef.current
		if (!el) return
		askHosts.add(el)
		return () => { askHosts.delete(el) }
	}, [hostRef])

	// 一次选中的结束：拖选和双击都落在 mouseup 上。
	useEffect(() => {
		const host = hostRef.current
		if (!host) return
		const onUp = (e: MouseEvent) => {
			// 点菜单自己（按钮、输入框）或队列面板不算新的一次选中 —— 队列是接着上一次选中做的
			// 事，读者在两者之间来回点是常态（面板由另一个组件画，所以按类找）。
			const target = e.target as Node
			if (boxRef.current?.contains(target) || isAskPanel(target)) return
			// 这一段落在外层的另一个宿主里（会话全文里嵌着附件卡预览就是这种情形）：归那个更内层
			// 的实例管，这里什么都不做 —— 连高亮也不碰（它是共用的一个名字，外层一撤就把内层刚
			// 画好的那片底色抹掉了）。
			if (hostFor(target) !== host) return
			const sel = window.getSelection()
			const range = sel && sel.rangeCount > 0 ? sel.getRangeAt(0) : null
			const text = sel?.toString().trim() || ''
			if (!sel || sel.isCollapsed || !range || !text || !host.contains(range.commonAncestorContainer)) {
				// 没选中（点了一下空白、或把选择取消掉了）：上一次的高亮跟着撤，屏幕上不留一块
				// 指着早已不选了的地方的底色。
				dismiss()
				return
			}
			// 多行选区取**最后一行**的盒子：菜单跟着选区的末尾走，比整个包围盒的底边自然。
			// 换行符自己占的零宽盒子要跳过，否则菜单会落在行尾之外。
			const rects = Array.from(range.getClientRects()).filter((r) => r.width > 0)
			const rect = rects[rects.length - 1] || range.getBoundingClientRect()
			// 选区对象是活的（下一次选中会改它），所以留一份拷贝来画高亮。
			rangeRef.current = range.cloneRange()
			paintSelection(rangeRef.current)
			setSpot({ text, left: rect.left, top: rect.bottom })
			setAsking(false)
			setPrompt('')
		}
		host.addEventListener('mouseup', onUp)
		return () => host.removeEventListener('mouseup', onUp)
	}, [hostRef, dismiss])

	// 组件走了，高亮也跟着撤 —— 它画在文档上，比这块 React 树活得久。
	useEffect(() => () => clearSelection(), [clearSelection])

	// 宿主一滚动，那个位置就指的是别的地方了。
	useEffect(() => {
		if (!spot) return
		const host = hostRef.current
		const drop = () => dismiss()
		host?.addEventListener('scroll', drop, true)
		window.addEventListener('resize', drop)
		return () => {
			host?.removeEventListener('scroll', drop, true)
			window.removeEventListener('resize', drop)
		}
	}, [spot, hostRef, dismiss])

	// 点到别处就收起来。落在宿主里的那次点击会紧接着走一遍上面的 mouseup（要么开一个新的，
	// 要么因为没有选中而收掉）。
	useEffect(() => {
		if (!spot) return
		const onDown = (e: MouseEvent) => {
			const target = e.target as Node
			if (boxRef.current?.contains(target) || isAskPanel(target)) return
			// 这一下点在菜单外面：这次选中到此为止，**连浏览器自己的选区一起清掉**。
			//
			// 少了这一句，紧接着的那次 mouseup 又会把菜单和高亮画回来：点在选中的文字上、点在两行
			// 之间的缝里，浏览器都不会塌陷选区（前者是「等你拖这段字」，后者按包围盒算在选区里），
			// 于是那次 mouseup 读到的仍是「一次有效的选中」。所以这里不猜点在哪，一律结束 ——
			// 想重新选，松开鼠标再拖就是了（拖动会从头产生一个新的选区）。
			window.getSelection()?.removeAllRanges()
			dismiss()
		}
		window.addEventListener('mousedown', onDown, true)
		return () => window.removeEventListener('mousedown', onDown, true)
	}, [spot, dismiss])

	// Esc 认领这枚键：文件视图自己的 Esc 监听也挂在 window 上（`keydown`，非 capture），
	// 所以这里必须压住它，否则收菜单会顺带把人送回会话。defaultPrevented 那一半是给查找条的
	// （find.tsx 也在 capture 阶段）：那一层在菜单上面，不该被这里再关一次。
	useEffect(() => {
		if (!spot) return
		const onKey = (e: KeyboardEvent) => {
			if (e.key !== 'Escape' || e.defaultPrevented) return
			e.preventDefault()
			e.stopPropagation()
			dismiss()
		}
		window.addEventListener('keydown', onKey, true)
		return () => window.removeEventListener('keydown', onKey, true)
	}, [spot, dismiss])

	// 展开输入框之后焦点在框里，可以直接打字。按钮态不抢焦点：它由鼠标点。
	useEffect(() => {
		if (asking) inputRef.current?.focus()
	}, [asking])

	// 量出来再摆正：菜单的实际尺寸取决于当前是按钮态还是输入态，所以 asking 也要重算。
	useLayoutEffect(() => {
		const box = boxRef.current
		if (!spot || !box) return
		const { width, height } = box.getBoundingClientRect()
		const gap = 8
		const top = spot.top + gap + height > window.innerHeight - gap
			? Math.max(gap, spot.top - gap - height)
			: spot.top + gap
		const left = Math.min(Math.max(gap, spot.left), Math.max(gap, window.innerWidth - gap - width))
		setPos({ left, top })
	}, [spot, asking])

	// queueAsk 是一次提问的落地：`send` 是「当场发出去」（Shift+回车），`queue` 是「收进待提问」
	// （回车、那枚按钮）。两条路都在这里收菜单 —— 留着它会挡住下一次选中，它的输入框还会抢走
	// 紧接着的那次回车；而收进队列之后那块底色也撤掉：那几段已经在面板里看得见了，留着反而像
	// 「它还攥在手里」。
	const queueAsk = useCallback((it: AskBlock, how: 'queue' | 'send') => {
		if (how === 'send') {
			sendAskQueue(it)
		} else {
			updateQueue((q) => [...q, newAskItem(it)])
			clearSelection()
		}
		close()
	}, [close, clearSelection])

	if (!spot) return null
	const at = pos || { left: spot.left, top: spot.top + 8 }
	return createPortal(
		<div ref={boxRef} className="sel-ask" style={{ left: at.left, top: at.top }} role="menu"
			// 按住按钮时别让默认动作把选区抹掉 —— 蓝底还在，读者才看得出自己问的是哪一段。
			// 输入框那一半要保住默认动作，否则它拿不到焦点。
			onMouseDown={(e) => { if (!(e.target instanceof HTMLInputElement)) e.preventDefault() }}>
			{asking ? (
				<>
					<input ref={inputRef} className="sel-ask-input" value={prompt} placeholder={DEFAULT_ASK}
						aria-label="就选中的内容提问"
						title="回车加入待提问；Shift+回车直接发出"
						onChange={(e) => setPrompt(e.target.value)}
						onKeyDown={(e) => {
							// 组合态里的回车是中文输入法在确认候选词，不是「提交」——拿它当提交是这类
							// 输入框的经典坑（lib.ts 的 imeActive 就是为它存在的）。
							if (e.key !== 'Enter' || imeActive(e)) return
							e.preventDefault()
							queueAsk(askItemFor(source, spot.text, prompt), e.shiftKey ? 'send' : 'queue')
						}} />
					{/* 「加入待提问」是这条路的入口（跟回车一样）：做成看得见的按钮，不认回车的人也能用。
					    **发出去**只有两条：Shift+回车，以及队列面板上的「发送」—— 一起问的几段要凑齐了
					    再走。 */}
					<button type="button" className="sel-ask-add" title="加入待提问（回车）"
						onClick={() => queueAsk(askItemFor(source, spot.text, prompt), 'queue')}>加入待提问</button>
				</>
			) : (
				<>
					{/* 复制完就收菜单：它是「就这段字做一件事」，事情做完了。 */}
					<button type="button" className="sel-ask-btn"
						onClick={() => { copyText(spot.text); close() }}>复制</button>
					<button type="button" className="sel-ask-btn is-ask" onClick={() => setAsking(true)}>提问</button>
				</>
			)}
		</div>,
		document.body,
	)
}

// isAskPanel 判断一次点击是不是落在待提问面板里（面板由 AskQueuePanel 单独画，所以按类找）。
function isAskPanel(target: Node): boolean {
	const el = target instanceof Element ? target : target.parentElement
	return !!el?.closest('.ask-queue')
}

// AskQueuePanel 是那份队列唯一的渲染者（App 挂一个）。浮在右下角：正文是居中的一栏
// （--reading-width），右下角那块通常是空的，而队列本身不该压在正在读的行上。
//
// 每一项都是「一段选中的字」，按**来源 → 问题**两级分组：同一份文件排在一起、来源只写一次，
// 而同一句问题在面板上只出现一次（默认那句作为输入框的 placeholder 出现，不印在每一行上），
// 它下面的几段原文各占一行、各带一个 ✕。这跟发出去的那条消息（askQueueMessage）是同一套分组
// 规则，所以读者在这里看到的结构就是模型将读到的结构。
export function AskQueuePanel() {
	const queue = useAskQueue()
	// 两级分组：来源 → 问题。**同一个问题只出现一次**（尤其是那句默认的：它作为输入框的
	// placeholder 出现，而不是每一行都印一遍），它下面的几段原文各占一行。这跟发出去的那条消息
	// 是同一套分组规则，所以读者在面板上看到的结构就是模型将读到的结构。顺序按首次出现的次序。
	//
	// 组的键是 `question.trim()`（「空着」与「敲了几个空格」是同一件事），而输入框显示的是组内
	// 首项的原文 —— **显示也用键的话，句尾一按空格就被立刻抹掉**（打不了「一句话 的 空格」）。
	const groups = useMemo(() => {
		const out: { source: string; asks: { key: string; items: AskItem[] }[] }[] = []
		for (const it of queue) {
			let group = out.find((g) => g.source === it.source)
			if (!group) {
				group = { source: it.source, asks: [] }
				out.push(group)
			}
			const key = it.question.trim()
			let ask = group.asks.find((a) => a.key === key)
			if (!ask) {
				ask = { key, items: [] }
				group.asks.push(ask)
			}
			ask.items.push(it)
		}
		return out
	}, [queue])

	if (!queue.length) return null
	return createPortal(
		<div className="ask-queue" role="dialog" aria-label="待提问">
			<div className="ask-q-head">
				<span>待提问</span>
				<span className="ask-q-count">{queue.length} 项</span>
			</div>
			<div className="ask-q-list">
				{groups.map((g) => (
					<div className="ask-q-group" key={g.source || '(chat)'}>
						{/* 来源只写一次（文件名；对话里选中的那几段写「对话」）。整条路径太长，撑不下
						    也没人看 —— 它留在 title 里，而发出去的消息用的是完整的那一份。 */}
						<div className="ask-q-where" title={g.source}>{g.source ? baseName(g.source) : '对话'}</div>
						{g.asks.map((a) => (
							// key 用组内首项的 id，**不是**问题文本：拿文本当 key 的话，读者敲下第一个
							// 字母 key 就变了，React 会把这个输入框卸载重建 —— 屏幕上就是「打一个字光标
							// 就跳出去」（这条由冒烟里「还是同一个 DOM 节点」那条断言钉着）。
							<div className="ask-q-askgroup" key={a.items[0] ? a.items[0].id : a.key}>
								{a.items.map((it) => (
									<div className="ask-q-line" key={it.id}>
										<span className="ask-q-text" title={it.text}>{it.text}</span>
										<button type="button" className="ask-q-del" title="删掉这一段"
											aria-label="删掉这一段"
											onClick={() => updateQueue((q) => q.filter((x) => x.id !== it.id))}>✕</button>
									</div>
								))}
								{/* 一个问题一个输入框：改它，这一组（共用这句话的那几段）一起改 —— 它们本来就
								    是同一个问题。清空就又回到那句默认的。
								    值取组内首项的**原文**（不是分组用的 trim 过的那个）：分组按 trim 后的
								    那句话归组，但输入框要显示读者敲的东西，否则句尾一按空格就被抹掉。 */}
								<input className="ask-q-ask" value={a.items[0] ? a.items[0].question : ''}
									placeholder={DEFAULT_ASK} aria-label="这一组要问的问题"
									onChange={(e) => {
										const next = e.target.value
										const ids = new Set(a.items.map((x) => x.id))
										updateQueue((q) => q.map((x) => (ids.has(x.id) ? { ...x, question: next } : x)))
									}} />
							</div>
						))}
					</div>
				))}
			</div>
			<div className="ask-q-foot">
				{/* 清空之后面板自己就没了（它画的是一份非空的队列）。 */}
				<button type="button" className="ask-q-clear" onClick={() => clearAskQueue()}>清空</button>
				<button type="button" className="ask-q-send" onClick={() => sendAskQueue()}>发送</button>
			</div>
		</div>,
		document.body,
	)
}

// baseName 是面板里那一行的来源标记：整条路径太长，撑不下也没人看 —— 文件名才认得出是哪个文件。
// 完整路径留在 title 里，也让每一项自己记着它（发出去的消息用的是它，不是这个短名字）。
function baseName(path: string): string {
	const cut = path.lastIndexOf('/')
	return cut >= 0 ? path.slice(cut + 1) : path
}
