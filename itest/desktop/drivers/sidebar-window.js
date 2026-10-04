// sidebar-window — 侧栏每个组只渲染「窗口」那么多行，滚到尾部（或点行尾那一行）再续下一段。
//
// 为什么要窗口：会话列表没有上界（这台机器上就有 100 多个无项目会话），而读者看的是最新的那一小段，
// 其余是没人要求绘制出来的滚动条。窗口是**每组各自**的（App.tsx 的 rowWindow），所以旁边那个只有
// 一条会话的项目组不该被桶的长短牵连。
//
// 这个场景要钉住三件事，都是界面上看不出来的：
//   1. 截断是诚实的：画出来的行 + 行尾那句「还有 N 条」= 桶里真实的条数；
//   2. 续窗口有两条路，且一次只续一段：滚到尾部会自己续，点行尾也续，但都不是「一次画完」；
//   3. **最关键的那条**：⌘K 选中桶里最老的一个会话（fixture 的「批量会话 01」，也就是窗口根本
//      够不到的最后一行）时，那一行必须被**撑出来**并滚进视野。窗口化最容易坏在这里——「把当前
//      会话滚进视野」这条规则撞上一个还没渲染的行，就是找不到行、请求悄悄卡住（App.tsx 的
//      revealRow effect 为此多了一次「撑窗口」）。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const nav = () => smoke.q('.session-list')
  // 无项目桶的行是 nav 的直接子元素（项目行缩在 .proj-rows 里）——与 session-keys.js 同一个判据。
  const looseRows = () => smoke.qa('.session-list > .session')
  const projRows = () => smoke.qa('.proj-rows .session')
  const more = () => smoke.q('.session-more')
  const sessionId = () => { const el = smoke.q('.app'); return (el && el.getAttribute('data-session-id')) || '' }
  // 行尾那句「还有 N 条 ▾」里的 N。它和画出来的行数必须自洽，否则那句话就是假的。
  const hidden = () => {
    const el = more()
    if (!el) return 0
    const m = smoke.text(el).match(/(\d+)/)
    return m ? parseInt(m[1], 10) : -1
  }
  const bucketTotal = () => looseRows().length + hidden()
  const foot = () => `画出 ${looseRows().length} + 行尾 ${hidden()}`
  const inView = (el) => {
    const box = nav().getBoundingClientRect()
    const r = el.getBoundingClientRect()
    return r.top >= box.top - 1 && r.bottom <= box.bottom + 1
  }

  // ── 1. 截断是诚实的，而且只发生在长的那个组上 ─────────────────────────────
  // fixture：无项目桶 101 条（100 个批量 + 无项目会话），项目组 1 条（冒烟会话）。
  const first = await smoke.waitFor(
    () => (looseRows().length > 0 && !!more() ? true : null), '长的那组出现行尾的「还有 N 条」', 10000)
  smoke.check('桶比窗口长时，行尾说还有多少条', !!first, foot())
  smoke.check('画出来的 + 行尾说的 = 桶里真实的条数（101）', bucketTotal() === 101, foot())
  smoke.check('短的组照旧完整、也没有行尾', projRows().length === 1 && !smoke.q('.proj-rows .session-more'),
    projRows().length + ' 行')

  // ── 2. 滚到尾部：自己续一段，但不是一次画完 ──────────────────────────────
  const beforeScroll = looseRows().length
  nav().scrollTop = nav().scrollHeight
  const grew = await smoke.waitFor(() => (looseRows().length > beforeScroll ? true : null), '滚到底部续上下一段', 8000)
  smoke.check('滚到尾部就续下一段', !!grew, `${beforeScroll} → ${looseRows().length}`)
  smoke.check('一次只续一段，没有把 101 条一次画完', !!more() && looseRows().length < 101, foot())

  // ── 3. 点行尾那一行也续（同一个动作的另一个入口）─────────────────────────
  const beforeClick = looseRows().length
  const tail = more()
  if (!tail) return smoke.fail('点行尾之前它还在', foot())
  tail.click()
  const grewByClick = await smoke.waitFor(() => (looseRows().length > beforeClick ? true : null), '点击行尾续上下一段', 8000)
  smoke.check('点一下行尾也续下一段', !!grewByClick, `${beforeClick} → ${looseRows().length}`)

  // ── 4. 窗口外的会话被选中时，窗口要为它撑开并把那行滚进视野 ────────────────
  // 「批量会话 01」是 fixture 里最老的一个（先播的），也是桶里最后一行——此刻窗口离它还差得远。
  const before = sessionId()
  smoke.key(document.body, 'k', { metaKey: true })
  if (!(await smoke.waitFor('.palette-box', '⌘K 浮层打开', 8000))) return smoke.finish()
  smoke.type('.palette-input', '批量会话 01')
  const only = await smoke.waitFor(() => (smoke.qa('.palette-row').length === 1 ? true : null), '按标题过滤到最老的那条', 4000)
  if (!only) return smoke.finish()
  smoke.key('.palette-input', 'Enter')
  const landed = await smoke.waitFor(() => (sessionId() && sessionId() !== before ? true : null), '⌘K 切到最老的那个会话', 10000)
  smoke.check('⌘K 切到了窗口根本够不到的会话', !!landed, `session-id ${sessionId()}`)

  const visible = await smoke.waitFor(() => {
    const row = smoke.q('.session.active')
    if (!row) return null
    return inView(row) ? true : null
  }, '那一行被撑出来并滚进视野', 8000)
  smoke.check('窗口为它撑开了，那一行在视野里', !!visible,
    smoke.q('.session.active')
      ? `行顶距视野 ${Math.round(smoke.q('.session.active').getBoundingClientRect().top - nav().getBoundingClientRect().top)}px`
      : '没有 active 行')
  // 撑到能看见它，就意味着窗口已经盖过整桶：行尾那句该消失了，而且总数没被改动。
  smoke.check('撑开之后桶里还是 101 条（窗口只多画，不增删）', bucketTotal() === 101 && !more(), foot())

  smoke.finish()
})()
