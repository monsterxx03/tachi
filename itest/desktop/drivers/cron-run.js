// cron-run — 定时任务真的跑起来：种一条 `@every 2s` 的一次性任务，应用启动后调度器自己触发它。
//
// driver 只断言消息区和弹层；Go 侧断言会话文件（提示词带无人值守说明、显示的是任务名、
// 回答落盘）和 crons.json（一次性任务跑完自己删掉）。这两半合起来才证明「到点 → 跑一轮 →
// 写进会话 → 清理」这条链真的接上了。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // ── 消息区：定时任务的用户气泡 + 回答 ──────────────────────────────────
  if (!(await smoke.waitFor('.msg-user', '定时任务的用户气泡出现'))) return smoke.finish()
  const bubble = smoke.text('.msg-user')
  smoke.check('气泡写的是任务名', bubble.indexOf('⏰ 快速巡检') >= 0, bubble)
  smoke.check('气泡带上任务说明', bubble.indexOf('看一眼工作区') >= 0, bubble)

  if (!(await smoke.waitFor('.msg-assistant', '定时任务的回答占位出现'))) return smoke.finish()
  // The placeholder exists the moment the turn opens ("正在思考…"); wait for the
  // ANSWER, not for the element. `waitFor` only proves existence.
  let reply = ''
  const deadline = Date.now() + 15000
  while (Date.now() < deadline) {
    reply = smoke.text('.msg-assistant')
    if (reply.indexOf('巡检完成。') >= 0) break
    await smoke.sleep(100)
  }
  smoke.check('回答出现在消息区', reply.indexOf('巡检完成。') >= 0, reply)

  // ── 一次性任务跑完从弹层里消失 ─────────────────────────────────────────
  smoke.click('.cron-btn')
  if (!(await smoke.waitFor('.cron-panel', '弹层打开'))) return smoke.finish()
  if (!(await smoke.waitFor('.cron-empty', '任务列表为空'))) return smoke.finish()
  smoke.check('一次性任务跑完从列表里消失', smoke.qa('.cron-job').length === 0,
    smoke.allText('.cron-job-name').join(' | '))
  smoke.check('空列表说得清', smoke.text('.cron-empty-title') === '还没有定时任务', smoke.text('.cron-empty-title'))

  smoke.finish()
})()
