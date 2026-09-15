// sessions — per-session numbers, the live sidebar title, and where the caret goes.
//
// Two of the three leaks in docs/agents/desktop.md live here: a brand-new session must not inherit the
// previous session's cache ring, and the row must pick up the generated title when the
// session_title event arrives (without it the row says 未命名会话 forever). And creating a
// session is the one moment the composer takes focus by itself — the click leaves it on the
// button, so a new session has to start with the user able to type.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  // 第一条消息带 @-file 引用：模型收到的是展开后的文件内容，但**转写里显示的**必须是用户打的
  // 原文 —— 切回旧会话时（最后那段）会从磁盘重建转写，那里钉住这件事。
  smoke.type('.composer-input', '@README.md 里的说明')
  // 打了 "@" 会弹出选择器，而 Enter 会被它拿去"接受补全"而不是发送；Esc 先把它关掉，
  // 这样提交的只有发送按钮（真实用户也是这么做的：先 Esc，或直接点发送）。
  smoke.key('.composer-input', 'Escape')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('svg.ctx-ring[aria-label^="缓存命中率"]', '老会话出现用量环', 30000))) {
    // The numbers live in the status row, so dump it: "the ring never appeared" is only
    // half the story (empty row? wrong ring? no reply at all?). This line is what found
    // the turn-end usage gap.
    smoke.log('诊断：用量行 / 环 / 助手气泡', [
      JSON.stringify(smoke.text('.usage-meta')),
      smoke.qa('.ctx-ring').map((e) => e.getAttribute('aria-label') || 'aria-hidden').join('|') || '(无)',
      smoke.qa('.msg-assistant').length,
    ].join(' · '))
    return smoke.finish()
  }
  smoke.check('老会话有缓存命中率', true, smoke.text('.usage-meta'))
  const oldTitle = smoke.text('.session.active .session-title')
  smoke.check('侧边栏标题已生成', oldTitle !== '' && oldTitle !== '未命名会话', oldTitle)

  // Hand the caret to the transcript before creating the session: the assertion below only
  // proves anything if the composer was NOT focused beforehand.
  smoke.q('.chat').focus()

  if (!smoke.click('.new-chat')) return smoke.fail('点击新建会话', '按钮找不到')
  if (!(await smoke.waitFor(() => smoke.text('.session.active .session-title') !== oldTitle, '切到新会话', 10000))) return smoke.finish()
  // Focus was moved off the composer above, so this says the app did it, not that it was
  // already there. No waitFor: the composer is focused before the sidebar is refreshed, so
  // by the time the title changed the caret is settled — and a waitFor that failed here
  // would only add a second, redundant failure line.
  const active = document.activeElement
  smoke.check('新建会话后光标自动落在输入框', active === smoke.q('.composer-input'),
    'activeElement=' + (active ? (active.className || active.tagName) : '(无)'))
  const ringGone = await smoke.waitFor(() => !smoke.q('svg.ctx-ring[aria-label^="缓存命中率"]'), '新会话的用量环清空', 8000)
  smoke.check('新会话不继承上一个会话的缓存环', !!ringGone, smoke.text('.usage-meta'))
  smoke.check('新会话状态行是空的', smoke.text('.usage-meta') === '', JSON.stringify(smoke.text('.usage-meta')))

  // Sending in the new session gives it its own numbers…
  smoke.type('.composer-input', '新会话第一条')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('svg.ctx-ring[aria-label^="缓存命中率"]', '新会话自己的用量环', 30000))) return smoke.finish()
  smoke.check('新会话跑完拿到自己的用量', true, smoke.text('.usage-meta'))

  // …and switching back restores the old session's.
  const back = smoke.qa('.session').filter((s) => smoke.text(s.querySelector('.session-title')) === oldTitle)
  if (!back.length) return smoke.fail('旧会话还能在侧边栏找到', oldTitle)
  smoke.click(back[0])
  if (!(await smoke.waitFor(() => smoke.text('.session.active .session-title') === oldTitle, '切回旧会话', 10000))) return smoke.finish()
  const restored = await smoke.waitFor('svg.ctx-ring[aria-label^="缓存命中率"]', '切回后用量环回来', 10000)
  smoke.check('切回旧会话用量数字回来', !!restored, restored ? restored.getAttribute('aria-label') : '')

  // 这一轮的 @-file 留在记录里的两份文本由 Go 侧断言（读 messages.jsonl）；"从磁盘渲染出来的
  // 气泡长什么样"是 at-file-reload 的事 —— 在这里切会话用的是内存副本，断言不到重建路径。

  // The sidebar row's right-click menu. The 打开会话目录 item is asserted but NOT clicked: it
  // launches the real Finder, which would take the foreground and suspend this webview
  // mid-run (the suspension trap — see docs/agents/desktop.md). Which path it hands over is
  // pinned in Go instead (TestOpenSessionDirOpensTheSessionDirectory), so this half only has
  // to prove the row really offers it.
  const row = smoke.q('.session.active')
  row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 60, clientY: 200 }))
  const menu = await smoke.waitFor('.ctx-menu', '会话行的右键菜单出现', 3000)
  const items = smoke.allText('.ctx-item')
  smoke.check('右键菜单列出「打开会话目录」', !!menu && items.indexOf('打开会话目录') >= 0, items.join(' / '))
  // Dismiss it the way a reader does — the menu's own rule, and the DOM artifact then shows
  // the page rather than a menu frozen over it.
  if (menu) menu.dispatchEvent(new MouseEvent('mouseout', { bubbles: true, relatedTarget: document.body }))
  const closed = await smoke.waitFor(() => !smoke.q('.ctx-menu'), '右键菜单失焦关闭', 2000)
  smoke.check('右键菜单移开后关闭', !!closed, closed ? '' : smoke.text('.ctx-menu'))
  smoke.finish()
})()
