// skill-catalog — 会话所在树的 .tachi/skills 会被发现，技能目录随第一条消息进入模型上下文。
//
// 重心在 Go 那一半：一个 skill 被用掉之前，界面上没有任何东西能看出它存在，所以「目录真的被
// 扫出来了」只能读 mock 收到的请求（那里能看见 <skill name=.../> 那一行）。这一侧证的是
//「这件事没有打扰任何东西」—— 目录是被动注入的上下文，不该冒出工具卡或确认卡。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '打个招呼')
  smoke.click('.send-btn')

  const reply = await smoke.waitText('.msg-assistant', '收到。', 30000)
  smoke.check('这一轮正常跑完', !!reply, smoke.text('.chat-content').slice(-70))

  const tools = smoke.qa('.tool-status').length
  smoke.check('技能目录只是上下文：没有工具卡，也没有确认卡',
    tools === 0 && smoke.qa('.perm-form').length === 0, tools + ' 张工具卡')
  smoke.finish()
})()
