// rewind — "回退到这里" on a user bubble must put the workspace AND the conversation
// back to the start of that turn.
//
// The file the agent wrote is written through BASH, because that is the coverage this
// feature exists for: no other agent's checkpoints see a shell command's changes. The
// Go half of this scenario (scenarios.go) then reads the working directory and asserts
// the file it created is GONE and the one it edited is back — the UI half here can only
// prove the card said so.
//
// The prompt that started the rewound turn comes back into the composer, which is the
// point of going back to it: you edit what you asked and send again.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  // One turn: send, then wait for the turn's own RESULT. Waiting for the stop button to
  // appear is the trap here — the scripted mock answers faster than the next poll, so a
  // turn can start and finish without ever being observed running (and "never seen
  // running" is not "did not run"). The settle wait after it is what keeps the second
  // prompt from being queued as a steer into the first turn.
  const runTurn = async (text, expect) => {
    smoke.type('.composer-input', text)
    smoke.click('.send-btn')
    if (!(await smoke.waitText('.msg-assistant', expect, 30000))) return false
    await smoke.waitFor(() => !smoke.q('.stop-btn'), '这一轮结算', 10000)
    await smoke.sleep(300)
    return true
  }

  if (!(await runTurn('用 bash 写文件', '写好了。'))) return smoke.finish()
  if (!(await runTurn('第二轮', '第二轮回复。'))) return smoke.finish()

  const bubbles = smoke.qa('.msg-user')
  smoke.check('两条用户消息都在（第二轮是回退要撤销的那一轮）', bubbles.length === 2, String(bubbles.length))
  if (bubbles.length < 2) return smoke.finish()

  // Right-click the FIRST prompt: the turn to go back to.
  bubbles[0].dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 80, clientY: 260 }))
  if (!(await smoke.waitFor('.ctx-menu', '回退菜单出现', 5000))) return smoke.finish()
  const item = smoke.qa('.ctx-menu .ctx-item')[0]
  smoke.check('这一轮有检查点，所以菜单项可用', !!item && !item.disabled, item ? (item.title || '可点') : '找不到菜单项')
  if (!item || item.disabled) return smoke.finish()
  item.click()

  // The card must say what it is about to do BEFORE it does it — and the destructive half
  // (a file it will delete) is the part a reader has to see.
  if (!(await smoke.waitFor('.confirm-box', '回退确认卡出现', 5000))) return smoke.finish()
  const card = smoke.text('.confirm-box')
  smoke.check('卡片写明会删除 shell 新建的那个文件', card.indexOf('删除 1') >= 0, card.slice(0, 200))
  smoke.check('卡片把该轮的原提示词摆在明面上', card.indexOf('用 bash 写文件') >= 0, card.slice(0, 200))

  const confirm = smoke.qa('.confirm-box .btn.danger')[0]
  if (!confirm) return smoke.fail('找到确认按钮', '按钮找不到')
  confirm.click()

  // A refusal (a running turn, an unavailable snapshot) belongs IN the card, next to the
  // button that was pressed — so if the card is still there, that is the story.
  await smoke.sleep(1500)
  if (smoke.q('.confirm-box')) {
    smoke.log('回退未生效，卡片上的说法', smoke.text('.confirm-box').slice(0, 200))
  }

  // After the rewind the conversation is back to before that turn: BOTH bubbles are gone
  // (the turn's own prompt is removed from the history too, and handed back instead).
  const gone = await smoke.waitFor(() => smoke.qa('.msg-user').length === 0, '回退后对话回到该轮之前', 15000)
  if (!gone) return smoke.finish()
  smoke.check('该轮自己的用户消息也移出了历史', smoke.qa('.msg-user').length === 0, String(smoke.qa('.msg-user').length))

  const chat = smoke.text('.chat-content')
  smoke.check('对话里留下一条回退提示', chat.indexOf('已回退到第 1 轮之前') >= 0, chat.slice(-160))

  // The notice's body is folded behind its own 摘要 toggle (the same part the compaction
  // notices use), so the numbers are a click away — and that click is part of what this
  // asserts: the count of what moved has to be reachable from the line that reports it.
  const toggle = smoke.q('.notice-toggle')
  if (!toggle) return smoke.fail('找到提示的摘要入口', '没有 .notice-toggle')
  toggle.click()
  await smoke.sleep(200)
  const expanded = smoke.text('.chat-content')
  smoke.check('提示说明了文件动过什么', expanded.indexOf('还原 1') >= 0 && expanded.indexOf('删除 1') >= 0, expanded.slice(-160))

  const input = smoke.q('.composer-input')
  smoke.check('该轮的提示词回到输入框（可以改着重发）',
    !!input && input.value === '用 bash 写文件', input ? input.value : '(找不到输入框)')

  smoke.finish()
})()
