// steer-fold — 插话（steer）会改变折叠的形状吗？会。
//
// 回合进行中排队的消息在**工具调用的间隙**被自动插入（不是「立即发送」那条打断路径），而
// `injectSteerVisual` 同时把这一轮**切成两段**：封住正在流的那一段（它的最后一段 text 就此成为
// **该段的结论**），插入插话自己那条用户气泡，再开一条新的助手消息承接后面的输出。
//
// 每一段都是独立的助手消息，各按自己的 parts 折叠（`turnView` 是 per message 的），所以：
//   · 插话之前那段正文 → 它那一段的结论 → **恒显**（在没插话的一轮里，这种中间正文通常是被折起来的）；
//   · 插话之后那段的中间正文 → 照旧折进过程条（和没插话时同一规则）；
//   · 过程条是每段一条（N 次插话 = N+1 条），步数/失败数都按段统计，不是整轮。
//
// 驱动这一半断"屏幕上是什么"，Go 那一半断"插话真的作为下一轮 user 消息进了模型"（第 2 次请求）。
;(async () => {
  const textOf = (el) => (el ? el.textContent.replace(/\s+/g, ' ').trim() : '')

  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  smoke.type('.composer-input', '跑一条慢命令')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.stop-btn', '回合开始（停止按钮出现）', 15000))) return smoke.finish()

  // 慢命令（sleep 3）还在跑的时候排队一条。不点「立即发送」：这里要的就是自动插入那条路。
  smoke.type('.composer-input', '插队这条：先做这件')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.pending-bar', '排队条出现（回合进行中，消息进队列）', 5000))) return smoke.finish()
  smoke.check('排队条说明会自动插入（走 steer，不是打断）',
    smoke.text('.pending-title').indexOf('自动插入') >= 0, smoke.text('.pending-title'))

  // 插话落地：这一轮被切成两段。这一步就是"会改变折叠行为"的现场。
  if (!(await smoke.waitFor(() => smoke.qa('.msg-assistant').length === 2, '插话把这一轮切成两段', 20000))) return smoke.finish()
  smoke.check('插话自己是一条用户气泡', smoke.qa('.msg-user').length === 2, smoke.qa('.msg-user').length + ' 条')

  // 回合结束再断言，避免读到中间态。
  if (!(await smoke.waitText('.msg-assistant .msg-content', '两段都跑完了。', 30000))) return smoke.finish()
  if (!(await smoke.waitFor(() => !smoke.q('.stop-btn'), '回合结束（停止按钮消失）'))) return smoke.finish()

  const segs = smoke.qa('.msg-assistant')
  smoke.check('回合结束后仍是两段（没有并回一段）', segs.length === 2, segs.length + ' 段')
  if (segs.length !== 2) return smoke.finish()
  const seg1 = segs[0]
  const seg2 = segs[1]
  const t1 = textOf(seg1)
  const t2 = textOf(seg2)

  smoke.check('两段各有一条过程条', smoke.qa('.process-head').length === 2, smoke.qa('.process-head').length + ' 条')
  smoke.check('第 1 段的步数只算自己那一段',
    textOf(seg1.querySelector('.process-head')).indexOf('1 步') >= 0, textOf(seg1.querySelector('.process-head')))

  // 插话之前那段正文：成了它那一段的结论 → 恒显（这一段没有别的正文）。
  smoke.check('插话之前那段正文恒显（它是那一段的结论）', t1.indexOf('第一段：先跑一条慢命令。') >= 0, t1.slice(0, 120))
  // 它那一步成功了 → 折进过程条，折叠态下没有卡片；展开后卡片回来（是折起来，不是丢掉）。
  smoke.check('第 1 段成功的步骤折进过程条', seg1.querySelectorAll('.tool-status').length === 0,
    seg1.querySelectorAll('.tool-status').length + ' 张卡')
  smoke.click(seg1.querySelector('.process-head'))
  if (!(await smoke.waitFor(() => smoke.qa('.msg-assistant')[0].querySelectorAll('.tool-status').length === 1,
    '展开第 1 段后卡片回来'))) return smoke.finish()
  smoke.check('第 1 段展开后工具卡在（内容没丢）', true, '')

  // 插话之后那一段：中间正文照旧折起来，收尾结论恒显——与没插话的一轮同一规则。
  smoke.check('第 2 段中间正文折起来（同一规则，不受插话影响）',
    t2.indexOf('收到插话了：这是插话之后那一段的开头。') < 0, t2.slice(0, 120))
  smoke.check('第 2 段收尾结论恒显', t2.indexOf('两段都跑完了。') >= 0, t2.slice(-80))
  smoke.finish()
})()
