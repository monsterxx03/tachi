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

  // ── 时间那一栏问两个互不重叠的问题 ────────────────────────────────────
  // 「做几次」是一个分段（反复 / 只一次），「什么频率」是第二个分段，只在反复运行时出现。
  // 早先它们和一个「只跑一次」的勾选框混在一起，于是「每 N 小时」旁边挂着「只跑一次」，
  // 而「自定义」和「高级」各有一个 cron 输入框。这三条断言就是那次改版的形状。
  const kindSeg = smoke.text('.cron-seg[aria-label="运行方式"]')
  smoke.check('「做几次」是一个分段', !!smoke.q('.cron-seg[aria-label="运行方式"]'), '')
  smoke.check('分段里有反复运行', kindSeg.indexOf('反复运行') >= 0, kindSeg)
  smoke.check('分段里有只运行一次', kindSeg.indexOf('只运行一次') >= 0, kindSeg)
  smoke.check('不再有「只跑一次」勾选框', smoke.qa('.cron-check').length === 0, '')
  smoke.check('高级里不再有第二个 cron 输入框',
    smoke.qa('.cron-adv .cron-cron-expr').length === 0, smoke.text('.cron-adv'))

  // 切到「只运行一次」：频率分段收起来，换成日期 + 时刻，预览仍然给得出下次运行
  const onceBtn = smoke.qa('.cron-seg button').find((b) => smoke.text(b) === '只运行一次')
  if (!onceBtn) return smoke.fail('有「只运行一次」按钮', smoke.text('.cron-field'))
  smoke.click(onceBtn)
  await smoke.sleep(60)
  smoke.check('只跑一次时没有频率分段', !smoke.q('.cron-seg[aria-label="运行频率"]'), '')
  smoke.check('只跑一次给出日期输入', !!smoke.q('.cron-date'), '')
  smoke.check('只跑一次仍然有下次运行', smoke.text('.cron-preview').indexOf('下次运行') >= 0, smoke.text('.cron-preview'))
  const backBtn = smoke.qa('.cron-seg button').find((b) => smoke.text(b) === '反复运行')
  if (!backBtn) return smoke.fail('能切回反复运行', smoke.text('.cron-field'))
  smoke.click(backBtn)
  await smoke.sleep(60)
  smoke.check('切回反复运行后频率分段回来了', !!smoke.q('.cron-seg[aria-label="运行频率"]'), '')

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

  // 弹层里也能删：先问一次（就在这一行里），取消之后那排动作回来。这里不真的删 ——
  // Go 那一半还要读 crons.json。
  const del = smoke.qa('.cron-job .cron-danger')[0]
  smoke.check('弹层里有删除键', !!del, '')
  if (del) {
    smoke.click(del)
    await smoke.sleep(60)
    smoke.check('删除先问一次', smoke.text('.cron-confirm-row').indexOf('删除「') >= 0, smoke.text('.cron-confirm-row'))
    const cancel = smoke.qa('.cron-confirm-row .btn').find((b) => smoke.text(b) === '取消')
    if (!cancel) return smoke.fail('确认条里有取消', smoke.text('.cron-confirm-row'))
    smoke.click(cancel)
    await smoke.sleep(60)
    smoke.check('取消之后回到那排动作', !!smoke.q('.cron-job .cron-danger'), '')
  }

  smoke.finish()
})()
