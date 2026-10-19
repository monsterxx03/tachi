// perm-files — 一次命中 `permissions.files.read.ask` 的读文件，用户点「允许一次」后真的读了。
//
// 这一侧断言 UI：卡片出现，且显示 agent 渲染的预览（工具 + 路径 + 命中规则）；三个选项都在；
// 点击后表单消失（后端收下了答复）。而 kind=files 的文案是这套东西里最容易悄悄退回 bash 文案的
// 地方（卡片里既没有命令也没有 bash 规则，而拒绝的语义是「只拒这一次调用」而不是中止这一轮），
// 所以这里连副标题一起断言。
//
// 「文件真的读了」「回合没有中止」属于 Go 那一半（scenarios.go 的 after）。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  smoke.type('.composer-input', '读一下那份 fixture')
  smoke.click('.send-btn')

  // 回合被 park 在这里：卡片要出现，而且要显示 agent 自己渲染的那段内容（工具 + 路径 + 命中规则）。
  if (!(await smoke.waitFor('.perm-form', '文件权限确认卡出现', 30000))) return smoke.finish()
  const preview = smoke.text('.perm-preview')
  smoke.check('确认卡显示要读的目标', preview.indexOf('perm-fixture.txt') >= 0, preview)
  smoke.check('确认卡显示命中的 ask 规则', preview.indexOf('perm-fixture*') >= 0, preview)

  // kind=files 的文案：标题与其他卡一样是「权限确认」，差别在副标题——这里拒绝只拒
  // 这一次调用，本轮继续（bash 的拒绝会中止这一轮），所以那句话不能退回 bash 的写法。
  const sub = smoke.text('.perm-sub')
  smoke.check('副标题说的是拒绝这次调用', sub.indexOf('这一次调用') >= 0, sub)

  const labels = smoke.qa('.perm-form .btn').map((b) => b.textContent.trim())
  smoke.check('三个选项：拒绝 / 本会话都允许 / 允许一次',
    labels.length === 3 && labels[0].indexOf('拒绝') >= 0 && labels[1].indexOf('本会话都允许') >= 0 && labels[2].indexOf('允许一次') >= 0,
    labels.join(' | '))

  const allow = smoke.qa('.perm-form .btn').find((b) => b.textContent.trim() === '允许一次')
  smoke.click(allow)
  if (!(await smoke.waitFor(() => !smoke.q('.perm-form'), '答复后确认卡消失'))) return smoke.finish()

  // 工具卡接着跑完，回合继续到模型的下一条回复。
  const reply = await smoke.waitText('.msg-assistant', '读过那份文件了', 30000)
  smoke.check('允许后回合继续，模型给出了回复', !!reply, smoke.text('.chat-content').slice(-80))
  smoke.finish()
})()
