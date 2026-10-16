// selection-ask — 选中一段文字之后浮出来的那个小菜单：复制，或者就这段文字问当前会话。
//
// 提问不是一条一条发出去的：回车（或提问框旁的「加入待提问」）把这一段收进队列，队列面板上的
// 「发送」才把整队列拼成一条消息；只有 Shift+回车是真的当场发。面板和发出去的那条消息都按
// **来源 → 问题**两级分组 —— 同一份文件只写一次来源，同一句问题也只写一次（默认那句只作为
// 输入框的 placeholder 出现，不印在每一行上）。
//
// **队列只有一份**（文件里选的、卡片预览里选的、对话里选的都在里面，各项自己记着来源），而清的
// 时候只有三种：面板上的「清空」、「发送」、以及**关掉那个预览**（文件视图返回会话、卡片收起）——
// 最后那条只带走它自己那份来源的项。这个场景把这个区别钉在两步上：
//   - 第 8 步：关掉卡片预览，NOTE.md 那组走了，对话那组还在；
//   - 第 12 步：会话里攒的一段，打开文件看一眼不会丢（这是「不静默丢东西」那一半）。
//
// 三个宿主各走一遍：附件卡片展开的预览（它住在会话全文**里面**，所以「哪一层管这一段」在这里被
// 钉住）、文件视图（四段入队、改一组、删一段、发送）、会话全文（Shift+回车那条捷径）。
//
// 合成的手法：driver 摆不出一次真的拖选，所以用 Range 选中一个段落，然后发一次真的 mouseup
// （选中的文本是在那里被记下来的）。够不到的那一半（按下、移动）不是这个场景要断的契约：
// 「松开时有非空选中 → 菜单」才是。段落按**内容**找，不按位置 —— 段落顺序是 markdown 的事。
;(async () => {
  const LINE_A = '这段文字是拿来被选中提问的。'
  const LINE_B = '第二段也有它自己的问题。'
  const LINE_C = '第三段用自己写的问题。'
  const LINE_D = '第四段入队之后会被删掉。'
  const ASK_C = '这一段是干什么的？'
  const ASK_C2 = '这一段到底是干什么的（改过）？'
  // 会话全文那一半：被选中的两句 agent 回答。
  const CHAT_LINE1 = '就这一段的回答。'
  const CHAT_LINE2 = '旁边这句也能选。'
  const CHAT_ASK = '这句是什么意思？'
  const DEFAULT_ASK = '解释一下？'

  // selectIn 选中一个元素里的全部文字，然后松手 —— 读者那一下的等价物。
  const selectIn = (el) => {
    const range = document.createRange()
    range.selectNodeContents(el)
    const sel = window.getSelection()
    sel.removeAllRanges()
    sel.addRange(range)
    el.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }))
  }
  // paraIn 是宿主正文里含那句话的那一段。
  const paraIn = (sel, line) => smoke.qa(sel).find((p) => smoke.text(p).indexOf(line) >= 0) || null
  // qRow 是队列里记着某一段的那一行（按内容找，不按位置）。
  const qRow = (line) => smoke.qa('.ask-q-line').find((it) => {
    const t = it.querySelector('.ask-q-text')
    return !!t && smoke.text(t).indexOf(line) >= 0
  }) || null
  // askOf 是那一行所属问题组里的输入框（一个问题一个框，同组的几段共用它）。
  const askOf = (line) => {
    const row = qRow(line)
    return row ? row.parentElement.querySelector('.ask-q-ask') : null
  }
  const esc = () => document.body.dispatchEvent(
    new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
  // askPainted 是「选中被画住了」那块底色（CSS Custom Highlight API，与 ⌘F 同一套）。
  const askPainted = () => (window.CSS && CSS.highlights ? (CSS.highlights.get('tachi-ask') || { size: 0 }).size : -1)
  // countOf 数一句话在正文里出现了几次 —— 「来源只写一次」就是它数出来的。
  const countOf = (s, sub) => s.split(sub).length - 1
  const waitLines = (n) => smoke.waitFor(
    () => (smoke.qa('.ask-q-line').length === n ? true : null), '队列里 ' + n + ' 段', 3000)
  const cardButton = (label) => smoke.qa('.file-card .file-btn').find((b) => b.textContent === label) || null
  const users = () => smoke.qa('.msg-user .msg-content').length
  // enter 是「交出去」的那一下：回车收进队列，Shift+回车当场发。
  const enter = (field, shift) => smoke.key(field, 'Enter', shift ? { shiftKey: true } : undefined)
  // askAbout 一遍读完一次手势：选中 → 点「提问」→（可选）填问题。返回那个输入框。
  const askAbout = async (hostSel, line, question) => {
    const p = paraIn(hostSel, line)
    if (!p) return null
    selectIn(p)
    const menu = await smoke.waitFor('.sel-ask', '选中「' + line.slice(0, 6) + '…」浮出菜单', 3000)
    if (!menu) return null
    smoke.click('.sel-ask-btn.is-ask')
    const field = await smoke.waitFor('.sel-ask-input', '提问输入框', 3000)
    if (!field) return null
    if (question) smoke.type(field, question)
    return field
  }

  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // ── 宿主一：会话里附件卡片展开的预览 ───────────────────────────────────────
  smoke.type('.composer-input', '把 NOTE.md 发给我')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant', '已经把', 30000))) return smoke.finish()
  if (!(await smoke.waitFor('.msg-assistant .turn-files .file-card', '附件卡出现', 15000))) return smoke.finish()
  const preview = await smoke.waitFor(() => cardButton('预览'), '卡片上有「预览」', 8000)
  if (!preview) return smoke.finish()
  preview.click()
  if (!(await smoke.waitFor('.file-preview-doc', '卡片预览展开', 8000))) return smoke.finish()

  // ── 菜单长什么样（钉一次就够），以及它归谁管 ────────────────────────────────
  // 卡片预览住在会话全文里面，两个宿主都会看见同一次选中 —— 弹两个菜单、发两条消息就是这里的
  // 失败方式。断言「屏幕上只有一个菜单」正是钉住「最内层那个负责」这条规则。
  const probe = paraIn('.file-preview-doc p', LINE_A)
  if (!probe) return smoke.fail('卡片预览里有第一段', '(找不到那一段)')
  selectIn(probe)
  const menu = await smoke.waitFor('.sel-ask', '选中之后浮出菜单', 3000)
  smoke.check('选中一段文字就浮出菜单（不用点任何东西）', !!menu, '')
  smoke.check('就一个菜单（卡片预览嵌在会话全文里，里层那个负责）',
    smoke.qa('.sel-ask').length === 1, smoke.qa('.sel-ask').length + ' 个')
  smoke.check('菜单上就两项：复制、提问',
    smoke.allText('.sel-ask-btn').join('/') === '复制/提问', smoke.allText('.sel-ask-btn').join('/'))
  smoke.check('菜单落在视口里（贴着选中文字，不跑到屏幕外）',
    !!menu && menu.getBoundingClientRect().top >= 0 &&
      menu.getBoundingClientRect().bottom <= window.innerHeight,
    menu ? JSON.stringify(menu.getBoundingClientRect()) : '')
  esc()
  if (!(await smoke.waitFor(() => (!smoke.q('.sel-ask') ? true : null), 'Esc 收起菜单', 3000))) return smoke.finish()

  // ── 点在选中的那几行里：这次选中被放掉 ──────────────────────────────────────
  // 浏览器默认会留着选区等你拖它，所以那次 mouseup 又把菜单和高亮画了回来 —— 屏幕上就是「点了
  // 一下，什么都没变」。这条断言钉的是「点自身也算取消」。合成的 mousedown 不会改浏览器选区，
  // 所以它证明的正是「由我们主动结束掉」这件事。
  selectIn(probe)
  if (!(await smoke.waitFor('.sel-ask', '再选一次，菜单回来', 3000))) return smoke.finish()
  smoke.check('点自身之前，那几行是亮着的', askPainted() >= 1, String(askPainted()))
  const selRect = (() => {
    const sel = window.getSelection()
    const rect = sel && sel.rangeCount > 0 ? sel.getRangeAt(0).getClientRects()[0] : null
    return rect ? { x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 } : null
  })()
  smoke.check('拿得到选中区域的坐标（下面两条断言的前提）', !!selRect, '')
  if (!selRect) return smoke.finish()
  const hit = (type) => probe.dispatchEvent(new MouseEvent(type,
    { bubbles: true, clientX: selRect.x, clientY: selRect.y }))
  hit('mousedown')
  hit('mouseup')
  // 菜单是被 React 的状态更新摘掉的，所以要等一次提交 —— 同步读会读到还没重画的那一帧。
  const selfGone = await smoke.waitFor(() => (!smoke.q('.sel-ask') ? true : null),
    '点自身之后菜单消失', 3000)
  smoke.check('点在选中的那几行里，菜单收起', !!selfGone, '')
  smoke.check('点在选中的那几行里，高亮也撤了（不用点别处）',
    askPainted() === 0, String(askPainted()))

  // ── 点在两行之间的缝里：同样算「放掉这次选中」 ──────────────────────────────
  // 缝既不在任何一个字的矩形里，读者的手也未必落在字上 —— 所以「点在哪」不该决定这次选中留不留。
  // 这里造一个跨两段的选区，两个矩形之间就是那条缝。
  const seam = (() => {
    const first = paraIn('.file-preview-doc p', LINE_A)
    const second = paraIn('.file-preview-doc p', LINE_B)
    if (!first || !second || !first.firstChild || !second.firstChild) return null
    const range = document.createRange()
    range.setStart(first.firstChild, 0)
    range.setEnd(second.firstChild, second.firstChild.length)
    const sel = window.getSelection()
    sel.removeAllRanges()
    sel.addRange(range)
    const rects = Array.from(range.getClientRects()).filter((r) => r.width > 0)
    if (rects.length < 2) return null
    return { x: rects[0].left + 4, y: (rects[0].bottom + rects[1].top) / 2, host: first.parentElement }
  })()
  smoke.check('造得出一个「两行之间的缝」（下面两条断言的前提）', !!seam, '')
  if (!seam) return smoke.finish()
  seam.host.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }))
  if (!(await smoke.waitFor('.sel-ask', '跨两行的选区也弹菜单', 3000))) return smoke.finish()
  const seamHit = (type) => seam.host.dispatchEvent(new MouseEvent(type,
    { bubbles: true, clientX: seam.x, clientY: seam.y }))
  seamHit('mousedown')
  seamHit('mouseup')
  const seamGone = await smoke.waitFor(() => (!smoke.q('.sel-ask') ? true : null),
    '点缝里之后菜单消失', 3000)
  smoke.check('点在两行之间的缝里，菜单收起', !!seamGone, '')
  smoke.check('点在两行之间的缝里，高亮也撤了', askPainted() === 0, String(askPainted()))

  // ── 放弃：点别处关掉提问框，那几行也该放下 ──────────────────────────────────
  // 提问框关了却还留着一块底色，会读成「这段还攥在手里」。driver 够不到真的按下-松开，所以派发
  // 一次真的 mousedown（dismiss 就挂在它上面；合成的 el.click() 不产生 mousedown，见
  // docs/agents/desktop.md 那条）。
  const asked = await askAbout('.file-preview-doc p', LINE_A, '')
  if (!asked) return smoke.finish()
  smoke.check('提问态里那几行亮着', askPainted() >= 1, String(askPainted()))
  smoke.q('.file-card .file-name').dispatchEvent(new MouseEvent('mousedown', { bubbles: true }))
  const closedAsk = await smoke.waitFor(() => (!smoke.q('.sel-ask') ? true : null), '点别处关掉提问框', 3000)
  smoke.check('点别处关掉提问框', !!closedAsk, '')
  smoke.check('放弃之后那块底色也撤了（选中不再「攥在手里」）', askPainted() === 0, String(askPainted()))
  smoke.check('放弃之后没有东西进队列', !smoke.q('.ask-queue'), '')

  // ── 提问框：placeholder、焦点、旁边那枚「加入待提问」 ───────────────────────
  const first = await askAbout('.file-preview-doc p', LINE_A, '')
  if (!first) return smoke.finish()
  smoke.check('输入框的 placeholder 是「解释一下？」', first.placeholder === DEFAULT_ASK, first.placeholder)
  smoke.check('输入框拿到焦点（回车就能收进队列）', document.activeElement === first, '')
  smoke.check('提问框旁边那枚按钮是「加入待提问」（不是直接发出去）',
    !!smoke.q('.sel-ask-add') && smoke.text(smoke.q('.sel-ask-add')) === '加入待提问',
    smoke.allText('.sel-ask-add').join(' | '))
  // 「点提问之后选中不要取消」：焦点进了输入框，浏览器的选区就没了，所以那几行由我们自己画住
  // （::highlight(tachi-ask)）。这一条钉的就是它 —— 屏幕上那块底色。
  smoke.check('点「提问」之后选中的那几行还亮着', askPainted() >= 1, String(askPainted()))

  // 中文输入法：组合态里的回车是在确认候选词，不是「提交」。这一条是**负断言** —— 要等的是
  // 一件不该发生的事，所以除了让出几帧没有别的可等（同 docs/agents/desktop.md 里那条固定等待）。
  const usersBefore = users()
  first.dispatchEvent(new KeyboardEvent('keydown',
    { key: 'Enter', isComposing: true, bubbles: true, cancelable: true }))
  await smoke.sleep(150)
  smoke.check('输入法组合态里的回车不会把这一项收进队列（那是在选词）',
    !!smoke.q('.sel-ask-input') && users() === usersBefore, users() + ' 条用户消息（之前 ' + usersBefore + ' 条）')

  // 回车（问题空着 → 用那句默认的）＝ 收进队列，不是发出去。
  enter(first)
  if (!(await waitLines(1))) return smoke.finish()
  smoke.check('回车是收进队列，不是发出去', users() === usersBefore, users() + ' 条用户消息')
  smoke.check('队列那一项记着它来自哪个文件',
    !!qRow(LINE_A) && smoke.text(smoke.q('.ask-q-group .ask-q-where')) === 'NOTE.md',
    smoke.allText('.ask-q-where').join(' | '))
  smoke.check('空着问题的那一项，面板上没有把「解释一下？」印出来（它只是 placeholder）',
    smoke.text('.ask-q-list').indexOf(DEFAULT_ASK) < 0 &&
      !!askOf(LINE_A) && askOf(LINE_A).placeholder === DEFAULT_ASK,
    smoke.text('.ask-q-list'))
  smoke.check('收进队列之后菜单收起', !smoke.q('.sel-ask'), '')
  smoke.check('收进队列之后那块底色撤了（面板里已经看得见那几段）', askPainted() === 0, String(askPainted()))

  // ── 「清空」：清完面板自己就没了 ───────────────────────────────────────────
  smoke.click('.ask-q-clear')
  const cleared = await smoke.waitFor(() => (!smoke.q('.ask-queue') ? true : null), '点「清空」之后面板消失', 3000)
  smoke.check('点「清空」把队列清掉（面板跟着消失）', !!cleared, '')

  // ── 提问框旁那枚按钮＝加入待提问，跟回车同一条路 ────────────────────────────
  const buttonAsk = await askAbout('.file-preview-doc p', LINE_A, '')
  if (!buttonAsk) return smoke.finish()
  smoke.click('.sel-ask-add')
  const added = await smoke.waitFor(() => (smoke.qa('.ask-q-line').length === 1 ? true : null),
    '点「加入待提问」之后队列里有一段', 3000)
  smoke.check('点「加入待提问」把这一段收进队列', !!added, smoke.qa('.ask-q-line').length + ' 段')
  smoke.check('点「加入待提问」不会把它发出去', users() === usersBefore, users() + ' 条用户消息')
  smoke.check('点「加入待提问」之后菜单收起', !smoke.q('.sel-ask'), '')

  // ── 一份队列：预览开着时从会话里选字，也进这同一份（按来源分组） ─────────────
  const chatA = await askAbout('.msg-assistant .msg-content p', CHAT_LINE1, '')
  if (!chatA) return smoke.finish()
  enter(chatA)
  if (!(await waitLines(2))) return smoke.finish()
  smoke.check('预览开着时从会话里选的也进同一份队列（一份队列、一个面板）',
    smoke.qa('.ask-queue').length === 1, smoke.qa('.ask-queue').length + ' 个面板')
  smoke.check('面板按来源分成两组（NOTE.md 与 对话）',
    smoke.qa('.ask-q-group').length === 2 &&
      smoke.allText('.ask-q-where').join('|') === 'NOTE.md|对话',
    smoke.qa('.ask-q-group').length + ' 组 / ' + smoke.allText('.ask-q-where').join(' | '))

  // ── 关掉预览：只带走它那份来源的项 ─────────────────────────────────────────
  // 这是「一份队列」里唯一一条清空规则：关掉产生它的那个预览，它那几段跟着走 —— 而对话里攒的
  // 不受影响（它属于会话，人在会话里还看得见）。
  const collapse = cardButton('收起')
  if (!collapse) return smoke.fail('卡片上有「收起」', smoke.allText('.file-card .file-btn').join(' | '))
  collapse.click()
  const onlyChat = await smoke.waitFor(() => (smoke.qa('.ask-q-line').length === 1 ? true : null),
    '关掉预览之后只剩会话那一组', 3000)
  smoke.check('关掉预览带走它那份来源的项', !!onlyChat, smoke.qa('.ask-q-line').length + ' 段')
  smoke.check('对话里攒的那一段没被关预览带走',
    !!qRow(CHAT_LINE1) && !qRow(LINE_A) &&
      smoke.text(smoke.q('.ask-q-group .ask-q-where')) === '对话',
    smoke.allText('.ask-q-where').join(' | '))
  smoke.check('关掉预览也撤了那块底色', askPainted() === 0, String(askPainted()))
  smoke.click('.ask-q-clear')
  if (!(await smoke.waitFor(() => (!smoke.q('.ask-queue') ? true : null), '把剩下那段清掉', 3000))) return smoke.finish()

  // ── 会话全文：Shift+回车是那条捷径（真的当场发） ────────────────────────────
  const chatShortcut = await askAbout('.msg-assistant .msg-content p', CHAT_LINE1, CHAT_ASK)
  if (!chatShortcut) return smoke.finish()
  enter(chatShortcut, true)
  const chatSent = await smoke.waitFor(
    () => smoke.qa('.msg-user .msg-content').find((m) => smoke.text(m).indexOf(CHAT_ASK) >= 0) || null,
    'Shift+回车那条进了会话', 5000)
  smoke.check('Shift+回车直接把这一段问了出去', !!chatSent, chatSent ? smoke.text(chatSent) : '(没有这条消息)')
  smoke.check('Shift+回车不经过队列（队列里没有东西停下来）', !smoke.q('.ask-queue'), '')
  smoke.check('从对话里选的那一条不带文件来源（它是从对话里选的）',
    !!chatSent && smoke.text(chatSent).indexOf('文件：') < 0, chatSent ? smoke.text(chatSent) : '')
  if (!(await smoke.waitText('.msg-assistant', '对话里那一段的回答', 20000))) return smoke.finish()

  // ── 在会话里攒一段，再打开文件看一眼：它不该丢 ──────────────────────────────
  const mine = await askAbout('.msg-assistant .msg-content p', CHAT_LINE2, '')
  if (!mine) return smoke.finish()
  enter(mine)
  if (!(await waitLines(1))) return smoke.finish()
  smoke.key(document.body, 'e', { metaKey: true })
  if (!(await smoke.waitFor('.filetree-panel', '文件树面板打开（⌘E）', 8000))) return smoke.finish()
  // 按名字找那一行：树的顺序是排序规则给的答案，不是这个场景可以说的事。
  const row = await smoke.waitFor(
    () => smoke.qa('.filetree-row').find((r) => smoke.text(r.querySelector('.filetree-name')) === 'NOTE.md') || null,
    '树里有 NOTE.md 这一行', 8000)
  if (!row) return smoke.finish()
  row.click()
  if (!(await smoke.waitFor('.file-view', '文件在内容列打开（文件视图）', 8000))) return smoke.finish()
  smoke.check('打开一个文件看一眼，会话里攒的那一段还在（不会静默丢东西）',
    !!qRow(CHAT_LINE2), smoke.allText('.ask-q-text').join(' | '))
  smoke.click('.ask-q-clear')
  if (!(await smoke.waitFor(() => (!smoke.q('.ask-queue') ? true : null), '清掉那一段', 3000))) return smoke.finish()

  // ── 宿主二：文件视图（内容列）：四段一起问，来源与默认问题都只写一次 ──────────
  // 第一段：空着回车（问题就是那句默认的）。
  const a = await askAbout('.fileview-doc p', LINE_A, '')
  if (!a) return smoke.finish()
  enter(a)
  if (!(await waitLines(1))) return smoke.finish()

  // Esc 归菜单自己：文件视图的 Esc 是「返回会话」，菜单开着时那一下只该收菜单 —— 而手里刚选好
  // 的那一段不能因为按了 Esc 就丢（队列里的那一项还在）。
  selectIn(paraIn('.fileview-doc p', LINE_B))
  if (!(await smoke.waitFor('.sel-ask', '再选一段，菜单回来', 3000))) return smoke.finish()
  esc()
  const menuGone = await smoke.waitFor(() => (!smoke.q('.sel-ask') ? true : null), 'Esc 收起菜单', 3000)
  smoke.check('Esc 收起菜单', !!menuGone, '')
  smoke.check('Esc 没有把人从文件里带走（文件视图还在）', !!smoke.q('.file-view'), '')
  smoke.check('Esc 之后队列没受影响', smoke.qa('.ask-q-line').length === 1, smoke.qa('.ask-q-line').length + ' 段')

  // 第二段：也空着回车（跟第一段同一句话问）—— 两句同一个问题，面板上只该有一个输入框。
  const b = await askAbout('.fileview-doc p', LINE_B, '')
  if (!b) return smoke.finish()
  enter(b)
  if (!(await waitLines(2))) return smoke.finish()
  smoke.check('同一份文件的几段归在一个来源组里（来源只写一次）',
    smoke.qa('.ask-q-group').length === 1 &&
      smoke.text(smoke.q('.ask-q-group .ask-q-where')) === 'NOTE.md',
    smoke.qa('.ask-q-group').length + ' 个来源组 / ' + smoke.allText('.ask-q-where').join(' | '))
  smoke.check('两句同一个默认问题合成一组（面板上只有一个输入框）',
    smoke.qa('.ask-q-askgroup').length === 1 && smoke.qa('.ask-q-ask').length === 1,
    smoke.qa('.ask-q-askgroup').length + ' 个问题组 / ' + smoke.qa('.ask-q-ask').length + ' 个输入框')

  // 第三段：写自己的问题（一会儿再改掉）。
  const c = await askAbout('.fileview-doc p', LINE_C, ASK_C)
  if (!c) return smoke.finish()
  enter(c)
  if (!(await waitLines(3))) return smoke.finish()
  smoke.check('换了一个问题就成了另一个组（两句话、两个输入框）',
    smoke.qa('.ask-q-askgroup').length === 2 && smoke.qa('.ask-q-ask').length === 2,
    smoke.qa('.ask-q-askgroup').length + ' 个问题组')

  // 第四段：也是默认问题，一会儿删掉。
  const d = await askAbout('.fileview-doc p', LINE_D, '')
  if (!d) return smoke.finish()
  enter(d)
  if (!(await waitLines(4))) return smoke.finish()
  smoke.check('空着问题的那一段并进了那句默认的组（还是两个输入框）',
    smoke.qa('.ask-q-askgroup').length === 2 && smoke.qa('.ask-q-ask').length === 2,
    smoke.qa('.ask-q-askgroup').length + ' 个问题组')

  // 一组的问题是可以改的（改的是共用这句话的那几段）。**逐次**敲，中途还要断言那个输入框还是
  // 同一个 DOM 节点：一个会变的 key（比如拿问题文本当 key）会让 React 把它卸载重建 —— 屏幕上
  // 看到的就是「打一个字母光标就跳出去」（driver 够不到真的键盘焦点，节点身份是同一件事的机制）。
  const askC = askOf(LINE_C)
  smoke.check('队列里一行一段（每行一个问题组的输入框）', !!askC, '')
  if (!askC) return smoke.finish()
  smoke.type(askC, ASK_C2.slice(0, 2))
  smoke.check('打了两个字，那个输入框还是同一个节点（光标不会被甩出去）',
    askOf(LINE_C) === askC, '')
  // 空格要留得住：分组按 trim 后的那句话归组，但输入框显示的必须是读者敲的原文 —— 否则句尾
  // 一按空格就被抹掉，写不了句子（下面这行断言就是这个）。
  smoke.type(askC, '两 个 词 ')
  smoke.check('编辑框里的空格留得住（含句尾那个）',
    !!askOf(LINE_C) && askOf(LINE_C).value === '两 个 词 ',
    askOf(LINE_C) ? JSON.stringify(askOf(LINE_C).value) : '(没有输入框)')
  smoke.type(askC, ASK_C2)
  const edited = await smoke.waitFor(
    () => (askOf(LINE_C) && askOf(LINE_C).value === ASK_C2 ? true : null), '改掉那一组的问题', 3000)
  smoke.check('队列里的一组可以改问题', !!edited, askOf(LINE_C) ? askOf(LINE_C).value : '')

  // 删掉一段。
  const rowD = qRow(LINE_D)
  if (!rowD) return smoke.finish()
  smoke.click(rowD.querySelector('.ask-q-del'))
  const droppedD = await smoke.waitFor(() => (smoke.qa('.ask-q-line').length === 3 ? true : null),
    '删掉那一段', 3000)
  smoke.check('队列里的一段可以删掉', !!droppedD, smoke.qa('.ask-q-line').length + ' 段')
  smoke.check('删掉的那一段确实不在了',
    !!qRow(LINE_A) && !!qRow(LINE_B) && !!qRow(LINE_C) && !qRow(LINE_D),
    smoke.allText('.ask-q-text').join(' | '))

  // 一次发送：整队列拼成一条消息，然后人回到会话（回答在那儿）。
  smoke.click('.ask-q-send')
  const back = await smoke.waitFor(() => (!smoke.q('.file-view') ? true : null), '发送之后回到会话', 5000)
  smoke.check('发送之后自动回到会话（回答在那儿）', !!back, '')
  smoke.check('发送之后队列空了', !smoke.q('.ask-queue'), '')
  const both = await smoke.waitFor(
    () => smoke.qa('.msg-user .msg-content').find(
      (m) => smoke.text(m).indexOf(LINE_A) >= 0 && smoke.text(m).indexOf(LINE_B) >= 0) || null,
    '几段在同一条消息里', 5000)
  smoke.check('几段拼成了同一条消息（一次提问）', !!both, both ? smoke.text(both) : '(没有这样的消息)')
  const batch = both ? smoke.text(both) : ''
  smoke.check('同一份文件的来源只写了一次', countOf(batch, '文件：') === 1, countOf(batch, '文件：') + ' 次')
  smoke.check('同一句默认问题只写了一次', countOf(batch, DEFAULT_ASK) === 1, countOf(batch, DEFAULT_ASK) + ' 次')
  smoke.check('三段的原文都在', [LINE_A, LINE_B, LINE_C].every((l) => batch.indexOf(l) >= 0), batch)
  smoke.check('删掉的那一段没发出去', batch.indexOf(LINE_D) < 0, batch)
  smoke.check('改过的问题算数（发出去的是改后的那句）',
    batch.indexOf(ASK_C2) >= 0 && batch.indexOf(ASK_C) < 0, batch)
  if (!(await smoke.waitText('.msg-assistant', '一起问的那几条的回答', 20000))) return smoke.finish()

  // ── 宿主三：会话全文里 agent 的回答，两句合着一起问 ─────────────────────────
  // 从对话里选中的文字没有文件来源，所以发出去的那条消息不带「文件：」那一行。两句都空着回车：
  // 对话来源的几段该归在一个组里，那句默认问题也只写一次。
  const chat2 = await askAbout('.msg-assistant .msg-content p', CHAT_LINE2, '')
  if (!chat2) return smoke.finish()
  enter(chat2)
  if (!(await waitLines(1))) return smoke.finish()
  const chat3 = await askAbout('.msg-assistant .msg-content p', CHAT_LINE1, '')
  if (!chat3) return smoke.finish()
  enter(chat3)
  if (!(await waitLines(2))) return smoke.finish()
  smoke.check('对话里选的那几段也归在一个组里（组名是「对话」）',
    smoke.qa('.ask-q-group').length === 1 && smoke.text(smoke.q('.ask-q-group .ask-q-where')) === '对话',
    smoke.qa('.ask-q-group').length + ' 个组 / ' + smoke.allText('.ask-q-where').join(' | '))
  smoke.check('对话里那两句同一个默认问题也只占一个输入框',
    smoke.qa('.ask-q-askgroup').length === 1 && smoke.qa('.ask-q-ask').length === 1,
    smoke.qa('.ask-q-askgroup').length + ' 个问题组')

  smoke.click('.ask-q-send')
  const chatBatch = await smoke.waitFor(
    () => smoke.qa('.msg-user .msg-content').find(
      (m) => smoke.text(m).indexOf(CHAT_LINE1) >= 0 && smoke.text(m).indexOf(CHAT_LINE2) >= 0 &&
        smoke.text(m).indexOf('文件：') < 0) || null,
    '对话里问的那条进了会话', 5000)
  smoke.check('会话全文里选中的两段一起问了出去', !!chatBatch, chatBatch ? smoke.text(chatBatch) : '(没有这条消息)')
  const chatMsg = chatBatch ? smoke.text(chatBatch) : ''
  smoke.check('那两段共用一个默认问题', countOf(chatMsg, DEFAULT_ASK) === 1, countOf(chatMsg, DEFAULT_ASK) + ' 次')
  smoke.check('发送之后队列空了', !smoke.q('.ask-queue'), '')
  if (!(await smoke.waitText('.msg-assistant', '两句一起问的回答', 20000))) return smoke.finish()

  smoke.finish()
})()
