// cron-page — 定时任务的完整页：从侧栏入口进去，新建一条任务，保存后左栏出现，
// 弹层里也看得到。
//
// 磁盘那一半由 Go 侧断言（scenarios.go 的 cron-page 读 crons.json）：名字、计划、
// 提示词、目标是一个逻辑会话。driver 只说界面 —— 入口在不在、表单默认落在哪个会话、
// 保存后列表和弹层有没有跟上。这条场景不触发运行，所以不会有模型请求。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // ── 侧栏搜索旁的时钟：完整页的入口 ──────────────────────────────────────
  const entry = smoke.qa('.sidebar-actions .sidebar-search')
    .find((b) => b.getAttribute('aria-label') === '定时任务')
  if (!entry) return smoke.fail('侧栏搜索旁有定时任务入口', smoke.allText('.sidebar-actions'))
  smoke.click(entry)
  if (!(await smoke.waitFor('.cron-view', '定时任务页出现'))) return smoke.finish()

  // ── 新建：表单默认落在「在这个会话里继续」 ─────────────────────────────
  if (!(await smoke.waitFor('.cron-new:not([disabled])', '「＋ 新建任务」可用'))) return smoke.finish()
  smoke.click('.cron-new')
  if (!(await smoke.waitFor('.cron-form', '编辑表单出现'))) return smoke.finish()

  const chosen = smoke.text('.cron-choice.is-on')
  smoke.check('默认「在这个会话里继续」', chosen.indexOf('在这个会话里继续') >= 0, chosen)
  smoke.check('默认给出下次运行时间', smoke.text('.cron-preview').length > 0, smoke.text('.cron-preview'))

  // ── 填名字与内容，保存 ──────────────────────────────────────────────────
  if (!smoke.type('[data-field="name"]', '冒烟巡检')) return smoke.fail('有名称输入框', '')
  if (!smoke.type('[data-field="prompt"]', '看一眼 CI 状态，没有变化就说没有变化。')) {
    return smoke.fail('有任务内容输入框', '')
  }
  smoke.click('.cron-form-actions .btn.primary')

  if (!(await smoke.waitFor('.cron-nav-row', '新任务出现在左栏'))) return smoke.finish()
  const names = smoke.allText('.cron-nav-name')
  smoke.check('左栏出现刚建的任务', names.indexOf('冒烟巡检') >= 0, names.join(' | '))
  smoke.check('保存后提示已创建', smoke.text('.cron-notice.is-ok').indexOf('已创建') >= 0, smoke.text('.cron-notice.is-ok'))

  // ── 回对话：Esc 和返回按钮同一条路 ─────────────────────────────────────
  smoke.key(document.body, 'Escape')
  await smoke.sleep(100)
  if (!(await smoke.waitFor('.cron-btn', 'Esc 回到对话（状态栏时钟在）'))) return smoke.finish()
  smoke.check('Esc 之后完整页已经关掉', !smoke.q('.cron-view'), '')

  // The count is derived from the session list, and the save event refreshes it
  // asynchronously; poll rather than read once.
  let rowBadge = ''
  const badgeDeadline = Date.now() + 3000
  while (Date.now() < badgeDeadline) {
    rowBadge = smoke.text('.session.active .session-cron')
    if (rowBadge.indexOf('1') >= 0) break
    await smoke.sleep(50)
  }
  smoke.check('会话行徽章显示任务数', rowBadge.indexOf('1') >= 0, rowBadge)
  const statusCount = smoke.text('.cron-btn .cron-btn-count')
  smoke.check('状态栏时钟角标显示本会话的任务数', statusCount === '1', statusCount)

  smoke.click('.cron-btn')
  if (!(await smoke.waitFor('.cron-panel', '状态栏弹层打开'))) return smoke.finish()

  const panelNames = smoke.allText('.cron-job-name')
  smoke.check('弹层里能看到刚建的任务', panelNames.indexOf('冒烟巡检') >= 0, panelNames.join(' | '))
  const hint = smoke.text('.cron-foot-hint')
  smoke.check('弹层写了退出后不补跑', hint.indexOf('不会补跑') >= 0, hint)

  smoke.finish()
})()
