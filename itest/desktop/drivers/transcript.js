// transcript — the baseline every other scenario assumes works.
//
// Sends one message, then asserts what the round trip leaves on screen: the reply text,
// the tool card (with its output and a closed state), the second reply, and the usage
// numbers. A run that fails here means the smoke harness itself is off, not the app.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  if (!smoke.type('.composer-input', '你好')) return smoke.fail('填入编辑器', 'textarea 找不到')
  if (!smoke.click('.send-btn')) return smoke.fail('点击发送', '按钮找不到')

  if (!(await smoke.waitFor('.tool-card', '工具卡出现'))) return smoke.finish()
  smoke.check('工具卡出现', true)
  await smoke.waitText('.tool-card', 'smoke-tool-ok')
  smoke.check('工具卡显示命令输出', smoke.text('.tool-card').indexOf('smoke-tool-ok') >= 0, smoke.text('.tool-card').slice(0, 40))
  smoke.check('工具卡已收尾（不再显示「执行中」）', smoke.text('.tool-card').indexOf('执行中') < 0)

  if (!(await smoke.waitText('.msg-assistant .msg-content', '命令跑完了'))) return smoke.finish()
  smoke.check('回复在工具卡之后上屏', true)

  // Usage reaches the status bar: the cache ring only renders once a rate arrives, so it
  // doubles as "the usage/cost path is wired".
  const ring = await smoke.waitFor('svg.ctx-ring[aria-label^="缓存命中率"]', '用量数字到达（缓存命中率环）')
  if (ring) smoke.check('缓存命中率环渲染', true, ring.getAttribute('aria-label'))

  smoke.check('发送按钮在输入为空时禁用', smoke.q('.send-btn').disabled)
  smoke.finish()
})()
