// oneoff-panel — the side-channel panel reads a one-off run back from disk, and the
// conversation keeps only a line about it.
//
// A typed /review is a one-off fork: it leaves a record in the session's oneoff/ directory
// and nothing in the conversation history. This driver pins both halves of that promise —
// the panel lists and replays the run (P1), and the transcript stops carrying its process
// (P2): no reply text, no tool cards, just the command the user typed and one notice saying
// where the output went.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.check('面板默认是收起的', !smoke.q('.oneoff-panel'), '')

  // A review needs no diff chip, so this scenario does not depend on a turn having edited
  // anything: the command itself is the whole trigger.
  //
  // No wait on the 「运行中」 markers: a mock-backed review round finishes in well under a
  // second, so a 150ms poll can miss the running state entirely. The panel is a READER — it
  // shows what is on disk — so the honest synchronisation is "the record is there yet?",
  // asked by refreshing until it is.
  smoke.type('.composer-input', '/review')
  smoke.click('.send-btn')

  // P2: the panel opens itself. The reader just launched the run, and this is where its
  // output goes.
  if (!(await smoke.waitFor('.oneoff-panel', '面板随运行自动打开', 8000))) return smoke.finish()

  // The switcher: the run just finished, and the record is read from the file — the panel
  // never saw the live stream, so listing it here proves the disk path works end to end.
  const listed = await smoke.waitFor(
    () => {
      const opts = smoke.qa('.oneoff-select option').filter((o) => o.textContent.indexOf('评审') >= 0)
      if (opts.length === 0) smoke.click('.oneoff-icon') // 记录可能还没落盘：重读一次
      return opts.length > 0
    },
    '切换器里出现这次评审', 20000)
  const options = smoke.qa('.oneoff-select option').map((o) => o.textContent)
  smoke.check('切换器列出了刚落盘的那次评审', !!listed, options.join(' | '))
  smoke.check('这个会话此时只有这一次旁路运行', options.length === 1, String(options.length))
  if (!listed) return smoke.finish()

  // The replay: the reviewer's prompt (the record's user message), the tool call it made and
  // its reply. All three travel as session messages, so all three render as usual.
  if (!(await smoke.waitFor(() => smoke.text('.oneoff-body').indexOf('oneoff-smoke') >= 0, '回放出工具输出', 8000))) return smoke.finish()
  const body = smoke.text('.oneoff-body')
  smoke.check('回放里有用户消息（这次评审收到的 prompt）', body.indexOf('Perform a thorough code review') >= 0,
    body.indexOf('Perform a thorough code review') >= 0 ? '' : body.slice(0, 120))
  smoke.check('回放里有模型回复', body.indexOf('评审完成：本次改动没有发现问题。') >= 0, '')
  // The CARD itself, not a loose `[class*=tool]` sweep: the request-summary rows render
  // `oneoff-req-tools` in the 过程 pane for any run that made a request, so the old fallback
  // held whether or not a tool card was replayed — the very claim this line exists to pin.
  smoke.check('回放里有工具卡（Bash）', !!smoke.q('.oneoff-body .msg-assistant .tool-card'),
    smoke.qa('.oneoff-body .tool-card').map((e) => e.className).join('|').slice(0, 80))

  // P2's other half: the CONVERSATION. The review's own words must not be in it — and what is
  // left is the user's line plus one notice naming what happened and where to look.
  const chat = smoke.text('.chat')
  smoke.check('主对话没有回放评审回复', chat.indexOf('评审完成：本次改动没有发现问题。') < 0, '')
  smoke.check('主对话没有评审的工具卡', !smoke.q('.chat .tool-card'), smoke.qa('.chat [class*=tool]').length + ' 个工具部件')
  const anchored = await smoke.waitFor(() => smoke.text('.chat .notice-block').indexOf('已完成') >= 0, '主对话留下一行锚点', 10000)
  smoke.check('占位气泡降级成一行锚点（指向面板）', !!anchored,
    smoke.text('.chat .notice-block') || '(没有 notice)')

  // The requests a run made are summarised, and the prompt text is fetched only when asked
  // for (a record is over half request payloads). One click must bring the正文 in.
  const reqRows = smoke.qa('.oneoff-req-row')
  smoke.check('列出了这次运行的 API 请求', reqRows.length >= 1, String(reqRows.length))
  if (reqRows.length) {
    reqRows[0].click()
    const promptShown = await smoke.waitFor(
      () => smoke.qa('.oneoff-pre').some((p) => p.textContent.indexOf('## Your task') >= 0),
      '按需取回请求正文', 8000)
    smoke.check('点开后取回了这次请求的正文（懒加载）', !!promptShown,
      smoke.qa('.oneoff-pre').map((p) => p.textContent.length).join(','))
  }

  // The panel's width is the reader's to drag, and it is remembered. Driving it here also pins
  // its bounds: 320px is the floor (mirrored by ONE_OFF_PANEL_MIN_WIDTH in oneoff.tsx), and the
  // ceiling exists so the conversation keeps 480px (CHAT_MIN_WIDTH there) — asserted as that
  // fact, not as the formula behind it, because a driver that recomputed the formula would only
  // be reading the implementation back to itself.
  const panelWidth = () => Math.round(smoke.q('.oneoff-panel').getBoundingClientRect().width)
  const chatWidth = () => Math.round(smoke.q('.chat').getBoundingClientRect().width)
  const handle = smoke.q('.oneoff-resizer')
  if (!handle) return smoke.fail('面板有拖拽手柄', '找不到 .oneoff-resizer')
  const box = handle.getBoundingClientRect()
  const at = { x: box.left + 2, y: box.top + 40 }
  // The moves land on the WINDOW: that is where the drag listens (a pointer that leaves the 6px
  // handle must keep resizing), and setPointerCapture would refuse a synthetic pointer.
  const press = () => handle.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, pointerId: 1, clientX: at.x, clientY: at.y }))
  const moveTo = (x) => window.dispatchEvent(new PointerEvent('pointermove', { bubbles: true, pointerId: 1, clientX: x, clientY: at.y }))
  const release = (x) => window.dispatchEvent(new PointerEvent('pointerup', { bubbles: true, pointerId: 1, clientX: x, clientY: at.y }))

  const initial = panelWidth()
  press()
  // Read SYNCHRONOUSLY: startDrag adds the class itself, so this is the one probe that does
  // not depend on React having committed anything yet.
  smoke.check('按下手柄即进入拖拽态（指针事件到了面板）', document.body.classList.contains('is-resizing'),
    `body="${document.body.className}" width=${panelWidth()}px`)
  moveTo(at.x + 5000); release(at.x + 5000) // far past the minimum
  // Wait for the STATE, not for a duration: with a fixed sleep the same drag read 420 in one
  // run and 320 in the next, because a sleep measures the commit by luck.
  const atFloor = await smoke.waitFor(() => panelWidth() === 320, '拖到极限（钳在最小宽度）', 3000)
  smoke.check('拖到极限时面板停在最小宽度', atFloor,
    `${initial} → ${panelWidth()}px（inline=${smoke.q('.oneoff-panel').style.width}）`)

  press(); moveTo(at.x - 5000); release(at.x - 5000) // and now to the ceiling
  const grew = await smoke.waitFor(() => panelWidth() > 320, '往外拖变宽', 3000)
  const widest = panelWidth()
  smoke.check('拖动手柄改变面板宽度', grew, `320 → ${widest}px`)
  smoke.check('面板再宽也不吃掉对话的最小宽度', chatWidth() >= 480, `对话 ${chatWidth()}px`)

  smoke.finish()
})()
