// perm-allow — 一条命中 `ask` 规则的 bash 命令，用户点「允许一次」后真的执行了。
//
// 三件事在这一侧断言：确认卡出现（连带它显示的命令与命中规则）、三个选项都在、
// 点击后表单消失（说明后端收下了这个答复）。「命令真的跑了」属于 Go 那一半——这里
// 看得到的是 UI，那里看得到的是模型收到的工具结果。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '读一下那个文件')
  smoke.click('.send-btn')

  // 回合被 park 在这里：卡片要出现，而且要显示 agent 自己渲染的那段内容（命令 + 命中规则）。
  if (!(await smoke.waitFor('.perm-form', '权限确认卡出现', 30000))) return smoke.finish()
  const preview = smoke.text('.perm-preview')
  smoke.check('确认卡显示要执行的命令', preview.indexOf('cat perm-fixture.txt') >= 0, preview)
  smoke.check('确认卡显示命中的 ask 规则', preview.indexOf('cat perm-fixture*') >= 0, preview)

  const labels = smoke.qa('.perm-form .btn').map((b) => b.textContent.trim())
  smoke.check('三个选项：拒绝 / 本会话全部允许 / 允许一次',
    labels.length === 3 && labels[0].indexOf('拒绝') >= 0 && labels[1].indexOf('本会话全部允许') >= 0 && labels[2].indexOf('允许一次') >= 0,
    labels.join(' | '))

  const allow = smoke.qa('.perm-form .btn').find((b) => b.textContent.trim() === '允许一次')
  smoke.click(allow)
  // 表单消失 = 后端收下了这个答复（前端只在 AnswerPermission 返回 ok 之后才清）。
  if (!(await smoke.waitFor(() => !smoke.q('.perm-form'), '答复后确认卡消失'))) return smoke.finish()

  // 工具卡接着跑完，回合继续到模型的下一条回复。
  const reply = await smoke.waitText('.msg-assistant', '读过文件了', 30000)
  smoke.check('允许后回合继续，模型给出了回复', !!reply, smoke.text('.chat-content').slice(-80))
  // 成功的一步现在收在过程条里（默认折叠），断言卡的状态前先展开——这类"先展开再断言"
  // 是本设计给 smoke 带来的连带改动，与 perm-deny 不同（被拒的卡是失败步，永不折叠）。
  const strip = smoke.q('.process-head')
  if (!strip) return smoke.fail('过程条存在', '找不到 .process-head')
  smoke.click(strip)
  if (!(await smoke.waitFor(() => smoke.qa('.tool-status').length > 0, '展开后工具卡出现'))) return smoke.finish()
  const status = smoke.qa('.tool-status')
  smoke.check('工具卡是成功状态', status.length > 0 && status.every((s) => s.className.indexOf('ok') >= 0),
    status.map((s) => s.className).join(','))
  smoke.finish()
})()
