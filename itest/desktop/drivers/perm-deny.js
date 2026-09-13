// perm-deny — 同一张确认卡，点「拒绝」：命令不执行，回合照常继续（模型被告知被拒）。
//
// perm-allow 的正对照。两者一起才说明规则确实命中了：只有 allow 一条时，「规则没命中、
// 命令直接跑了」也会让那条通过；只有 deny 一条时，「根本没弹卡」也会让那条通过。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '读一下那个文件')
  smoke.click('.send-btn')

  if (!(await smoke.waitFor('.perm-form', '权限确认卡出现', 30000))) return smoke.finish()

  const deny = smoke.qa('.perm-form .btn').find((b) => b.textContent.trim() === '拒绝')
  smoke.check('确认卡上有「拒绝」', !!deny, smoke.qa('.perm-form .btn').map((b) => b.textContent.trim()).join(' | '))
  smoke.click(deny)
  if (!(await smoke.waitFor(() => !smoke.q('.perm-form'), '答复后确认卡消失'))) return smoke.finish()

  const reply = await smoke.waitText('.msg-assistant', '我不执行了', 30000)
  smoke.check('拒绝后回合继续，模型给出了回复', !!reply, smoke.text('.chat-content').slice(-80))
  const status = smoke.qa('.tool-status')
  smoke.check('工具卡是失败状态（命令没跑）', status.length > 0 && status.every((s) => s.className.indexOf('err') >= 0),
    status.map((s) => s.className).join(','))
  smoke.finish()
})()
