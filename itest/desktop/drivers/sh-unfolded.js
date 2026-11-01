// sh-unfolded — /sh 的输出必须自己出现在屏幕上，而不是藏在「1 步」后面。
//
// 这一条盯的是两层折叠里被漏掉的那一层：`ToolAutoExpand` 从前只打开**卡片**，而卡片被折进了
// 过程条（`turnView` 的 folded），过程条默认收起 —— 于是 `/sh` 那一轮（一步、没有正文）在屏幕上
// 只剩「1 步」，用户要的输出一个字也看不到。
//
// 所以断三件事：
//   1. 输出正文自己可见（不需要点任何东西）；
//   2. 卡片在屏幕上，且不靠过程条展开（`/sh` 轮没有可折的东西，那条「1 步」连渲染都不该有）；
//   3. 它确实没经过模型 —— `/sh` 是本地执行，mock 一次都不该被调（Go 侧的 after 断言计数）。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  smoke.type('.composer-input', '/sh echo sh-smoke-ok')
  smoke.click('.send-btn')

  // 1. 结果正文：不点任何东西就该在
  if (!(await smoke.waitText('.msg-assistant', 'sh-smoke-ok', 20000))) return smoke.finish()
  // 回合真的结束了再断言（停止按钮消失是「这一轮跑完」的信号）
  if (!(await smoke.waitFor(() => !smoke.q('.stop-btn'), '回合结束（停止按钮消失）'))) return smoke.finish()

  const card = smoke.q('.msg-assistant .tool-card')
  smoke.check('卡片在屏幕上（没有被折进过程条）', !!card, card ? card.className : '找不到 .tool-card')

  const body = smoke.q('.msg-assistant .tool-summary')
  smoke.check('结果正文直接可见（不需要点开）',
    !!body && smoke.text(body).indexOf('sh-smoke-ok') >= 0,
    body ? smoke.text(body).slice(0, 80) : '没有 .tool-summary（卡片是折着的）')

  // 2. 一轮只有一个「用户点名的调用」时，过程条没有内容可折 —— 不该多出一行「1 步」
  const strip = smoke.q('.process-head')
  smoke.check('不再多一行过程条', !strip, strip ? smoke.text(strip).trim() : '没有过程条')

  // 3. 切走再回来：输出还在，而且是展开的。这一条盯的是记录——`/sh` 的调用和结果被写进会话
  //    文件（标记 auto_expand），所以重新载入的对话仍画得出这张卡；而它不进模型历史（那由
  //    agent/session_convert_test.go 与这条用例的 go 半段一起钉住）。
  if (!(await smoke.waitFor('.new-chat', '侧栏的「新会话」按钮'))) return smoke.finish()
  smoke.click('.new-chat')
  // 新会话成为第一行，所以回原会话要按标题找，不能按下标。
  const rowByTitle = (title) =>
    smoke.qa('.session').find((r) => smoke.text(r.querySelector('.session-title')) === title)
  if (!(await smoke.waitFor(() => smoke.qa('.session').length >= 2, '侧栏出现第二个会话'))) return smoke.finish()
  const back = rowByTitle('冒烟会话')
  if (!back) return smoke.fail('找到原会话那一行', smoke.allText('.session-title').join(' | '))
  smoke.click(back)
  if (!(await smoke.waitFor(() => smoke.text('.msg-assistant').indexOf('sh-smoke-ok') >= 0,
    '回到原会话，输出还在', 10000))) return smoke.finish()
  const reopened = smoke.q('.msg-assistant .tool-summary')
  smoke.check('切走再回来，/sh 的输出还在（它进了会话记录）',
    !!reopened && smoke.text(reopened).indexOf('sh-smoke-ok') >= 0,
    reopened ? smoke.text(reopened).slice(0, 80) : '没有 .tool-summary（卡片折着或没画出来）')

  // 对照：模型自己跑的 shell 仍然折进过程条（这改动只对「用户点名」的调用生效）
  smoke.type('.composer-input', '跑一条模型自己的命令')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant .msg-content', '对照轮跑完了。', 30000))) return smoke.finish()
  if (!(await smoke.waitFor(() => !smoke.q('.stop-btn'), '对照轮结束'))) return smoke.finish()
  const control = smoke.q('.process-head')
  smoke.check('模型自己的 shell 照旧折起来（对照轮有过程条）', !!control,
    control ? smoke.text(control).trim() : '没有过程条')
  smoke.finish()
})()
