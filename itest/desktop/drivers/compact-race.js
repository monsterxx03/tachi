// compact-race — /compact on a conversation that is ALREADY over the compaction threshold, i.e.
// the state every real /compact is typed in. The run must produce exactly ONE child session.
//
// The interesting half is not on screen at all: the loop used to auto-compact the compact run's OWN
// turn (the /compact turn is an ordinary turn whose message is the compact instruction, and its
// history is over the threshold by definition), which replaced that history with a summary and sent
// the model a request with no instruction in it. Its reply was then stored as "the summary", so the
// reader got a child of a child. The Go half of this scenario counts the sessions and reads the
// child's first message (see the scenario's `after`); the driver's job is to do what a user does —
// two turns and /compact — and to check that the UI ends on ONE conversation.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  smoke.type('.composer-input', '看一下工作目录')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.chat', '第一轮回复', 30000))) return smoke.finish()

  smoke.type('.composer-input', '继续')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.chat', '这是一段很长的历史内容', 30000))) return smoke.finish()

  smoke.type('.composer-input', '/compact')
  smoke.click('.send-btn')

  // Wait for the FOLD, not for a notice: the notice is written into the parent's transcript and the
  // switch to the child takes it off screen with it (see compact.js).
  const childRow = await smoke.waitFor(
    () => (smoke.qa('.session-chain').length === 1 ? smoke.qa('.session-chain')[0] : null),
    '侧栏出现「压缩前」折叠入口', 30000)
  if (!childRow) {
    smoke.log('诊断：侧栏行', smoke.qa('.session').map((r) => smoke.text(r)).join(' | '))
    return smoke.finish()
  }

  smoke.check('压缩后侧栏仍是一行对话（父会话折起来了）', smoke.qa('.session').length === 1,
    smoke.qa('.session').map((r) => smoke.text(r)).join(' | '))
  // 一段链上的折叠入口只有一个：两层折叠入口就是两个子会话，也就是那个 bug。
  smoke.check('折叠入口只有一层', smoke.qa('.session-chain').length === 1,
    smoke.qa('.session-chain').length + ' 个折叠入口')
  smoke.check('折叠入口报出压缩前的节数', smoke.text(childRow).indexOf('压缩前 1 节') >= 0, smoke.text(childRow))

  smoke.finish()
})()
