// orphan-tool-call — 进程死在工具批次中间之后重启：重建出来的历史里那条没有结果的 tool call 必须
// 被丢掉，会话还能继续用。
//
// 现场由 Go 侧 fixture 预置（scenarios.go 的 orphan-tool-call）：一条 user + 两条 Bash call + 只有
// 一条 result —— 正是「先把 call 全记下、再逐个记 result」的落盘顺序被 kill 在中间留下的形状。harness
// 无法真的重启 app，而「重启后第一次从磁盘重建历史」正是这份 seed 造出来的东西。
//
// 断言在线路那一侧（Go 侧读 mock 收到的请求）：孤儿 call 上不上线，屏幕上本来就看不出区别 —— 真
// provider 会直接拒掉那个请求，而这里的 mock 照收不误。driver 只负责把这条历史推上线（发一句话），
// 并确认预置的那轮对话在屏幕上确实起来了。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // 预置的会话由磁盘渲染：先确认它真的起来了，再看后面那一轮。
  if (!(await smoke.waitFor('.msg-user', '预置的会话渲染出用户气泡', 15000))) return smoke.finish()
  smoke.check('气泡显示预置的那句话', smoke.text('.msg-user').indexOf('两条都跑一下') >= 0,
    smoke.text('.msg-user').slice(0, 120))

  smoke.type('.composer-input', '继续')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.chat', '好，接着来', 30000))) return smoke.finish()
  smoke.finish()
})()
