// plan-writeback — the plan panel follows a plan whose STEP STATUSES advanced.
//
// The mock saves the same plan_id twice (all pending, then one done and one in progress),
// so this asserts the panel shows the same document moving 0/3 → 1/3 and that the disk
// holds one file. The runner checks the same two things from outside (the saved JSON, and
// the reminder in the second turn's prompt).
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '按计划来')
  smoke.click('.send-btn')
  const chip = await smoke.waitFor('.plan-chip', '计划 chip 出现', 30000)
  if (!chip) return smoke.finish()
  const first = smoke.text('.plan-chip')
  smoke.check('首次保存后 chip 显示 0/3', first.indexOf('0/3') >= 0, first)

  smoke.click('.plan-chip')
  if (!(await smoke.waitFor('.plan-panel', '计划面板打开'))) return smoke.finish()
  const steps0 = smoke.qa('.plan-step').map((s) => s.className.indexOf('is-pending') >= 0)
  smoke.check('面板里三个步骤都是待办', steps0.length === 3 && steps0.every(Boolean), smoke.text('.plan-steps').slice(0, 40))
  smoke.key(document.body, 'Escape')
  await smoke.waitFor(() => !smoke.q('.plan-panel'), 'Esc 关闭面板')

  // Second turn: same plan_id, statuses advanced.
  smoke.type('.composer-input', '继续')
  smoke.click('.send-btn')
  const advanced = await smoke.waitFor(() => smoke.text('.plan-chip').indexOf('1/3') >= 0, 'chip 走到 1/3', 30000)
  smoke.check('状态回写推进了 chip（0/3 → 1/3）', !!advanced, smoke.text('.plan-chip'))

  smoke.click('.plan-chip')
  if (!(await smoke.waitFor('.plan-panel', '面板再次打开'))) return smoke.finish()
  const cls = smoke.qa('.plan-step').map((s) => s.className.replace('plan-step is-', ''))
  smoke.check('第一步已完成、第二步进行中、第三步待办',
    cls.join(',') === 'completed,in_progress,pending', cls.join(','))
  smoke.check('面板标题还是同一份计划', smoke.text('.plan-title') === '冒烟计划', smoke.text('.plan-title'))
  smoke.finish()
})()
