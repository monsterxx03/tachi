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

  // ── 回退链：一次看到全部可回退点 ──────────────────────────────────────────
  // 转录只装最新一页记录，所以链不可能从它来——这份列表来自检查点 manifest。这里先用
  // ⌘⇧H 打开（会话行右键是另一个入口，回退之后再用一次），验两件事：行的状态与数字，以及
  // 「点一行 = 打开气泡那张确认卡片」。
  smoke.key(document.body, 'h', { metaKey: true, shiftKey: true })
  const chainOpen = await smoke.waitFor(
    () => (smoke.qa('.rewind-row').length ? smoke.qa('.rewind-row') : null), '⌘⇧H 打开回退链', 6000)
  smoke.check('⌘⇧H 打开回退链', !!chainOpen, '')
  if (!chainOpen) return smoke.finish()

  const rows = () => smoke.qa('.rewind-row')
  smoke.check('链里两轮都在（这一版没有别的入口能看到全部）', rows().length === 2,
    smoke.allText('.rewind-row-when').join(' | '))
  smoke.check('最新的一轮排在最上面（从新到旧）',
    rows().length === 2 && smoke.text(rows()[0]).indexOf('第 2 轮') >= 0, smoke.text(rows()[0]).slice(0, 60))
  const rowTexts = rows().map((r) => smoke.text(r))
  smoke.check('只读的那一轮标成「没写文件」', rowTexts[0].indexOf('没写文件') >= 0, rowTexts[0].slice(0, 80))
  smoke.check('写过文件的那一轮给出检查点的真实数字（2 files +2 −1）',
    rowTexts[1].indexOf('2') >= 0 && rowTexts[1].indexOf('+2') >= 0 && rowTexts[1].indexOf('−1') >= 0,
    rowTexts[1].slice(0, 80))
  smoke.check('链本身就说明了语义（之后的工作会被放弃）',
    smoke.text('.rewind-chain').indexOf('会被放弃') >= 0, smoke.text('.rewind-chain-why'))
  // 还没回退过：对话伸到所有轮次之后，所以位置是「对话末尾」，没有任何一行被标成「你在这里」。
  // （这与「位置 = 0」是两回事——后者发生在回退到第一轮之后，行必须被标出来。）
  const hereRowNow = () => rows().find((r) => smoke.text(r).indexOf('你在这里') >= 0) || null
  smoke.check('回退之前位置是「对话末尾」，没有行被标成当前位置',
    smoke.text('.rewind-chain-now').indexOf('对话末尾') >= 0 && hereRowNow() === null,
    smoke.text('.rewind-chain-now'))

  // 点一行 → 同一张确认卡片（同一套问题：还原什么、删除什么、什么撤不回）。这里不确认，取消。
  rows()[1].click()
  if (!(await smoke.waitFor('.confirm-box', '链里选一轮打开确认卡', 5000))) return smoke.finish()
  const fromChain = smoke.text('.confirm-box')
  smoke.check('点链里的行打开的是同一张卡片（说明会删除什么）', fromChain.indexOf('删除 1') >= 0, fromChain.slice(0, 160))
  smoke.check('卡片带着该轮的提示词', fromChain.indexOf('用 bash 写文件') >= 0, fromChain.slice(0, 160))
  smoke.check('开卡片时链自己关掉（不叠两层）', !smoke.q('.rewind-chain'), '')
  const cancel = smoke.qa('.confirm-box .btn.ghost')[0]
  if (!cancel) return smoke.fail('找到取消按钮', '按钮找不到')
  cancel.click()
  if (!(await smoke.waitFor(() => !smoke.q('.confirm-box'), '取消后卡片关闭', 3000))) return smoke.finish()
  smoke.check('取消不动任何东西', smoke.qa('.msg-user').length === 2, String(smoke.qa('.msg-user').length))

  const bubbles = smoke.qa('.msg-user')
  smoke.check('两条用户消息都在（第二轮是回退要撤销的那一轮）', bubbles.length === 2, String(bubbles.length))
  if (bubbles.length < 2) return smoke.finish()

  // Right-click the FIRST prompt: the turn to go back to. Before using the menu, prove it
  // can be DISMISSED — with the two gestures a reader actually makes (a press on blank
  // space, Escape). A menu that cannot be closed covers the page it belongs to, and this
  // one could not: it was dismissed by `onMouseLeave` alone, which only fires once the
  // pointer has been inside the menu, something a right-click does not guarantee.
  const openMenu = async (label) => {
    bubbles[0].dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 80, clientY: 260 }))
    return !!(await smoke.waitFor('.ctx-menu', label, 5000))
  }
  if (!(await openMenu('回退菜单出现'))) return smoke.finish()
  smoke.q('.chat-content').dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }))
  const pressClosed = await smoke.waitFor(() => !smoke.q('.ctx-menu'), '点空白处关掉菜单', 2000)
  smoke.check('点空白处菜单关闭', !!pressClosed, pressClosed ? '' : smoke.text('.ctx-menu').slice(0, 60))

  if (!(await openMenu('再次打开回退菜单'))) return smoke.finish()
  smoke.key(document.body, 'Escape')
  const escClosed = await smoke.waitFor(() => !smoke.q('.ctx-menu'), 'Esc 关掉菜单', 2000)
  smoke.check('Esc 关掉菜单', !!escClosed, escClosed ? '' : smoke.text('.ctx-menu').slice(0, 60))

  if (!(await openMenu('第三次打开回退菜单'))) return smoke.finish()
  const item = smoke.qa('.ctx-menu .ctx-item')[0]
  smoke.check('这一轮有检查点，所以菜单项可用', !!item && !item.disabled, item ? (item.title || '可点') : '找不到菜单项')
  if (!item || item.disabled) return smoke.finish()

  // The status bar's context ring has to follow the rewind: the backend recomputed the
  // estimate for the history that REMAINS and dropped the anchor that measured the removed
  // one, and the ring only ever learns these numbers from a push. So a rewind that forgets
  // to push leaves it pointing at the size of the conversation that was just taken away —
  // read BEFORE confirming, because that is the value the reader is looking at.
  const ringPct = () => {
    const el = smoke.q('.ctx-btn[aria-label^="上下文"]')
    const m = el ? /上下文\s*([\d.]+)%/.exec(el.getAttribute('aria-label') || '') : null
    return m ? parseFloat(m[1]) : null
  }
  // The popover says `X% 的上下文窗口` (only when both numbers are known).
  const detailPct = () => {
    const m = /([\d.]+)%\s*的上下文窗口/.exec(smoke.text('.ctx-panel'))
    return m ? parseFloat(m[1]) : null
  }
  const ringBefore = ringPct()

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

  // …and the numbers the rewind changed reach the status bar, not just the agent's memory. The
  // pair that proves it is the ring against the POPOVER, which fetches on open and so cannot be
  // stale: both must describe the same moment. That pair is the invariant — the DIRECTION is
  // not (with the scripted provider the anchor comes from its synthetic prompt_tokens, so the
  // number can even rise; with a real one the dropped anchor makes it fall). Without the push
  // the ring keeps the discarded conversation's size while the popover already has the new one.
  smoke.click('.ctx-btn')
  if (!(await smoke.waitFor('.ctx-panel', '明细面板打开', 5000))) return smoke.finish()
  const detailAfter = detailPct()
  smoke.click('.ctx-btn')
  const ringAfter = ringPct()
  smoke.check('回退后状态栏的上下文占用与明细同口径（环也跟上了回退）',
    ringAfter !== null && detailAfter !== null && Math.abs(ringAfter - detailAfter) < 0.5,
    `回退前 环=${ringBefore}% · 回退后 环=${ringAfter}% · 明细=${detailAfter}%`)

  // The notice's body is folded behind its own 摘要 toggle (the same part the compaction
  // notices use), so the numbers are a click away — and that click is part of what this
  // asserts: the count of what moved has to be reachable from the line that reports it.
  const toggle = smoke.q('.notice-toggle')
  if (!toggle) return smoke.fail('找到提示的摘要入口', '没有 .notice-toggle')
  toggle.click()
  await smoke.sleep(200)
  const expanded = smoke.text('.chat-content')
  smoke.check('提示说明了文件动过什么', expanded.indexOf('还原 1') >= 0 && expanded.indexOf('删除 1') >= 0, expanded.slice(-160))

  // ── 回退之后，链变短 ─────────────────────────────────────────────────────
  // A rewind DROPS the turns after its target (an abandoned branch is disposable), so the list
  // has to lose them too — a chooser still offering 「第二轮」 would promise a return to a
  // conversation that no longer exists. This is also the session-row menu entry (the other way
  // in) being exercised.
  const sessionRow = smoke.qa('.session')[0]
  if (!sessionRow) return smoke.fail('侧栏有会话行（右键入口要用它）', '找不到 .session')
  sessionRow.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 90, clientY: 120 }))
  if (!(await smoke.waitFor('.ctx-menu', '会话行右键菜单出现', 5000))) return smoke.finish()
  const chainItem = smoke.qa('.ctx-menu .ctx-item').find((b) => b.textContent.indexOf('回退链') >= 0)
  smoke.check('会话右键菜单里有回退链入口', !!chainItem, smoke.allText('.ctx-menu .ctx-item').join(' | '))
  if (!chainItem) return smoke.finish()
  chainItem.click()
  if (!(await smoke.waitFor('.rewind-chain', '从会话菜单打开回退链', 5000))) return smoke.finish()
  smoke.check('被放弃的轮次从链里消失（只剩回退到的那一轮）', rows().length === 1,
    smoke.allText('.rewind-row-when').join(' | '))
  smoke.check('剩下的就是回退到的那一轮', rows().length === 1 && smoke.text(rows()[0]).indexOf('第 1 轮') >= 0,
    rows().length ? smoke.text(rows()[0]).slice(0, 60) : '(空)')
  smoke.key(document.body, 'Escape')
  const chainClosed = await smoke.waitFor(() => !smoke.q('.rewind-chain'), 'Esc 关掉回退链', 3000)
  smoke.check('Esc 关掉回退链', !!chainClosed, '')

  const input = smoke.q('.composer-input')
  smoke.check('该轮的提示词回到输入框（可以改着重发）',
    !!input && input.value === '用 bash 写文件', input ? input.value : '(找不到输入框)')

  // A turn that wrote NOTHING must still be rewindable — and it is the common case
  // ("that answer was wrong, let me ask again"), because a conversation that only
  // reads has no file state at all. The card has to distinguish the two ways of "no
  // file work": the workspace is ALREADY at the state to return to (a fact — what
  // this turn is) versus "nothing was restored" (a warning). Refusing the first is
  // the bug this half exists for.
  if (!(await runTurn('再问一句', '第三轮回复。'))) return smoke.finish()
  const again = smoke.qa('.msg-user')
  smoke.check('回退之后还能接着对话', again.length === 1, String(again.length))
  if (again.length !== 1) return smoke.finish()

  again[0].dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 80, clientY: 260 }))
  if (!(await smoke.waitFor('.ctx-menu', '回退菜单出现', 5000))) return smoke.finish()
  const onlyItem = smoke.qa('.ctx-menu .ctx-item')[0]
  smoke.check('只读的一轮也有回退入口', !!onlyItem && !onlyItem.disabled, onlyItem ? (onlyItem.title || '可点') : '找不到菜单项')
  if (!onlyItem || onlyItem.disabled) return smoke.finish()
  onlyItem.click()

  if (!(await smoke.waitFor('.confirm-box', '回退确认卡出现', 5000))) return smoke.finish()
  const card2 = smoke.text('.confirm-box')
  smoke.check('不把「没有文件要还原」当成不能回退', card2.indexOf('不能回退') < 0, card2.slice(0, 200))
  smoke.check('卡片说明工作区无需还原', card2.indexOf('无需还原') >= 0, card2.slice(0, 200))
  const confirm2 = smoke.qa('.confirm-box .btn.danger')[0]
  if (!confirm2) return smoke.fail('找到确认按钮', '按钮找不到')
  confirm2.click()

  const gone2 = await smoke.waitFor(() => smoke.qa('.msg-user').length === 0, '只读的一轮也回退了', 15000)
  smoke.check('只读的一轮同样撤回到了它之前', gone2, String(smoke.qa('.msg-user').length))

  // ── 回退到最新那一轮之后：链要说出「你站在这里」 ────────────────────────────
  // 这一轮的 records 就是现在对话的末尾，所以它不会被裁掉（没有「之后」可裁），看起来「回退
  // 了什么都没发生」。链必须说清位置，并且不再把它当成一个目的地——否则它的卡片会承诺撤销
  // 一批已经不存在的轮次。这正是「回退过一次后链里还有回退前的节点」那一次报告。
  smoke.key(document.body, 'h', { metaKey: true, shiftKey: true })
  if (!(await smoke.waitFor('.rewind-chain', '再看一次回退链', 5000))) return smoke.finish()
  const afterRows = rows()
  smoke.check('回退到最新那一轮后，链里仍是两轮（没有「之后」可裁）', afterRows.length === 2,
    smoke.allText('.rewind-row-when').join(' | '))
  const hereRow = afterRows.find((r) => smoke.text(r).indexOf('你在这里') >= 0)
  smoke.check('最新那一轮被标成「你在这里」', !!hereRow, hereRow ? smoke.text(hereRow).slice(0, 50) : smoke.allText('.rewind-row-badge'))
  smoke.check('位置写在最新那一行上', !!hereRow && smoke.text(afterRows[0]).indexOf('你在这里') >= 0,
    smoke.text(afterRows[0]).slice(0, 50))
  smoke.check('锚点不再说「对话末尾」（位置就在下面那一行）',
    smoke.text('.rewind-chain-now').indexOf('第 2 轮的开头') >= 0, smoke.text('.rewind-chain-now'))
  const hereGo = hereRow && hereRow.querySelector('.rewind-row-go')
  smoke.check('这一行没有「回退到这里」入口（它不是目的地）',
    !!hereGo && getComputedStyle(hereGo).display === 'none',
    hereGo ? getComputedStyle(hereGo).display : '(找不到按钮)')
  smoke.check('点它也不会打开卡片', (() => { hereRow.click(); return !smoke.q('.confirm-box') })(), '')

  smoke.key(document.body, 'Escape')
  if (!(await smoke.waitFor(() => !smoke.q('.rewind-chain'), 'Esc 关闭（结尾）', 3000))) return smoke.finish()

  smoke.finish()
})()
