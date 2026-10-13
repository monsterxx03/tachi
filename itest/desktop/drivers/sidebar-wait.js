// sidebar-wait — 一个会话在等你，侧栏怎么把它说出来。
//
// 三种处境在这一侧钉住，每一种都是「标记该出现在哪里」的答案：
//   * 展开着：在那一行上（`.wait-dot`），并且说的是在等什么；
//   * 折叠着：行不在 DOM 里了，所以只能落在那一组的表头上（`.proj-wait`），带上数量与
//     「哪个会话、等什么」的 tooltip —— 别的分组不该有它；
//   * 答复之后：两个标记都消失。终结这次等待的事实写下下一个状态，标记是它的读数，不是
//     另开的一份账（Go 那一半见 AgentState.Waiting）。
//
// 两次等待走同一条路（bash 授权 / 模型提问），措辞不同，所以两句话都断言一次。
//
// 折叠表头那一段是这条 driver 存在的理由：只断言行上标记的话，一个「折叠后什么都不说」的
// 实现在这里照样全绿 —— 而那正是最需要它的时刻。
//
// 答复之后那两次 waitGone 等的是**稳定**窗口：fixture 在两次等待之间放了一次真的在跑、什么也不问
// 的工具调用（一秒），因为背靠背的两次 park 之间「标记不在」的那一瞬间短到轮询必然错过 —— 用
// waitFor 去抓一个瞬态就是抛硬币，那是断言写错了，不是实现坏了。
;(async () => {
  const PROJECT = 'smoke-wait'
  if (!(await smoke.waitForSession())) return smoke.finish()

  // 表头每次都重新查：作答会让侧栏重渲染，之前抓到的节点已经脱离文档（对它点击是无声的空操作）。
  const headOf = (name) => smoke.qa('.proj-head').find((h) => smoke.text(h).indexOf(name) >= 0)
  const titleOf = (el) => (el && el.getAttribute('title')) || ''

  smoke.type('.composer-input', '读一下那个文件')
  smoke.click('.send-btn')

  // ── 展开着：标记在那一行上 ────────────────────────────────────────────────
  if (!(await smoke.waitFor('.perm-form', '权限确认卡出现', 30000))) return smoke.finish()
  const rowDot = await smoke.waitFor('.session.active .wait-dot', '在等你的会话那一行出现标记')
  if (!rowDot) return smoke.finish()
  smoke.check('行上的标记说出在等什么（授权）', titleOf(rowDot).indexOf('等待你的授权') >= 0, titleOf(rowDot))

  // ── 折叠它所在的那一组：行离开 DOM，标记落到表头上 ────────────────────────
  const head = headOf(PROJECT)
  if (!head) return smoke.fail('项目的表头存在', '找不到 ' + PROJECT)
  smoke.click(head)
  if (!(await smoke.waitFor(() => !smoke.q('.session.active'), '折叠后当前会话的行离开 DOM'))) return smoke.finish()

  const badge = await smoke.waitFor('.proj-head .proj-wait', '折叠的表头上出现等待标记')
  if (!badge) return smoke.finish()
  smoke.check('表头标记给出数量', smoke.text(badge) === '1', smoke.text(badge))
  smoke.check('表头标记点出是哪个会话、在等什么',
    titleOf(badge).indexOf('冒烟会话') >= 0 && titleOf(badge).indexOf('等待你的授权') >= 0, titleOf(badge))
  const others = smoke.qa('.proj-head').filter((h) => smoke.text(h).indexOf(PROJECT) < 0)
  smoke.check('别的分组没有这枚标记（它属于有成员在等的那一组）',
    others.length > 0 && others.every((h) => !h.querySelector('.proj-wait')),
    others.map((h) => smoke.text(h).slice(0, 20)).join(' | '))

  // ── 答复：标记跟着消失（此刻它只可能出现在表头上，组还折着）──────────────
  const allow = smoke.qa('.perm-form .btn').find((b) => b.textContent.trim() === '允许一次')
  if (!smoke.click(allow)) return smoke.fail('点「允许一次」', '按钮找不到')
  if (!(await smoke.waitGone('.perm-form', '答复后确认卡消失', 30000))) return smoke.finish()
  // 这段回复在流（1.5s），所以「标记没了」是这一整段窗口里的稳态，不是一瞬间。
  if (!(await smoke.waitGone('.proj-head .proj-wait', '答复后表头标记消失'))) return smoke.finish()
  smoke.check('答复之后，折叠的表头不再说有人在等你', true)

  // ── 第二次等待，另一种措辞：提问 ─────────────────────────────────────────
  if (!(await smoke.waitFor('.ask-form', '提问表单出现', 30000))) return smoke.finish()
  const badge2 = await smoke.waitFor('.proj-head .proj-wait', '提问停在这里时表头再次出现标记')
  if (!badge2) return smoke.finish()
  smoke.check('第二次等待的措辞是「回答」', titleOf(badge2).indexOf('等待你的回答') >= 0, titleOf(badge2))

  // 展开回去：标记回到行上，表头不再重复这件事（它只在折叠时才有话说）。
  const head2 = headOf(PROJECT)
  if (!head2) return smoke.fail('项目的表头还在', '找不到 ' + PROJECT)
  smoke.click(head2)
  const rowDot2 = await smoke.waitFor('.session.active .wait-dot', '展开后行上出现标记')
  if (!rowDot2) return smoke.finish()
  smoke.check('行上的标记说的是在等什么（回答）', titleOf(rowDot2).indexOf('等待你的回答') >= 0, titleOf(rowDot2))
  if (!(await smoke.waitFor(() => !smoke.q('.proj-head .proj-wait'), '展开后表头不再重复这件事'))) return smoke.finish()

  // ── 答复它：串起两次的规矩是同一条 ────────────────────────────────────────
  const box = smoke.q('.ask-form .ask-input')
  if (!box) return smoke.fail('自由输入框存在', '找不到 .ask-input')
  if (!smoke.type(box, '按你写的来')) return smoke.fail('填入回答', '输入框找不到')
  smoke.key(box, 'Enter')
  if (!(await smoke.waitGone('.ask-form', '回答后表单消失', 30000))) return smoke.finish()
  if (!(await smoke.waitGone('.wait-dot', '回答后行上的标记消失'))) return smoke.finish()
  smoke.check('两次等待答复之后，侧栏里一枚标记都不剩',
    smoke.qa('.wait-dot').length === 0 && smoke.qa('.proj-wait').length === 0,
    'wait-dot=' + smoke.qa('.wait-dot').length + ' proj-wait=' + smoke.qa('.proj-wait').length)

  smoke.finish()
})()
