// compact-window — 自动压缩完成之后、那一轮结束之前的那段窗口期。
//
// 那段时间里对话已经搬进子会话（agent.FinalizeCompact 在 loop 里跑的），而 run、以及这一轮的事件流
// 还挂在父会话的键上（搬到回合结束才发生，见 desktop/compaction.go）。所以子会话**不能**被当成一个
// 普通的空闲会话：
//
//   · 这条对话的那一行要带运行标记 —— 读者看的就是它（改前：标记所在的键是父会话，而父会话那一行要么
//     折在「压缩前」里、要么根本不在屏幕上）；
//   · 点开它，发送键应该是「排队」、并且有停止按钮（改前：看起来空闲 → 直接发出去）；
//   · 在窗口期发的那句进队列，等这一轮结束后才作为下一轮真正的请求发出去。
//     改前：它立刻另起一个回合 —— 两个回合交错写同一个 messages.jsonl，侧栏两行一起转圈，
//     重建出来的历史里 tool_call / tool_result 还会配不上对。
//
// 窗口要靠对端撑住：子会话那一轮的回复里排了 6 个 1.5s 的停顿，驱动就在这 ~9s 里做完上面的事。
// 驱动断"屏幕上是什么"，Go 那一半断"两次请求的先后" —— 顺序这件事 DOM 说不出来。
;(async () => {
  const QUEUED = '窗口期这句先排着'
  const sessionId = () => { const el = smoke.q('.app'); return (el && el.getAttribute('data-session-id')) || '' }
  const rows = () => smoke.qa('.session')
  const dump = () => rows().map((r) => smoke.text(r)).join(' | ')

  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()
  if (!(await smoke.waitFor(() => (sessionId() ? true : null), '会话加载完成（app 上出现 data-session-id）'))) return smoke.finish()
  const fixtureId = sessionId()
  smoke.check('压缩前侧栏只有一个会话', rows().length === 1, rows().length + ' 行')

  smoke.type('.composer-input', '跑一个会触发压缩的回合')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.stop-btn', '回合开始（停止按钮出现）', 15000))) return smoke.finish()

  // 窗口打开的判据是「对话已压缩」那条通知：它写在**父会话**的转录里，而父会话此刻还在屏幕上
  // （切换要等这一轮结束才发生），所以它一出现，子会话就已经建好、而 run 还挂在父会话键上。
  if (!(await smoke.waitFor(() => (smoke.text('.chat').indexOf('对话已压缩') >= 0 ? true : null), '自动压缩完成（窗口打开）', 30000))) {
    smoke.log('诊断：侧栏行', dump())
    return smoke.finish()
  }

  // 窗口期侧栏列的还是一分钟前那份列表（回合中途没人刷新它），所以要看到这条对话的那一行，读者得先
  // 做点什么。新建会话是用户真会做的那件事，它顺带刷一次列表 —— 报的那个 bug 就是这么被撞见的。
  if (!smoke.click('.proj-head.is-loose .proj-add')) return smoke.fail('点击新建会话', '按钮找不到')
  if (!(await smoke.waitFor(() => (sessionId() !== fixtureId ? true : null), '新建会话', 10000))) return smoke.finish()
  const blankId = sessionId()

  const chain = await smoke.waitFor(
    () => (smoke.qa('.session-chain').length === 1 ? smoke.qa('.session-chain')[0] : null),
    '侧栏出现这条对话（带「压缩前」折叠入口）', 10000)
  if (!chain) { smoke.log('诊断：侧栏行', dump()); return smoke.finish() }
  // 折叠入口紧挨着它所属的那一行渲染（App.tsx 的 groupRows：行、折叠入口、被折起来的祖先），
  // 所以这一行就是这条对话本身 —— 按结构取，不按下标取。
  const convo = chain.previousElementSibling
  if (!convo || !convo.classList.contains('session')) {
    return smoke.fail('找到这条对话的那一行', convo ? String(convo.className) : '折叠入口前面没有会话行')
  }
  if (!smoke.click(convo)) return smoke.fail('点开这条对话', '行不可点')

  // 从这一刻起读者看的是子会话（这一轮正在往里写的那一个）。它的 id 既不是 fixture 也不是刚建的空会话。
  if (!(await smoke.waitFor(
    () => (sessionId() !== blankId && sessionId() !== fixtureId ? true : null),
    '点开这条对话（压缩后的子会话）', 10000))) return smoke.finish()

  // 这一轮还在跑，而前端必须知道 *这条对话* 在跑：发送键变「排队」，并且有停止按钮。
  // （这次点击自己会 refreshRunning，所以读到的是后端刚答的那一份。）
  if (!(await smoke.waitFor(() => (smoke.text('.send-btn').indexOf('排队') >= 0 ? true : null), '发送键变成排队（这条对话在跑）', 8000))) {
    smoke.log('诊断：发送键', smoke.text('.send-btn'))
    return smoke.finish()
  }
  smoke.check('窗口期这条对话是「在跑」的：发送键写着排队', true, smoke.text('.send-btn'))
  smoke.check('窗口期这条对话有停止按钮（Stop 打的是真正在跑的那一轮）', !!smoke.q('.stop-btn'), '')
  smoke.check('运行标记落在这条对话的那一行上', !!smoke.q('.session.active .spin-dot'), dump())

  // 往里打字：进队列，而不是另起一个回合 —— 那个回合正往同一个 messages.jsonl 里写。
  smoke.type('.composer-input', QUEUED)
  smoke.click('.send-btn')
  const queued = await smoke.waitFor(
    () => (smoke.qa('.pending-item').length === 1 ? smoke.qa('.pending-item')[0] : null),
    '窗口期这句进了待发送队列', 5000)
  smoke.check('窗口期发的一句是排队，不是第二个回合', !!queued && smoke.text(queued).indexOf(QUEUED) >= 0,
    smoke.text('.pending-bar'))
  smoke.check('它没有立刻作为用户消息出现在转录里', smoke.text('.chat').indexOf(QUEUED) < 0,
    smoke.text('.chat').slice(-160))

  // 那一轮跑到尾。它的正文写在父会话的键上，所以子会话的转录要先等切换（回合结束）把磁盘重读一遍，
  // 才会出现它的收尾标记 —— 这正是"读者在窗口期看的是一份静止的转录"的那一面（见手册）。
  if (!(await smoke.waitText('.chat', '窗口收尾标记', 60000))) {
    smoke.log('诊断：转录', smoke.text('.chat').slice(-200))
    return smoke.finish()
  }
  // 回合结束 → 队列自己冲出去 → 那句成为下一轮真正的请求。顺序由 Go 那一半钉住。
  if (!(await smoke.waitText('.chat', '排队那条我收到了', 40000))) {
    smoke.log('诊断：转录', smoke.text('.chat').slice(-200))
    return smoke.finish()
  }
  smoke.check('排队那句最终发出去并收到了回复', smoke.text('.chat').indexOf(QUEUED) >= 0, '')
  smoke.check('队列已清空', !smoke.q('.pending-bar'), '')

  // 两轮都跑完之后，侧栏不该留任何运行标记 —— 残留的那一个就是「两个会话同时在跑」的另一半。
  const settled = await smoke.waitGone('.spin-dot', '两轮都结束（侧栏没有运行标记残留）', 30000)
  smoke.check('回合结束后没有残留的运行标记', !!settled, '')

  smoke.finish()
})()
