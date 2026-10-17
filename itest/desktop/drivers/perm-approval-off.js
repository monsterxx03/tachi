// perm-approval-off — 状态栏里那个审批模式下拉框切到「无需审批」之后，bash 与文件工具的 ask 都不再弹卡。
//
// 前提必须先立住：第一轮用**默认**的模式做一次命中文件 read ask 规则的读取，卡片要出现（否则"没有
// 弹卡"可能只是规则没命中，什么也证明不了）。然后切成「无需审批」，第二轮里一条命中 bash ask 规则的
// 命令和一次命中文件规则的读取都不该再打断这一轮。
//
// 「写进会话记录」属于 Go 那一半（meta.json），这里看得到的是 UI 与这一轮有没有被挂住。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const sel = () => smoke.q('.approval-select')
  if (!sel()) return smoke.fail('状态栏里有审批模式下拉框', '找不到 .approval-select')
  smoke.check('默认是「需要审批」（现状）', sel().value === 'ask', sel().value)
  const options = Array.prototype.map.call(sel().options, (o) => o.textContent.trim())
  smoke.check('两个选项：需要审批 / 无需审批',
    options.length === 2 && options[0] === '需要审批' && options[1] === '无需审批', options.join(' | '))

  // 第一轮：默认模式下命中文件 ask 规则的读取确实会问 —— 这是第二轮那个对照的前提。卡片上那句话是
  // agent 写的预览（工具 + 路径），所以"这张卡说的是这次读"能一并断出来。
  smoke.type('.composer-input', '先读一次那个文件')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.perm-form', '默认模式下命中文件 ask 规则时弹出确认卡', 30000))) return smoke.finish()
  const preview = smoke.text('.perm-preview')
  smoke.check('卡片说的是这次读取（工具 + 路径）', preview.indexOf('perm-fixture-read.txt') >= 0, preview)
  const allow = smoke.qa('.perm-form .btn').find((b) => b.textContent.trim() === '允许一次')
  smoke.click(allow)
  if (!(await smoke.waitFor(() => !smoke.q('.perm-form'), '答复后确认卡消失'))) return smoke.finish()
  const first = await smoke.waitText('.msg-assistant', '读过文件了', 30000)
  smoke.check('第一轮跑完了（规则确实会问）', !!first, smoke.text('.chat-content').slice(-80))

  // 切成「无需审批」：值变成 skip 说明后端收下了这次写入（前端只在 SetSkipApproval 返回 ok 之后才改
  // 自己的状态；被拒会重读，那读回来的还是 ask）。
  smoke.pick('.approval-select', 'skip')
  const off = await smoke.waitFor(() => sel() && sel().value === 'skip', '下拉框切到「无需审批」')
  smoke.check('下拉框切到「无需审批」', !!off, sel() ? sel().value : '(没有下拉框)')

  // 第二轮：命令 + 读文件，两条各自命中一条 ask 规则。任何一条弹卡都会把这一轮挂在等人上，于是
  // 下面那句等待超时 —— 这正是"它还在问"的信号。
  smoke.type('.composer-input', '两条一起读一遍')
  smoke.click('.send-btn')
  const reply = await smoke.waitText('.msg-assistant', '两条都跑完了', 30000)
  smoke.check('第二轮跑到了结尾（没有停在等人上）', !!reply, smoke.text('.chat-content').slice(-80))
  smoke.check('第二轮的两次调用都没有弹卡', smoke.qa('.perm-form').length === 0,
    smoke.qa('.perm-form').length + ' 张卡还在')
  smoke.finish()
})()
