// perm-unparseable — 命令解析不出来时的确认卡，说的是真话。
//
// 政策对"解析不了"的命令（末尾挂着 && 、引号没闭合这类）保守地问一次，这条路径**没有命中任何规则**
// —— 卡片上那句「命中 permissions.bash.ask 规则」曾经是写死的，预览又把哨兵值当规则名念出来
// （`matched ask rule: "command could not be parsed"`），于是读者看到的是"一条我没写过的规则 +
// 一条看着像被截断的命令"，就报成了渲染 bug（issue #40）。
//
// 这个场景的下半张卡（`ls -la | head && `）是刻意造的：它真的解析不了，config 里那条 ask 规则与它
// 无关，所以"卡片**没有**提那条规则"本身就是断言。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  smoke.type('.composer-input', '看一眼目录')
  smoke.click('.send-btn')

  if (!(await smoke.waitFor('.perm-form', '权限确认卡出现', 30000))) return smoke.finish()
  const preview = smoke.text('.perm-preview')
  const head = smoke.text('.perm-head')

  smoke.check('确认卡照原样显示那条命令（末尾的 && 也在）',
    preview.indexOf('ls -la | head &&') >= 0, JSON.stringify(preview))
  smoke.check('卡片说出真正的原因：命令解析不出来', preview.indexOf('没能解析出来') >= 0, JSON.stringify(preview))
  smoke.check('不把自己说成"命中了某条规则"（没有任何规则被咨询过）',
    preview.indexOf('命中 ask 规则') < 0, JSON.stringify(preview))
  smoke.check('预览里不出现机器面向的哨兵值', preview.indexOf('no-rule:unparseable') < 0,
    JSON.stringify(preview))
  smoke.check('卡片那句副标题也不再断言"命中了 ask 规则"',
    head.indexOf('命中') < 0 && head.indexOf('拒绝') >= 0, JSON.stringify(head))
  smoke.check('与命令无关的那条 ask 规则没有被念出来（它真的没参与）',
    preview.indexOf('perm-fixture') < 0, JSON.stringify(preview))

  const deny = smoke.qa('.perm-form .btn').find((b) => b.textContent.trim() === '拒绝')
  if (!deny) return smoke.fail('卡上有「拒绝」', smoke.qa('.perm-form .btn').map((b) => smoke.text(b)).join(' | '))
  smoke.click(deny)
  if (!(await smoke.waitFor(() => (!smoke.q('.perm-form') ? true : null), '答复后确认卡消失'))) return smoke.finish()
  smoke.check('拒绝后回合继续（模型给出下一条回复）',
    !!(await smoke.waitText('.msg-assistant', '不跑了', 30000)), smoke.text('.chat-content').slice(-80))

  smoke.finish()
})()
