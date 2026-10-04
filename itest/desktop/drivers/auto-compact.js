// auto-compact — the compaction the READER never asked for: the agent loop compacts in-flight
// when the context estimate crosses the threshold, with no /compact typed anywhere. The
// conversation moves into a child session mid-turn, and the sidebar has to show the same
// folded chain the typed command produces (one row per conversation, the pre-compaction
// session under it as 「压缩前」).
//
// This is its own scenario because the two paths differ in exactly the way that matters: a
// typed /compact ends the command's turn with the record already set, while auto-compaction
// swaps the session UNDER a running turn and only re-homes the run when that turn ends. The
// sidebar is the surface that tells them apart — the reader is looking at the new session
// while the row still belongs to the old one.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const rows = () => smoke.qa('.session')
  const dump = () => rows().map((r) => smoke.text(r)).join(' | ')
  // Wait for the sidebar's row before typing: the composer exists a moment earlier, and a
  // send whose text never reached React's state hits a DISABLED button — no turn, no request,
  // and a report that reads like a broken app.
  if (!(await smoke.waitFor(() => (rows().length === 1 ? rows()[0] : null), '侧栏出现这一个会话'))) {
    return smoke.finish()
  }
  smoke.check('压缩前侧栏只有一个会话', rows().length === 1, rows().length + ' 行')

  // The turn compacts on its first iteration (the scenario's threshold is below the floor of
  // any prompt), then keeps going in the child session and answers.
  smoke.type('.composer-input', '随便说一句')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.chat', '压缩之后我还能回答', 30000))) {
    smoke.log('诊断：侧栏行', dump())
    return smoke.finish()
  }

  // The signal to wait for is the FOLD, not the 「对话已压缩」 notice: the notice is written
  // into the parent's transcript, so the session switch that follows takes it off screen with
  // it. What has to survive the switch is the sidebar's row for the conversation.
  const chain = await smoke.waitFor(
    () => (smoke.qa('.session-chain').length === 1 ? smoke.qa('.session-chain')[0] : null),
    '侧栏出现「压缩前」折叠入口', 15000)
  if (!chain) {
    smoke.log('诊断：侧栏行', dump())
    return smoke.finish()
  }
  smoke.check('压缩后侧栏仍是一行对话（父会话折起来了）', rows().length === 1, dump())
  smoke.check('折叠入口报出压缩前的节数', smoke.text(chain).indexOf('压缩前 1 节') >= 0, smoke.text(chain))

  // The folded half is reachable, and marked as such.
  chain.click()
  const shown = await smoke.waitFor(
    () => (smoke.qa('.session.is-compacted').length === 1 ? smoke.qa('.session.is-compacted')[0] : null),
    '展开后出现「压缩前」的会话行', 5000)
  smoke.check('展开后能看到压缩前的会话', !!shown, shown ? smoke.text(shown) : '')
  smoke.check('压缩前的行标注了自己的身份', !!shown && smoke.text(shown).indexOf('压缩前') >= 0,
    shown ? smoke.text(shown) : '')
  smoke.check('展开不改变对话行数（只是多了一条子行）', rows().length === 2, rows().length + ' 行')

  smoke.finish()
})()
