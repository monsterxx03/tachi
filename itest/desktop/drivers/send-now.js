// send-now — interrupt a running turn with a queued message (「立即发送」).
//
// This is the ordering fix in docs/agents/desktop.md's history: the interrupted turn's terminal event
// targets "the newest running assistant", so a placeholder placed before it was the one
// marked 已停止 and its own reply was dropped. Asserted from the UI here and from the
// prompt by the runner.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '第一件事：跑很久')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.stop-btn', '第一轮开始运行（停止按钮出现）'))) return smoke.finish()
  smoke.check('运行中显示停止按钮', true)

  // Queue a second message while the turn is still running.
  smoke.type('.composer-input', '插队：先做这件')
  smoke.click('.send-btn')
  const bar = await smoke.waitFor('.pending-bar', '待发送队列出现')
  if (!bar) return smoke.finish()
  smoke.check('运行中发送进入待发送队列', true, smoke.text('.pending-bar').slice(0, 30))

  const sendNow = smoke.q('.pending-send')
  if (!sendNow) return smoke.fail('立即发送按钮存在', '找不到 .pending-send')
  smoke.click(sendNow)

  // The reply to the queued message has to land: that is the whole point.
  if (!(await smoke.waitText('.msg-assistant .msg-content', '插队那条的回复', 30000))) return smoke.finish()
  smoke.check('打断后排队的消息拿到回复', true)

  // Exactly one 已停止 — the interrupted turn's. A second one means the new placeholder
  // was hit by the old turn's terminal event.
  const stopped = smoke.qa('.stopped-note').length
  smoke.check('只有被中断那一轮标了「已停止」', stopped === 1, stopped + ' 处')

  const users = smoke.allText('.msg-user')
  smoke.check('两条用户消息都在（插队那条在新回合前）',
    users.length >= 2 && users[users.length - 1].indexOf('插队') >= 0, JSON.stringify(users.slice(-2)))
  smoke.finish()
})()
