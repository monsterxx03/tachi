// review-all — 最新一轮多出来的那个「评审所有改动」入口。
//
// 它和「评审本轮改动」是两件事，所以这个 driver 要钉住三样别处钉不住的东西：
//
//   1. 位置：它只在**最新那一轮**上。第 1 轮自己有改动时两个入口都在；第 2 轮是只读的一问，
//      于是第 2 轮只剩它、第 1 轮不再有它 —— 而它仍然要在，因为工作树里的 NOTES.md 还没提交。
//      （这就是"空档"：没有这一条，读者问完一个只读的问题，就再也没有地方评审全部改动了。）
//   2. 范围：它起的那次评审读的是**工作树 vs HEAD**，所以面板的标题是「全部改动」而不是
//      「本轮改动」，头行说与 git HEAD 对照。
//   3. 状态机：和另一个入口一样是 评审所有改动 → 评审中… → 已评审 N 条 · 查看，两个入口共用
//      一轮的 footer，所以它们的状态绝不能互相顶替（此处只有它一个在跑，用来看它自己的状态）。
;(async () => {
  // 当前显示的是哪个会话：标题栏不再显示 id 了，读 app 根上的 data-session-id（唯一稳定的把手 ——
  // 标题做不到这件事，第二个会话一建出来「未命名会话」就会重复）。
  const sessionId = () => { const el = smoke.q('.app'); return (el && el.getAttribute('data-session-id')) || '' }
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitFor(() => (sessionId() ? true : null), '会话加载完成（app 上出现 data-session-id）'))) return smoke.finish()

  const chipIn = (bubble, label) =>
    Array.from(bubble.querySelectorAll('.diff-chip')).find((b) => b.textContent.indexOf(label) >= 0) || null
  // Scoped to `.chat`: the side-channel panel replays its run through the same bubble markup
  // (`.msg msg-assistant oneoff-turn`) and sits AFTER <main> in the DOM, so a bare
  // '.msg-assistant' query puts the panel's messages "last" — and the footer under test has no
  // chips there. (`frozen-panel` scopes the same way.)
  const bubbles = () => smoke.qa('.chat .msg-assistant')
  const lastBubble = () => bubbles()[bubbles().length - 1] || null
  const chipsOf = (bubble) => (bubble ? Array.from(bubble.querySelectorAll('.diff-chip')).map((b) => b.textContent.trim()).join(' | ') : '(没有气泡)')

  // ── 第 1 轮：这一轮自己写了文件 → 两个入口都在 ──────────────────────────────
  smoke.type('.composer-input', '写一个 NOTES.md')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant', '已经写下', 30000))) return smoke.finish()
  const both = await smoke.waitFor(
    () => {
      const last = lastBubble()
      return last && chipIn(last, '评审本轮改动') && chipIn(last, '评审所有改动') ? last : null
    },
    '最新一轮同时出现两个评审入口', 20000)
  smoke.check('有改动的一轮：评审本轮改动 + 评审所有改动 在同一行', !!both, chipsOf(lastBubble()))
  if (!both) return smoke.finish()

  // ── 第 2 轮：只读的一问 → 整树入口跟着最新一轮走 ────────────────────────────
  smoke.type('.composer-input', 'NOTES.md 里写了什么？')
  smoke.click('.send-btn')
  const firstGone = await smoke.waitFor(
    () => (bubbles().length >= 2 && !chipIn(bubbles()[0], '评审所有改动') ? true : null),
    '第 1 轮的整树入口随"最新"身份一起消失', 40000)
  smoke.check('整树入口只在最新一轮上（第 1 轮不再有它）', !!firstGone,
    '气泡数=' + bubbles().length + '；第 1 轮=' + chipsOf(bubbles()[0]))
  const gap = await smoke.waitFor(
    () => (lastBubble() && chipIn(lastBubble(), '评审所有改动') ? lastBubble() : null),
    '只读的那一轮仍然带着整树入口（工作树还有未提交的改动）', 20000)
  smoke.check('最新一轮自己没有改动、工作树还是脏的：整树入口还在', !!gap, chipsOf(lastBubble()))
  smoke.check('只读的那一轮没有本轮的评审入口（它没有改动）',
    !!gap && !chipIn(gap, '评审本轮改动'), chipsOf(lastBubble()))
  if (!gap) return smoke.finish()

  // 那一行没有 🧾 chip 可以解释自己，所以它自己说明有多少东西可评审 —— 否则"这一轮什么都没改，
  // 评审的是什么"就只能靠猜。
  const hint = smoke.text('.diff-hint')
  smoke.check('空档那一行说清有多少未提交的改动', hint.indexOf('未提交') >= 0, hint || '(没有 .diff-hint)')

  // ── 状态机：点击 → 评审中… → 已评审 N 条 · 查看 ────────────────────────────
  const seen = []
  const tick = setInterval(() => {
    const chip = chipIn(lastBubble() || document.body, '评审')
    if (!chip) return
    const text = smoke.text(chip)
    if (!seen.length || seen[seen.length - 1].text !== text) seen.push({ text, disabled: chip.disabled })
  }, 10)
  chipIn(gap, '评审所有改动').click()
  const after = await smoke.waitFor(
    () => {
      const last = lastBubble()
      return last && chipIn(last, '已评审') ? chipIn(last, '已评审') : null
    },
    'footer 变成「已评审 N 条 · 查看」', 40000)
  clearInterval(tick)
  const trail = seen.map((s) => s.text).join(' → ')
  smoke.check('整树评审期间经过「评审中…」', seen.some((s) => s.text.indexOf('评审中') >= 0), trail)
  smoke.check('条数来自后端对 ReportFinding 的计数（mock 报了 1 条）',
    !!after && smoke.text(after).indexOf('1 条') >= 0, after ? smoke.text(after) : trail)
  if (!after) return smoke.finish()

  // ── 面板：这次评审读的是工作树对 HEAD，不是某一轮的两个快照 ────────────────
  if (!(await smoke.waitFor('.oneoff-panel', '评审开始即打开面板', 8000))) return smoke.finish()
  const findingsTab = smoke.qa('.oneoff-tab').find((b) => b.textContent.indexOf('意见') >= 0)
  if (!findingsTab) return smoke.fail('面板有「意见」标签', smoke.allText('.oneoff-tab').join('|'))
  findingsTab.click()
  if (!(await smoke.waitFor('.oneoff-tab.active', '面板切到意见区', 8000))) return smoke.finish()

  const pane = await smoke.waitFor(
    () => (smoke.text('.oneoff-body').indexOf('NOTES.md') >= 0 ? smoke.text('.oneoff-body') : null),
    '意见区显示被评审的文件 diff', 15000)
  smoke.check('意见区里有被评审文件的 diff', !!pane && pane.indexOf('NOTES.md') >= 0, (pane || '').slice(0, 100))

  // finding 行自己也会印出文件路径，「其它文件」那一组也一样 —— 所以"看到 NOTES.md"并不证明 diff
  // 到了。要等的是 NOTES.md 自己的 diff 文件组：那是后端一次单独的往返（整树评审走的是
  // GetWorkingTreeChanges），它没到就会退成"空 diff"或"没有记录文件清单"。
  const diffGroup = await smoke.waitFor(
    () => smoke.qa('.oneoff-body .diff-file').find((g) => {
      const p = g.querySelector('.diff-path')
      return p && smoke.text(p) === 'NOTES.md'
    }) || null,
    '意见区渲染出 NOTES.md 自己的 diff 文件组', 15000)
  smoke.check('意见区有 diff 文件组（不是只有意见行/「其它文件」）', !!diffGroup,
    '组=' + smoke.allText('.oneoff-body .diff-path').join(' | ') + ' ｜ ' + smoke.text('.oneoff-body').slice(0, 120))
  smoke.check('没有退到"这次运行没有记录文件清单"那一态',
    smoke.text('.oneoff-body').indexOf('没有记录文件清单') < 0,
    smoke.text('.oneoff-body').slice(0, 240))

  // 标题跟着那次往返来，所以要等它 —— 直接读会抢在 diff 之前（那正是"标题还是本轮改动"的原因）。
  const title = await smoke.waitFor(
    () => (smoke.text('.diff-panel-title') === '全部改动' ? smoke.text('.diff-panel-title') : null),
    '标题变成「全部改动」', 10000)
  smoke.check('标题说「全部改动」（不是「本轮改动」）', !!title,
    smoke.text('.diff-panel-title') + ' ｜ ' + smoke.text('.diff-panel-head'))
  const head = smoke.text('.diff-panel-head')
  smoke.check('头行说与 git HEAD 对照（整树评审的基线就是 HEAD）',
    head.indexOf('与 git HEAD 对照') >= 0, head)
  smoke.check('意见区报出条数', (pane || '').indexOf('评审意见 1 条') >= 0, '')
  smoke.check('意见挂在 diff 行上（有 finding 行）', !!smoke.q('.oneoff-body .finding'), '')

  // 运行过之后入口仍然可点，而且点它是回到面板（不是再起一次评审）——两个入口共用这四态，
  // 这里顺带确认整树那一个也守同样的约定。
  if (!smoke.click('.oneoff-toggle')) return smoke.fail('关掉面板', '按钮找不到')
  if (!(await smoke.waitFor(() => !smoke.q('.oneoff-panel'), '面板已收起', 5000))) return smoke.finish()
  const stillThere = lastBubble() && chipIn(lastBubble(), '已评审')
  smoke.check('收起后入口还在（已评审状态不随面板走）', !!stillThere, chipsOf(lastBubble()))
  if (stillThere) stillThere.click()
  const reopened = await smoke.waitFor('.oneoff-panel', '点「查看」重新打开面板', 5000)
  smoke.check('完成后这个入口是回到面板的路', !!reopened, '')

  smoke.finish()
})()
