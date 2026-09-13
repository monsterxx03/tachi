// perm-session — 「本会话全部允许」之后的第二条命令不再弹卡。
//
// 两次调用的命令**不同**、但命中同一条 ask 规则：这才是这个选项要解决的场景（agent 造出来的命令
// 不会逐字重复）。Go 那一半证"两条命令都真的跑了、三步都走到了"；这一侧证"只见到一张卡"。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '读两个文件')
  smoke.click('.send-btn')

  if (!(await smoke.waitFor('.perm-form', '第一条命令的确认卡出现', 30000))) return smoke.finish()
  const labels = smoke.qa('.perm-form .btn').map((b) => b.textContent.trim())
  smoke.check('确认卡有三个选项，中间那个是整个会话的',
    labels.length === 3 && labels[1] === '本会话全部允许', labels.join(' | '))

  const allowAll = smoke.qa('.perm-form .btn').find((b) => b.textContent.trim() === '本会话全部允许')
  smoke.click(allowAll)
  if (!(await smoke.waitFor(() => !smoke.q('.perm-form'), '答复后确认卡消失'))) return smoke.finish()

  // 第二条命令也命中同一条规则：如果"全部允许"没生效，它会停在那里等人，这一轮就永远跑不完。
  const reply = await smoke.waitText('.msg-assistant', '两个文件都读完了', 30000)
  smoke.check('第二条命令没有被打断，回合跑到了结尾', !!reply, smoke.text('.chat-content').slice(-70))
  smoke.check('全程只出现过一张确认卡（第二条没有再问）', smoke.qa('.perm-form').length === 0,
    smoke.qa('.perm-form').length + ' 张卡还在')

  const status = smoke.qa('.tool-status')
  smoke.check('两次调用都是成功状态', status.length === 2 && status.every((s) => s.className.indexOf('ok') >= 0),
    status.map((s) => s.className).join(','))
  smoke.finish()
})()
