// image-reload — 重启后加载会话：记录里带图的那条消息，重新发出去的请求里必须还带着那张图。
//
// 现场由 Go 侧 fixture 预置（scenarios.go 的 image-reload）：会话记录里躺着一条带 image part 的
// 用户消息，图片字节内联在记录上、走 store.AppendMessage 落盘 —— 那是真实写入的同一条路，图片会被
// 搬进会话目录的 images/，记录只留一个引用。harness 无法真的重启 app，而"重启后第一次从磁盘重建
// 历史"正是这份 seed 造出来的东西。
//
// 这个 driver 只负责把这条重建出来的历史推上线（发一句话），断言在线路那一侧：图片 part 在归一化
// 视图里看不见（mockllm 只保留文本），所以 Go 侧查的是请求的原始 body。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  // 预置的会话由磁盘渲染：先确认它真的起来了，再看后面那一轮。
  if (!(await smoke.waitFor('.msg-user', '预置的会话渲染出用户气泡', 15000))) return smoke.finish()
  smoke.check('气泡显示预置的那句话', smoke.text('.msg-user').indexOf('这张图是什么') >= 0,
    smoke.text('.msg-user').slice(0, 120))

  smoke.type('.composer-input', '那再确认一次')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.chat', '图还在', 30000))) return smoke.finish()
  smoke.finish()
})()
