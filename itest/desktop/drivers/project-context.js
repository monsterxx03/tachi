// project-context — 会话工作目录里的 .tachi.md 会被注入，跟着它一起来的还有那段「规矩」。
//
// 这一侧只钉 UI 不炸：挂载、首条消息能被发出、回复落地。注入的内容与规矩有没有到达模型，由 Go
// 那一半在 LLM 边界断言（两处标记 + 顺序）——那才是能证明"契约随着内容旅行"的地方。
//
// 这里刻意不断言 ! 提醒入口：`Message.reminder` 只在 buildTurns（历史回放）里被设置，所以实时
// 跑完的那一轮不挂这个按钮，要等会话重建（切走再切回、或重启）才看得到。那是所有系统提醒共通
// 的既有行为，不是这条注入路径的性质，钉在这里只会钉住一个与主题无关的事实。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '看下项目约定')
  smoke.click('.send-btn')

  const reply = await smoke.waitText('.msg-assistant', '只改 NOTES.md', 30000)
  smoke.check('首条消息正常跑完（带了项目上下文的回合）', !!reply, smoke.text('.chat-content').slice(-60))

  // 用户的那条消息必须在场：提醒是挂在它上面的，一条被吞掉的首条消息会让 Go 侧那两处断言
  // 变成"模型收到了没人发过的东西"。
  smoke.check('用户消息在场', smoke.text('.chat-content').indexOf('看下项目约定') >= 0, '')
  smoke.finish()
})()
