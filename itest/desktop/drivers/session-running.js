// session-running — the stop control (and the status behind it) belongs to the session that is
// actually running, and a background turn's END has to reach that session.
//
// The report: with a turn in flight, switching to another session — or starting a new one — left
// the red stop ring spinning in a session that had nothing to stop; it never cleared once the
// running turn was over, and clicking it did nothing (Stop acts on the DISPLAYED session, which
// was idle). One cause: the backend pushed "what the agent is doing" only while THAT session was
// the displayed one, and the frontend kept the single value it received as a global — so a
// background turn's end was never reported at all, and the stale busy value answered for every
// conversation the reader looked at. The desktop runs each session's turn in its own goroutine
// (switching cancels nothing), so a status has to be per session in both directions.
//
// Every judgement below is about the session ON SCREEN, which is the only thing a reader sees:
//   · 切到一个没在跑的会话 → 没有停止按钮            (改前失败：全局状态残留 busy)
//   · 后台那一轮结束后，屏幕上仍然没有停止按钮         (改前失败：结束事件根本没发出来)
//   · 切回跑完的会话 → 没有停止按钮，回复还在          (改前失败)
//   · 运行中新建会话 → 新会话干净，老会话仍带运行标记    (两边都成立才算对)
//
// The turn is held open with mock pauses on purpose: without a running turn there is nothing to
// leak into the next session. The switch back and forth uses titles/ids on purpose — the second
// session is visited once BEFORE the turn starts, so switching to it later takes the CACHED path
// (ActivateSession, no reload), which is the path the report came from.
;(async () => {
  // 当前显示的是哪个会话：标题栏不再显示 id 了，读 app 根上的 data-session-id（唯一稳定的把手 ——
  // 标题做不到这件事，第二个会话一建出来「未命名会话」就会重复）。
  const sessionId = () => { const el = smoke.q('.app'); return (el && el.getAttribute('data-session-id')) || '' }
  const rowByTitle = (t) => smoke.qa('.session').find((s) => smoke.text(s.querySelector('.session-title')) === t)

  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()
  // The displayed session's id is the switch's own fact (unique per session, unlike a title):
  // 未命名会话 repeats as soon as a second session is created. The titlebar stopped showing it, so
  // the app root carries it as data-session-id.
  if (!(await smoke.waitFor(() => (sessionId() ? true : null), '会话加载完成（app 上出现 data-session-id）'))) return smoke.finish()
  const fixtureId = sessionId()
  const fixtureTitle = smoke.text('.session.active .session-title')

  if (!smoke.click('.proj-head.is-loose .proj-add')) return smoke.fail('点击新建会话', '按钮找不到')
  if (!(await smoke.waitFor(() => sessionId() !== fixtureId, '新建会话', 10000))) return smoke.finish()
  const otherId = sessionId()
  const otherTitle = smoke.text('.session.active .session-title')
  smoke.check('新建会话里没有停止按钮', !smoke.q('.stop-btn'), '')

  // Back to the fixture session and send: this turn is the one that must not follow the reader
  // into any other conversation.
  if (!smoke.click(rowByTitle(fixtureTitle))) return smoke.fail('在侧边栏找到 fixture 会话', fixtureTitle)
  if (!(await smoke.waitFor(() => sessionId() === fixtureId, '切回 fixture 会话', 10000))) return smoke.finish()
  smoke.type('.composer-input', '跑一个长回合')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.stop-btn', '回合开始（停止按钮出现）', 15000))) return smoke.finish()
  smoke.check('运行中的会话在侧栏带运行标记', !!smoke.q('.session.active .spin-dot'), '')

  // Away, to the session that is NOT running.
  if (!smoke.click(rowByTitle(otherTitle))) return smoke.fail('在侧边栏找到第二个会话', otherTitle)
  if (!(await smoke.waitFor(() => sessionId() === otherId, '切到没在跑的会话', 10000))) return smoke.finish()
  const away = await smoke.waitGone('.stop-btn', '没在跑的会话里停止按钮消失', 3000)
  smoke.check('切到没在跑的会话后停止按钮消失', !!away,
    '残留的按钮会一直转，而且点它没有任何效果（Stop 作用在屏幕上的会话）')

  // The background turn ends while THIS session is on screen. The sidebar marker is the fact
  // (it clears on the run's own end event), so wait for it before judging the composer.
  const ended = await smoke.waitGone('.spin-dot', '后台回合结束（侧栏运行标记消失）', 30000)
  smoke.check('后台回合结束', !!ended, '')
  smoke.check('后台回合结束后屏幕上没有停止按钮', !smoke.q('.stop-btn'), '')

  // Back to the session that ran: its turn is over, so no stop control — and the reply it
  // streamed while nobody was looking is still in its transcript.
  if (!smoke.click(rowByTitle(fixtureTitle))) return smoke.fail('在侧边栏找到运行过的会话', fixtureTitle)
  if (!(await smoke.waitFor(() => sessionId() === fixtureId, '切回运行过的会话', 10000))) return smoke.finish()
  const back = await smoke.waitGone('.stop-btn', '跑完的会话里停止按钮消失', 3000)
  smoke.check('切回跑完的会话没有停止按钮', !!back, '')
  const transcript = smoke.text('.chat')
  smoke.check('后台跑完的回复留在会话里', transcript.indexOf('长回合收尾标记') >= 0, transcript.slice(-140))

  // The report's other half: a NEW session while a turn is running. The new conversation is idle,
  // so it must not show its neighbour's turn — and the neighbour must keep its own marker.
  smoke.type('.composer-input', '再来一个短回合')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.stop-btn', '第二个回合开始', 15000))) return smoke.finish()
  const runningId = sessionId()
  if (!smoke.click('.proj-head.is-loose .proj-add')) return smoke.fail('运行中点击新建会话', '按钮找不到')
  if (!(await smoke.waitFor(() => sessionId() !== runningId, '运行中切到新会话', 10000))) return smoke.finish()
  smoke.check('运行中新建会话：新会话没有停止按钮', !smoke.q('.stop-btn'), '')
  smoke.check('运行中的会话仍带运行标记', !!smoke.q('.spin-dot'), '')

  // Let the second turn finish before reporting: the app is killed right after, and a reply cut
  // off mid-stream would leave the mock's script half consumed.
  await smoke.waitGone('.spin-dot', '第二个回合结束', 20000)
  smoke.finish()
})()
