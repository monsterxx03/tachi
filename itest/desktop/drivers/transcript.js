// transcript — the baseline every other scenario assumes works.
//
// Sends one message, then asserts what the round trip leaves on screen: the reply text,
// the tool card (with its output and a closed state), the usage numbers — and the order
// the turn keeps: the process strip sits above the reply it belongs to.
//
// The card is asserted AFTER expanding the turn's process strip: a successful call folds
// into the strip the moment it finishes (and the smoke's Bash returns in milliseconds, so
// the card is not reliably visible while it runs), while the reply is the turn's
// conclusion and stays visible. Asserting the card without expanding would depend on
// catching a millisecond-wide window. A run that fails here means the smoke harness itself is off, not
// the app.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  if (!smoke.type('.composer-input', '你好')) return smoke.fail('填入编辑器', 'textarea 找不到')
  if (!smoke.click('.send-btn')) return smoke.fail('点击发送', '按钮找不到')

  // The turn's conclusion is always visible — no toggle in the way.
  const reply = await smoke.waitText('.msg-assistant .msg-content', '命令跑完了')
  if (!reply) return smoke.finish()
  smoke.check('回复上屏（结论恒显）', true)

  const strip = smoke.q('.process-head')
  if (!strip) return smoke.fail('过程条出现', '找不到 .process-head')
  smoke.check('过程条提示这一轮走了一步', strip.textContent.indexOf('1 步') >= 0, strip.textContent.trim())
  // The invariant is containment, not geometry: the strip and the answer belong to the SAME
  // assistant bubble (a turn's process must never drift away from the turn it explains).
  // Comparing positions would only say how they were laid out this time.
  const sameBubble = smoke.qa('.msg-assistant').some((b) => b.contains(strip) && b.contains(reply))
  smoke.check('过程条与回复在同一条助手消息里', sameBubble,
    smoke.qa('.msg-assistant').length + ' 条助手消息')
  // ORDER, not containment: the strip summarizes the turn and the conclusion closes it, so the
  // reply must be the LAST block of .turn-parts and must come after the strip. Containment alone
  // would pass even if a reorder pushed the prose above the cards it describes.
  const last = smoke.q('.msg-assistant .turn-parts')?.lastElementChild
  smoke.check('结论正文是这一轮的最后一块', !!last && last.textContent.indexOf('命令跑完了') >= 0,
    last ? last.textContent.slice(0, 40) : '找不到')
  smoke.check('过程条排在结论之前', !!last && (strip.compareDocumentPosition(last) & 4) !== 0,
    'strip 应位于最后一块之前')

  smoke.click(strip)
  if (!(await smoke.waitFor('.tool-card', '展开后工具卡出现'))) return smoke.finish()
  smoke.check('工具卡出现', true)
  await smoke.waitText('.tool-card', 'smoke-tool-ok')
  smoke.check('工具卡显示命令输出', smoke.text('.tool-card').indexOf('smoke-tool-ok') >= 0, smoke.text('.tool-card').slice(0, 40))
  smoke.check('工具卡已收尾（不再显示「执行中」）', smoke.text('.tool-card').indexOf('执行中') < 0)

  // Usage reaches the status bar: the cache ring only renders once a rate arrives, so it
  // doubles as "the usage/cost path is wired".
  const ring = await smoke.waitFor('svg.ctx-ring[aria-label^="缓存命中率"]', '用量数字到达（缓存命中率环）')
  if (ring) smoke.check('缓存命中率环渲染', true, ring.getAttribute('aria-label'))

  smoke.check('发送按钮在输入为空时禁用', smoke.q('.send-btn').disabled)
  smoke.finish()
})()
