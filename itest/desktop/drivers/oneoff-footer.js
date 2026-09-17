// oneoff-footer — the turn-level review entry, and the state machine it became.
//
// The chip used to start a review that streamed a whole turn into the conversation (and left
// that turn looking like one that had changed files). Now the review is a side-channel run:
// the chip goes 评审本轮改动 → 评审中… → 已评审 N 条 · 查看, where N is the backend's tally of
// ReportFinding calls, and 查看 opens the panel the run's process went to.
//
// The scope here is the file the turn wrote, so this scenario runs in a git repository with an
// uncommitted change — a scoped review refuses to start without a baseline.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  // Two keys this driver presses from several places: Esc (dismissal, and the panel's ownership
  // dance at the end) and ⌘F (find, which three surfaces now host).
  const pressEsc = (target) => (target || document.body).dispatchEvent(
    new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
  const pressCmdF = (target) => (target || document.body).dispatchEvent(
    new KeyboardEvent('keydown', { key: 'f', metaKey: true, bubbles: true, cancelable: true }))

  smoke.type('.composer-input', '写一个 NOTES.md')
  smoke.click('.send-btn')

  // The turn writes TWO files in two rounds, and the chip below appears as soon as the FIRST
  // file's change landed — it is a proxy for "this turn changed something", not for "this turn's
  // file set is final". Clicking 完整 diff on the strength of that proxy captured a one-file
  // `paths` set, and the overlay then showed only NOTES.md: a full run failed exactly these two
  // assertions that way (2/78, and the DOM carried NOTES.md with no OTHER.md in the overlay).
  // What the click needs is the turn's FINAL file set, so wait for the turn's own closing line.
  if (!(await smoke.waitText('.msg-assistant', '已经写下', 30000))) return smoke.finish()

  // The chip's arrival is the proof that a changed-file turn gets a review entry (and that the
  // WriteFile round landed — a fragment diff on the card feeds the same aggregate).
  const before = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('评审本轮改动') >= 0) || null,
    'footer 出现「评审本轮改动」', 30000)
  if (!before) return smoke.finish()
  smoke.check('有改动的回合才有评审入口', true, smoke.qa('.diff-chip').map((b) => b.textContent).join(' | '))
  smoke.check('评审前没有已评审状态', !smoke.qa('.diff-chip').some((b) => b.textContent.indexOf('已评审') >= 0), '')

  // ── 「完整 diff」 belongs to the TURN, and works with no review at all ────────
  // This is the entry's own promise (本轮的完整改动，带真实行号；与什么对照由浮层顶部那行说明),
  // and it used to be routed into the side-channel panel — which is keyed by RUN. On a turn nobody
  // had reviewed there was no run to show, so the click opened an empty column (「点完整 diff，侧边栏
  // 是空的」); once a run did exist, the panel showed the SELECTED run's file set rather than this
  // turn's. Hence both halves are pinned HERE, before any review exists: the turn's own files are in
  // the overlay, and no panel was involved in getting them.
  const diffChip = smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('完整 diff') >= 0)
  if (!diffChip) return smoke.fail('找到「完整 diff」入口', '按钮找不到')
  diffChip.click()
  const overlay = await smoke.waitFor('.viewer-overlay .viewer-doc.is-diff', '「完整 diff」打开宽浮层', 8000)
  if (!overlay) return smoke.finish()
  smoke.check('没有评审时也不打开旁路面板（它按 run 组织，此刻没有 run）',
    !smoke.q('.oneoff-panel'), smoke.q('.oneoff-panel') ? '面板被打开了' : '')
  const turnDiff = await smoke.waitFor(
    () => (smoke.text('.viewer-doc.is-diff').indexOf('OTHER.md') >= 0 ? smoke.text('.viewer-doc.is-diff') : null),
    '浮层里是本轮两个文件对 HEAD 的差异', 10000)
  smoke.check('浮层里就是本轮改动的文件（NOTES.md + OTHER.md）',
    !!turnDiff && turnDiff.indexOf('NOTES.md') >= 0 && turnDiff.indexOf('OTHER.md') >= 0,
    (turnDiff || '').slice(0, 100))
  smoke.check('带真实文件行号（卡片上的片段 diff 没有这一列）',
    smoke.qa('.viewer-doc.is-diff .diff-ln').length > 0,
    String(smoke.qa('.viewer-doc.is-diff .diff-ln').length))
  // The fold's default is the panel's ("a file with findings opens, one without is folded") and
  // that rule makes no sense here: with no findings at all it folded EVERY file, so the chip's
  // promise arrived as a list of file headers — the reader had to click each one to see a diff.
  smoke.check('浮层里每个文件默认展开（没有意见可围着折）',
    smoke.qa('.viewer-doc.is-diff .diff-file').length > 0 &&
    smoke.qa('.viewer-doc.is-diff .diff-file').every((g) => !g.classList.contains('is-folded')),
    smoke.qa('.viewer-doc.is-diff .diff-file').map((g) => g.className).join(' | '))
  smoke.check('没有评审就没有意见行', !smoke.q('.viewer-doc.is-diff .finding'), '')

  // ── The file index down the left ────────────────────────────────────────────
  // A diff of twenty files is a wall, and the reader arrives with one file in mind. Two facts are
  // pinned here that the markup alone does not promise: the list does NOT move while the diff
  // scrolls (the requirement is "fixed", not "scrolls along"), and a click LANDS on that file.
  const filelist = smoke.q('.viewer-doc.is-diff .diff-filelist')
  smoke.check('完整 diff 浮层里有左侧文件索引', !!filelist, filelist ? smoke.text(filelist) : '(没有 .diff-filelist)')
  const rows = smoke.qa('.diff-filelist-row')
  smoke.check('索引里每个文件一行（本轮两个文件两行）',
    rows.length === 2 && smoke.text(rows[0]).indexOf('NOTES.md') >= 0 && smoke.text(rows[1]).indexOf('OTHER.md') >= 0,
    rows.map((r) => smoke.text(r)).join(' | '))
  // 单根 diff 不分区：一棵树没有要区分的东西，一条「主目录」标题只会白占一行高度。
  smoke.check('单根 diff 的索引没有分区标题', !smoke.q('.diff-filelist-group'),
    smoke.q('.diff-filelist-group') ? '出现了 .diff-filelist-group' : '')

  const stage = smoke.q('.viewer-overlay .viewer-stage')
  const head = smoke.q('.viewer-doc.is-diff .diff-panel-head')
  const topOf = (el) => (el ? Math.round(el.getBoundingClientRect().top) : NaN)
  // 钉住 = 滚到哪儿它都停在该在的位置。起点不算：不滚的时候列表在头部下方（它的自然位置），那正是
  // 它该在的地方 —— sticky 是"要滚出去时留在那儿"，不是"永远不动"。所以取两个滚过的位置比较：
  //   头部 → 滚动区顶端（它就是标题栏，滚走了读者就不知道这是谁的 diff）；
  //   列表 → 头部底边（在头部下方，不是钻到它里面 —— 那会吃掉前几行）。
  // 跟内容一起滚的话，这两个位置都会随 scrollTop 变。
  const stageTop = topOf(stage)
  // 头部占的垂直空间 = 它的盒子 + 它下面那道被阴影涂上的缝（12px）。列表要钉在这整段之下：
  // 钉在盒子底边的话，第一行会被那道阴影盖掉一半。
  const headH = head
    ? Math.round(head.getBoundingClientRect().height + (parseFloat(getComputedStyle(head).marginBottom) || 0))
    : 0
  const at = (y) => { if (stage) stage.scrollTop = y; return { head: topOf(head), list: topOf(filelist) } }
  const mid = at(Math.round((stage ? stage.scrollHeight : 0) / 3))
  const bottom = at(stage ? stage.scrollHeight : 0)
  const diffScrolled = !!stage && stage.scrollTop > 0
  smoke.check('标题栏钉在滚动区顶部（滚到哪都在）',
    diffScrolled && Math.abs(mid.head - stageTop) <= 2 && Math.abs(bottom.head - stageTop) <= 2,
    `滚动区 top=${stageTop}；标题栏 top 在 1/3 处=${mid.head}、底部=${bottom.head}（高 ${headH}）`)
  smoke.check('列表钉在标题栏下方（滚到哪都在，也没被标题栏盖住）',
    diffScrolled && Math.abs(mid.list - (stageTop + headH)) <= 2 && Math.abs(bottom.list - (stageTop + headH)) <= 2,
    `期望 ${stageTop + headH}（滚动区 top ${stageTop} + 标题栏高 ${headH}）；` +
    `列表 top 在 1/3 处=${mid.list}、底部=${bottom.list}` +
    // What the pin actually resolves to, for the one failure this check cannot explain on its own:
    // the sticky element is the COLUMN (`.diff-filelist-col`), and its offset is the pane's
    // measured `--diff-head-h`. A pin at the scroller's top means that variable resolved to 0.
    (() => {
      const pane = smoke.q('.viewer-doc.is-diff')
      const col = smoke.q('.diff-filelist-col')
      return `；${['pane', 'col'].map((n) => {
        const el = n === 'pane' ? pane : col
        return `${n} var=${el ? getComputedStyle(el).getPropertyValue('--diff-head-h').trim() || '(空)' : '(无元素)'}` +
          ` position=${el ? getComputedStyle(el).position : '-'}` +
          ` top=${el ? getComputedStyle(el).top : '-'}`
      }).join(' / ')}` +
        `；col top=${topOf(col)} navTop=${topOf(filelist)} head bottom=${head ? Math.round(head.getBoundingClientRect().bottom) : NaN}`
    })())

  // 从底部点第一行（NOTES.md 那 80 行在 OTHER.md 之前）：跳的方向穿越整份 diff，而且落点一定
  // 到得了。反过来点末尾那个文件就证明不了什么 —— 滚动区已经到底，位置会被钳住。
  const notesGroup = () => smoke.qa('.viewer-doc.is-diff .diff-file').find((el) => {
    const p = el.querySelector('.diff-path')
    return p && smoke.text(p) === 'NOTES.md'
  }) || null
  const notesOffset = () => {
    const g = notesGroup()
    return g && stage ? Math.round(g.getBoundingClientRect().top - stage.getBoundingClientRect().top) : null
  }
  if (rows[0]) rows[0].click()
  const want = headH + 10 // 标题栏高 + scroll-margin：落点必须在钉住的标题栏下方
  const fileLanded = await smoke.waitFor(
    // 滚动区此刻在底部，所以这个位置顺带证明了"真的滚了"，而不是"恰好没动"。±3px 是取整 +
    // 头部高度测量的误差。
    () => { const d = notesOffset(); return d !== null && Math.abs(d - want) <= 3 ? d : null },
    '点索引后 NOTES.md 的组落到标题栏下方', 5000)
  smoke.check('点索引跳到那个文件（落在标题栏下方，不是被它盖住）', fileLanded !== null,
    `落在滚动区顶部下 ${notesOffset()}px（期望 ${want} = 标题栏 ${headH} + 10）；` +
    `scrollTop=${stage ? Math.round(stage.scrollTop) : 'n/a'}`)
  smoke.check('落点带高亮（"就是这里"）', !!(notesGroup() || {}).classList
    && notesGroup().classList.contains('is-jump-target'),
    notesGroup() ? notesGroup().className : '')
  if (stage) stage.scrollTop = 0

  // ── 索引里"当前是哪个文件" ────────────────────────────────────────────────
  // 索引钉在左边、正文在滚，所以这个问题只能由**滚动**回答：只跟着点击走的话，它报的是读者上一次
  // 点的位置，而正文早就飘到别的文件去了。点击给一个即时的答案，滚动再把它纠正 —— 两条都钉。
  const rowFor = (name) => smoke.qa('.diff-filelist-row').find((r) => smoke.text(r).indexOf(name) >= 0) || null
  const isCurrent = (name) => { const r = rowFor(name); return !!r && r.classList.contains('is-current') }
  smoke.check('点过的那一行就是当前文件', isCurrent('NOTES.md'),
    smoke.allText('.diff-filelist-row.is-current').join(' | ') || '(没有高亮行)')
  smoke.check('同一时刻只有一行是当前', smoke.qa('.diff-filelist-row.is-current').length === 1,
    smoke.qa('.diff-filelist-row.is-current').length + ' 行高亮')
  // 滚到底：当前文件必须换成第二个。这一步与点击无关，是"高亮在回答我在读什么"的唯一证据。
  if (stage) stage.scrollTop = stage.scrollHeight
  const followed = await smoke.waitFor(() => (isCurrent('OTHER.md') ? true : null), '滚到底后高亮跟到 OTHER.md', 4000)
  smoke.check('高亮跟着滚动走（不是只跟着点击）', !!followed,
    smoke.allText('.diff-filelist-row.is-current').join(' | ') || '(没有高亮行)')
  if (stage) stage.scrollTop = 0
  const back = await smoke.waitFor(() => (isCurrent('NOTES.md') ? true : null), '回到顶部高亮回到第一个', 4000)
  smoke.check('滚回顶部高亮跟着回来', !!back, smoke.allText('.diff-filelist-row.is-current').join(' | ') || '(没有高亮行)')

  // ── 索引列可拖拽（文件名太长看不全时的那条出路）────────────────────────────
  // 与旁路面板同一条判据：按下就进拖拽态、拖动改宽度、两端有钳位、正文不许被吃穿。
  const listEl = () => smoke.q('.viewer-doc.is-diff .diff-filelist')
  const diffBodyEl = () => smoke.q('.viewer-doc.is-diff .diff-split-body')
  const widthOf = (el) => (el ? Math.round(el.getBoundingClientRect().width) : 0)
  const handle = smoke.q('.diff-filelist-resizer')
  if (!handle) return smoke.fail('索引有拖拽手柄', '找不到 .diff-filelist-resizer')
  // 手柄自己会跟着宽度跑，所以每次按下都重新读它的位置（第二次拖还用旧坐标就是拖了个空气）。
  const pressHandle = () => {
    const b = handle.getBoundingClientRect()
    const x = b.left + 4, y = b.top + 30
    handle.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, pointerId: 1, clientX: x, clientY: y }))
    return { x, y }
  }
  const moveTo = (x, y) => window.dispatchEvent(new PointerEvent('pointermove', { bubbles: true, pointerId: 1, clientX: x, clientY: y }))
  const releaseAt = (x, y) => window.dispatchEvent(new PointerEvent('pointerup', { bubbles: true, pointerId: 1, clientX: x, clientY: y }))

  const sizesBefore = { list: widthOf(listEl()), body: widthOf(diffBodyEl()) }
  let p = pressHandle()
  smoke.check('按下手柄即进入拖拽态', document.body.classList.contains('is-resizing'), `body=${document.body.className}`)
  moveTo(p.x - 5000, p.y); releaseAt(p.x - 5000, p.y) // 往左拖到底 → 钳在下限
  const floored = await smoke.waitFor(() => (widthOf(listEl()) === 180 ? 180 : null), '列表被钳在最小宽度', 3000)
  smoke.check('列表有下限（再窄就没法读路径了）', !!floored, `${sizesBefore.list} → ${widthOf(listEl())}px`)
  p = pressHandle()
  moveTo(p.x + 5000, p.y); releaseAt(p.x + 5000, p.y) // 往右拖到底 → 钳在"正文还有地方"的那条线
  const widened = await smoke.waitFor(() => (widthOf(listEl()) > sizesBefore.list ? true : null), '列表被拖宽', 3000)
  smoke.check('拖动手柄能把列表拉宽（长路径看全的出路）', !!widened, `${sizesBefore.list} → ${widthOf(listEl())}px`)
  smoke.check('列表宽度有上限（它是找文件的索引，不是第二份 diff）', widthOf(listEl()) <= 560,
    widthOf(listEl()) + 'px')
  smoke.check('拉宽之后正文仍然是正文（没被索引吃掉）', widthOf(diffBodyEl()) >= 420,
    `正文 ${widthOf(diffBodyEl())}px（拖前 ${sizesBefore.body}px）`)

  // ── ⌘F in the diff overlay ─────────────────────────────────────────────────
  // A diff is the surface find exists for, and this overlay is one of its three hosts (the panel
  // and a previewed file are the others). NOTES.md's fixture repeats one line 80 times, so the
  // count is a number rather than "some hits" — and the walk has somewhere to walk to.
  pressCmdF()
  const diffBar = await smoke.waitFor('.viewer-overlay .find-bar', '⌘F 在 diff 浮层里打开查找条', 5000)
  smoke.check('⌘F 在 diff 浮层里打开查找条', !!diffBar, '')
  smoke.type('.find-bar .find-input', '一行笔记')
  const diffCount = await smoke.waitFor(
    () => (smoke.text('.find-count') === '1/80' ? smoke.text('.find-count') : null),
    '查找条数出 80 处命中', 8000)
  smoke.check('命中数是这份 diff 里真实的条数（1/80）', !!diffCount, smoke.text('.find-count'))
  smoke.check('命中画在 diff 正文里（高亮注册表里有 80 段）',
    (CSS.highlights.get('tachi-find')?.size || 0) === 80, String(CSS.highlights.get('tachi-find')?.size))
  smoke.key('.find-bar .find-input', 'Enter')
  const diffWalk = await smoke.waitFor(
    () => (smoke.text('.find-count') === '2/80' ? smoke.text('.find-count') : null),
    'Enter 走到下一处命中', 5000)
  smoke.check('Enter 走到下一处命中（2/80）', !!diffWalk, smoke.text('.find-count'))
  pressEsc()
  const diffBarGone = await smoke.waitFor(() => !smoke.q('.find-bar'), 'Esc 关掉查找条', 3000)
  smoke.check('Esc 关掉查找条，diff 浮层还在（查找条拥有这个键）',
    !!diffBarGone && !!smoke.q('.viewer-overlay'), '')

  smoke.key(document.body, 'Escape')
  const closed = await smoke.waitFor(() => !smoke.q('.viewer-overlay'), 'Esc 关掉浮层', 5000)
  smoke.check('Esc 关掉浮层（viewer 的通用约定）', !!closed, '')

  // The chip's MIDDLE state — the one that was reported missing (「点击评审按钮后，立刻会变成
  // 已评审查看，但没法点击，应该是评审中」). Two things made it invisible: the pending state was
  // cleared as soon as the fork had been STARTED (while the record on disk already carried this
  // turn's id, so the chip jumped straight to 已评审 with the button disabled by the very run it
  // reported on), AND it is a short-lived state whose length is the run's — a few hundred ms under
  // the mock, seconds to minutes for a real review. So this does not wait for the state, it
  // SAMPLES the chip and asserts the sequence contains it: what is being pinned is that the chip
  // passes through 评审中…, and that it stays clickable there.
  const seen = []
  const tick = setInterval(() => {
    const chip = smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('评审') >= 0)
    if (!chip) return
    const text = smoke.text(chip)
    if (!seen.length || seen[seen.length - 1].text !== text) seen.push({ text, disabled: chip.disabled })
  }, 10)
  before.click()
  const after = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('已评审') >= 0) || null,
    'footer 变成「已评审 N 条 · 查看」', 30000)
  clearInterval(tick)
  const trail = seen.map((s) => s.text).join(' → ')

  const pending = seen.find((s) => s.text.indexOf('评审中') >= 0)
  smoke.check('评审期间 chip 经过「评审中…」（而不是直接跳到已评审）', !!pending, trail)
  smoke.check('「评审中…」可以点（去看过程）', !!pending && !pending.disabled,
    pending ? `disabled=${pending.disabled}` : '')
  smoke.check('评审结束后 chip 报出意见条数', !!after, after ? smoke.text(after) : trail)
  // The FINAL state is read off the element the wait returned, not off the trail: the trail is
  // for states whose whole life is the run (评审中…), and its 10ms ticks can be starved while the
  // webview is busy — the last change before the wait resolved was missed in one run, which
  // turned a steady state into an intermittent failure.
  smoke.check('「已评审 · 查看」可以点（会话忙也不禁用）', !!after && !after.disabled,
    after ? `disabled=${after.disabled}` : '(没找到已评审的 chip)')
  smoke.check('条数来自后端对 ReportFinding 的计数（mock 报了 1 条）',
    !!after && smoke.text(after).indexOf('1 条') >= 0, after ? smoke.text(after) : '')

  // The run's process is NOT in the conversation — that was the original complaint: a review
  // turn that looked like a turn which had changed files. The turn's OWN cards stay (they are
  // the turn: one per file it wrote), so the assertion is about the review's: its
  // ReportFinding call must not appear.
  const chat = smoke.text('.chat')
  smoke.check('对话里没有评审的回复', chat.indexOf('评审完成：1 条意见。') < 0, '')
  // 工具卡现在收在过程条里（成功的一步默认折叠），所以先把对话里的过程条都展开再数——
  // 展开也让这两条断言更强：评审若真把卡留在了对话里，展开后一定会露出来。
  smoke.qa('.chat .process-head').forEach((s) => smoke.click(s))
  if (!(await smoke.waitFor(() => smoke.qa('.chat .tool-name').length === 2, '展开后本轮两张卡都在'))) return smoke.finish()
  const names = smoke.qa('.chat .tool-name').map((e) => e.textContent)
  smoke.check('对话里没有评审的 ReportFinding 卡', !names.some((n) => n.indexOf('ReportFinding') >= 0), names.join('|'))
  smoke.check('对话里只有本轮自己的工具卡（两个文件两张）', names.length === 2, names.join('|'))

  // ...and the chip is now the way TO the run.
  if (!(await smoke.waitFor('.oneoff-panel', '评审开始即打开面板', 8000))) return smoke.finish()
  if (!smoke.click('.oneoff-toggle')) return smoke.fail('关掉面板', '按钮找不到')
  if (!(await smoke.waitFor(() => !smoke.q('.oneoff-panel'), '面板已收起', 5000))) return smoke.finish()
  after.click()
  const reopened = await smoke.waitFor('.oneoff-panel', '点「查看」重新打开面板', 5000)
  smoke.check('完成后 chip 变成回到面板的入口', !!reopened, '')

  // The panel opens on 过程 and is showing THIS review, with the reply the mock produced.
  const replay = await smoke.waitFor(
    () => smoke.text('.oneoff-body').indexOf('评审完成：1 条意见。') >= 0 ? smoke.text('.oneoff-body') : null,
    '面板回放这次评审', 8000)
  smoke.check('面板回放出这次评审的回复', !!replay, (replay || '').slice(0, 80))

  // P3: the 意见 pane pairs the run's OWN findings with the diff of the files it reviewed. The
  // way in is the pane's own TAB — 「完整 diff」 used to be pressed into service as this trigger,
  // which is exactly how the chip came to promise the TURN's diff while showing a run's (and why
  // a turn with no review opened an empty column; that chip is pinned above, on its own).
  const findingsTab = smoke.qa('.oneoff-tab').find((b) => b.textContent.indexOf('意见') >= 0)
  if (!findingsTab) return smoke.fail('面板有「意见」标签', smoke.allText('.oneoff-tab').join('|'))
  findingsTab.click()
  if (!(await smoke.waitFor('.oneoff-tab.active', '面板切到意见区', 8000))) return smoke.finish()
  smoke.check('「意见」标签切到意见 + diff 区', smoke.text('.oneoff-tab.active').indexOf('意见') >= 0, smoke.text('.oneoff-tab.active'))
  const pane = await smoke.waitFor(() => smoke.text('.oneoff-body').indexOf('NOTES.md') >= 0 ? smoke.text('.oneoff-body') : null,
    '意见区显示被评审的文件 diff', 10000)
  smoke.check('意见区里有被评审文件的 diff', !!pane && pane.indexOf('NOTES.md') >= 0, (pane || '').slice(0, 100))
  smoke.check('意见挂在 diff 行上（有 finding 行）', !!smoke.q('.oneoff-body .finding'), '')
  smoke.check('意见区报出条数', (pane || '').indexOf('评审意见 1 条') >= 0, '')

  // The pane needs the FILE GROUPS, not just the text: the diff is a backend round trip of its
  // own, and the finding's own path (「其它文件」 renders it too) used to satisfy the wait above
  // while the diff itself was still on its way — so the fold assertions below ran against a
  // pane that had nothing to fold.
  const groups = await smoke.waitFor(() => (smoke.qa('.diff-file').length >= 2 ? smoke.qa('.diff-file') : null),
    '意见区渲染出被评审文件的 diff 组', 15000)
  if (!groups) {
    smoke.log('诊断：面板此刻的状态', smoke.text('.oneoff-body').slice(0, 200))
    return smoke.finish()
  }
  // 文件索引只属于浮层：面板是读者可拖窄的列，210px 的索引会吃掉三分之一，把 diff 挤到读不了
  // （而且那边已经有 ↑/↓ 走查）。
  smoke.check('面板的意见区没有文件索引（那是浮层的）', !smoke.q('.oneoff-body .diff-filelist'),
    smoke.q('.oneoff-body .diff-filelist') ? '面板里出现了 .diff-filelist' : '')

  // ── The file groups, their fold, and the walk between findings ───────────────
  // The turn wrote two files and the review reported ONE finding, so the two rules can be
  // told apart: a file with a finding opens (the review is what the reader came for), one
  // without stays shut (its diff is context, not news).
  // Groups are found by their path, not by index: the order is git's, not the driver's.
  const group = (name) => smoke.qa('.diff-file').find((g) => smoke.text(g.querySelector('.diff-path')).indexOf(name) >= 0)
  const notes = group('NOTES.md')
  const other = group('OTHER.md')
  smoke.check('diff 里有两个被评审的文件', !!notes && !!other,
    smoke.allText('.diff-file .diff-path').join(' | '))
  smoke.check('有意见的文件默认展开', !!notes && !!notes.querySelector('.diff-line'), '')
  smoke.check('没意见的文件默认折叠', !!other && !other.querySelector('.diff-line'), '')
  const otherToggle = other && other.querySelector('.diff-file-toggle')
  smoke.check('折叠态报给无障碍层', !!otherToggle && otherToggle.getAttribute('aria-expanded') === 'false',
    otherToggle ? 'aria-expanded=' + otherToggle.getAttribute('aria-expanded') : '(无按钮)')

  // Folding is the reader's decision too: collapsing the file with the finding drops its rows…
  const notesToggle = notes && notes.querySelector('.diff-file-toggle')
  if (!notesToggle) return smoke.fail('找到 NOTES.md 的折叠按钮', '按钮找不到')
  notesToggle.click()
  if (!(await smoke.waitFor(() => {
    const g = group('NOTES.md')
    return g && !g.querySelector('.diff-line')
  }, '折叠 NOTES.md 后不再渲染差异行', 4000))) return smoke.finish()

  // …and the ↑/↓ walk has to UNFOLD it before it can land on a finding inside (the two
  // features meet here: a jump target in a folded file has no row to scroll to).
  const jumpDown = smoke.qa('.diff-jump-btn').find((b) => (b.getAttribute('aria-label') || '') === '下一条意见')
  if (!jumpDown) return smoke.fail('意见区有「下一条意见」按钮', smoke.allText('.diff-jump-btn').join('|'))
  jumpDown.click()
  const landed = await smoke.waitFor(() => {
    const g = group('NOTES.md')
    const row = g && g.querySelector('.finding.is-jump-target')
    return row ? { open: !!g.querySelector('.diff-line'), row } : null
  }, '下一条意见把目标行标出来', 5000)
  smoke.check('跳转先展开所属文件，再落到那一条', !!landed && landed.open, landed ? 'open=' + landed.open : '')
  smoke.check('跳转计数报出位置', smoke.text('.diff-jump-pos') === '1/1', smoke.text('.diff-jump-pos'))

  // The send bar: the action sits at the LEFT end, so a long diff (whose horizontal scroll
  // used to carry the right end of the bar off-screen) cannot hide the way out.
  const go = smoke.q('.diff-sendbar-go')
  smoke.check('发出口的文案是「发给 Tachi」', !!go && smoke.text(go) === '发给 Tachi', go ? smoke.text(go) : '(无按钮)')
  const bodyEl = smoke.q('.oneoff-body')
  if (bodyEl && go) {
    bodyEl.scrollLeft = bodyEl.scrollWidth // harmless when the pane does not scroll sideways
    const b = bodyEl.getBoundingClientRect()
    const g = go.getBoundingClientRect()
    smoke.check('横向滚到底后按钮仍在面板可视区内', g.left >= b.left - 1 && g.right <= b.right + 1,
      `button=[${Math.round(g.left)},${Math.round(g.right)}] pane=[${Math.round(b.left)},${Math.round(b.right)}]` +
      ` scrollW/${bodyEl.clientWidth}=${bodyEl.scrollWidth}`)
  }

  // Re-running from the pane: the record carries the scope, so "review these files again" also
  // works from a historical entry. The mock's second review reports TWO findings, which is how
  // the chip proves it is showing the new run rather than the old one.
  const rerun = smoke.qa('.oneoff-body .diff-open').find((b) => b.textContent.indexOf('重新评审') >= 0)
  if (!rerun) return smoke.fail('意见区有「重新评审」按钮', '按钮找不到')
  rerun.click()
  const again = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('已评审 2 条') >= 0) || null,
    '重跑后 chip 报出新的条数', 30000)
  smoke.check('按同一范围重跑，chip 报出新条数（2 条）', !!again,
    again ? smoke.text(again) : smoke.qa('.diff-chip').map((b) => b.textContent).join(' | '))

  // The walk over the NEW run's payload: two findings, so it can be seen to advance — and to
  // start from the beginning again, because the position belongs to the run (the pane keeps
  // its state across a switch in the switcher, so a stale index would land mid-list).
  if (!(await smoke.waitFor(
    () => (smoke.text('.oneoff-body').indexOf('评审意见 2 条') >= 0 ? smoke.text('.oneoff-body') : null),
    '意见区换成重跑那一次的 2 条意见', 15000))) return smoke.finish()
  const next2 = smoke.qa('.diff-jump-btn').find((b) => (b.getAttribute('aria-label') || '') === '下一条意见')
  if (!next2) return smoke.fail('重跑后仍有「下一条意见」按钮', '按钮找不到')
  next2.click()
  if (!(await smoke.waitFor(() => smoke.text('.diff-jump-pos') === '1/2', '走到第 1 条', 5000))) return smoke.finish()
  smoke.check('新一次评审的走势从第 1 条重新开始', true, smoke.text('.diff-jump-pos'))
  next2.click()
  const last = await smoke.waitFor(() => {
    const hit = smoke.q('.finding[data-finding-idx="1"].is-jump-target')
    return smoke.text('.diff-jump-pos') === '2/2' && hit ? smoke.text('.diff-jump-pos') : null
  }, '走到第 2 条并把高亮落在它身上', 5000)
  smoke.check('再走一步到第 2 条（计数与高亮指向同一条）', !!last, smoke.text('.diff-jump-pos'))

  // ── The walk has to stay reachable while the pane is SCROLLED ─────────────────
  // This is what the ↑/↓ pair's home is about. The second finding sits 60 lines down a file of
  // 80, so the jump scrolls the pane — exactly the state in which a header-hosted pair was off
  // the top of the screen and the reader had to scroll back up to jump again
  // (「一往下跳箭头就看不到了」). The bar it lives in now is sticky, so it does not move.
  const scrolled = await smoke.waitFor(() => {
    const pane = smoke.q('.oneoff-body')
    return pane && pane.scrollTop > 0 ? Math.round(pane.scrollTop) : null
  }, '跳到第 2 条后面板确实滚动了', 4000)
  smoke.check('这次跳转把面板滚了下去（否则下面那条断言没有对象）', scrolled !== null,
    scrolled === null ? 'scrollTop=0，面板没有被滚动' : `scrollTop=${scrolled}`)
  const walkBox = (() => {
    const el = smoke.q('.diff-jump'), pane = smoke.q('.oneoff-body')
    if (!el || !pane) return null
    const a = el.getBoundingClientRect(), p = pane.getBoundingClientRect()
    return { visible: a.top >= p.top - 1 && a.bottom <= p.bottom + 1, a, p }
  })()
  smoke.check('面板滚动后 ↑/↓ 仍在可视区内（它跟着 sticky 浮动条）', !!walkBox && walkBox.visible, walkBox
    ? `jump=[${Math.round(walkBox.a.top)},${Math.round(walkBox.a.bottom)}] pane=[${Math.round(walkBox.p.top)},${Math.round(walkBox.p.bottom)}]`
    : '找不到 .diff-jump 或 .oneoff-body')

  // The resize handle's drag is NOT asserted here. Its fix (the width is written straight to the
  // panel, not through a state update per pointermove) is «左右拖动时很卡», and the property that
  // would show it — "the width follows the pointer in the same task" — cannot be READ from a
  // driver on this machine: with Reduce motion ON, base.css's blanket `transition-duration:
  // 0.01ms !important` makes every property change a real CSSTransition (`transition-property`
  // defaults to `all`), so a same-task rect read returns the OLD width even with the fix in place
  // (measured: inline "380px", computed "420px", `CSSTransition:width`, 380 one frame later). The
  // drag's behaviour is oneoff-panel's business, measured after the commit.

  // ── Esc closes the panel (the × says it does) ─────────────────────────────────
  // The button's own tooltip is 「关闭（Esc）」, and until now nothing in the panel listened: the
  // viewers' Esc handling lives in ViewerOverlay, which this column is not — so the promise was
  // simply false.
  //
  // WHO OWNS THE FIRST PRESS is decided by the reader's focus, and BOTH orderings are pinned
  // here, because the panel yields to a focused field on purpose — and the composer's Esc is the
  // same gesture as the panel's note field: /review leaves the focus IN the composer, exactly
  // where the reader typed it, so the case is the common one, not a corner.
  //   · focus in the composer (typed /review there) → the first Esc leaves the input (the focus
  //     goes to the message area) and the panel stays; the next press closes it;
  //   · focus in the panel's own note field → the same: it blurs, the next press closes;
  //   · nobody claiming it (focus on a div, on the body — where a click on a div leaves it in
  //     WebKit) → one press closes.
  // The handler must therefore not depend on the focus being "in" the panel: it is a
  // window-level listener that only refuses a key somebody else already claimed.
  const panelOpen = () => !!smoke.q('.oneoff-panel')
  const activeLabel = () => {
    const el = document.activeElement
    return el ? (el.className ? '.' + String(el.className).split(' ')[0] : el.tagName) : '(无)'
  }

  // (1) The composer has the focus — where /review left it.
  const composerInput = smoke.q('.composer-input')
  smoke.check('下方输入框在（下面这条要用它）', !!composerInput, '')
  if (composerInput) {
    composerInput.focus()
    pressEsc(composerInput) // a real key event on the field: React's handler runs, then the panel's
    await smoke.sleep(80)
    smoke.check('焦点在下方输入框，第一次 Esc 只离开输入框（面板不关）',
      panelOpen() && document.activeElement !== composerInput,
      `open=${panelOpen()} active=${activeLabel()}`)
    pressEsc() // the focus is on the message area now
    const closed = await smoke.waitFor(() => !panelOpen(), '第二次 Esc 关掉面板', 3000)
    smoke.check('焦点让出这个键之后，一次 Esc 就关掉面板（× 的提示是真的）', !!closed, '')
    if (!smoke.click('.oneoff-toggle')) return smoke.fail('重新打开面板', '按钮找不到')
    if (!(await smoke.waitFor('.oneoff-panel', '重新打开面板（下面那条要用它）', 3000))) return smoke.finish()
  }

  // (2) The focus is in the panel's own note field: it owns the first Esc, so a half-typed
  // note survives the key that closes the pane.
  const noteField = smoke.q('.oneoff-body .finding-comment')
  smoke.check('意见区有补充说明输入框（下面两条要用它）', !!noteField, noteField ? '' : '找不到 .finding-comment')
  if (noteField) {
    noteField.focus()
    pressEsc(noteField)
    await smoke.sleep(60)
    smoke.check('输入框里的第一次 Esc 只退出输入（面板不关）',
      panelOpen() && document.activeElement !== noteField,
      `open=${panelOpen()} active=${activeLabel()}`)
    pressEsc() // focus is on the body now — where a click on a div leaves it too
    const closed = await smoke.waitFor(() => !panelOpen(), '再按一次 Esc 关掉面板', 3000)
    smoke.check('再按一次 Esc 就关掉面板（× 的提示是真的）', !!closed, '')
    // …and put it back: the run's Go-side check reads the persisted "panel is open" flag.
    smoke.click('.oneoff-toggle')
    const reopened = await smoke.waitFor('.oneoff-panel', '重新打开面板', 3000)
    smoke.check('重新打开面板（把「开着」留给落盘的断言）', !!reopened, '')
  }

  // ── ⌘F in the PANEL — the same host, a different surface ────────────────────
  // The panel is ONE find host for all three of its panes (过程 / 意见+diff / 报告): the reader
  // asks to find in "what I am looking at", and the pane is what decides that. It is showing 意见
  // with the second run's diff here.
  pressCmdF()
  const panelBar = await smoke.waitFor('.oneoff-panel .find-bar', '⌘F 在旁路面板里打开查找条', 5000)
  smoke.check('⌘F 在旁路面板里打开查找条', !!panelBar, '')
  smoke.type('.find-bar .find-input', 'NOTES.md')
  const panelCount = await smoke.waitFor(
    () => (smoke.text('.find-count').indexOf('/') > 0 ? smoke.text('.find-count') : null),
    '面板里的查找条报出命中数', 8000)
  smoke.check('面板里的查找条有命中（diff 的路径里有 NOTES.md）', !!panelCount, smoke.text('.find-count'))
  smoke.check('命中画在面板正文里（高亮注册表非空）',
    (CSS.highlights.get('tachi-find')?.size || 0) > 0, String(CSS.highlights.get('tachi-find')?.size))
  pressEsc()
  const panelBarGone = await smoke.waitFor(() => !smoke.q('.find-bar'), 'Esc 关掉面板里的查找条', 3000)
  smoke.check('Esc 只关查找条，面板还在', !!panelBarGone && !!smoke.q('.oneoff-panel'), '')

  smoke.finish()
})()
