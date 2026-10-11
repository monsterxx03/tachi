// managed-session-busy — 一条子系统的执行记录，**并且有一个正在跑的任务持有它**。
//
// 与 managed-session 的区别就是那份持有（fixture 写进 session 的保留记录里）：它正是"删不掉"的原因，
// 也是窗口该给出出路的地方。只读横幅上会多出一个按钮 —— 而按钮只有"确实有东西在跑"时才出现，
// 所以这条 driver 同时钉住了另一面：上一条场景里它**不该**出现。
//
// 点它是真的走一遍后端：binding → AgentService.EndSessionTask → 本机控制口。冒烟沙箱里没有节点服务，
// 于是正确答案是那句诚实的话（"本机没有可用的节点服务"），而不是静默失败 —— 只查按钮在不在的测试，
// 在 binding 接错时照样能过。
;(async () => {
  if (!(await smoke.waitForSession())) return smoke.finish()

  // ── 横幅上出现了出路 ─────────────────────────────────────────────────────
  const banner = await smoke.waitFor('.managed-banner', '只读横幅出现')
  if (!banner) return smoke.finish()
  const stop = await smoke.waitFor('.managed-banner-stop', '横幅上有结束任务的按钮')
  if (!stop) return smoke.finish()
  smoke.check('按钮写的是它做什么', smoke.text(stop).indexOf('结束正在运行的任务') >= 0, smoke.text(stop))

  // ── 点它：真的到了后端，而后端诚实回答 ────────────────────────────────────
  smoke.click('.managed-banner-stop')
  const note = await smoke.waitFor('.managed-banner-stop-note', '点击后给出了回答')
  if (!note) return smoke.finish()
  const text = smoke.text(note)
  smoke.check('本机没有节点服务时如实说明，而不是假装成功',
    text.indexOf('节点服务') >= 0, text)

  // 会话还在（点击不是删除），横幅还在，输入框依旧不存在。
  smoke.check('受管会话仍然没有输入框', smoke.qa('.composer-input').length === 0,
    smoke.qa('.composer-input').length + ' 个 .composer-input')

  smoke.finish()
})()
