// sessions — per-session numbers, the live sidebar title, and where the caret goes.
//
// Two of the three leaks in docs/agents/desktop.md live here: a brand-new session must not inherit the
// previous session's cache ring, and the row must pick up the generated title when the
// session_title event arrives (without it the row says 未命名会话 forever). And creating a
// session is the one moment the composer takes focus by itself — the click leaves it on the
// button, so a new session has to start with the user able to type.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '第一件事')
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
  smoke.finish()
})()
