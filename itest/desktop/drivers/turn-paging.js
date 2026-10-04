// turn-paging — 一轮 turn 跨过分页边界时，它必须仍然是**一张**卡片。
//
// 转写分页载入（PAGE_SIZE 条原始记录一页，"滚到顶再往前取"另走一个 binding），而**每页各自跑一遍
// buildTurns**：于是一轮跨越分页边界的长回合会渲染成每页一张卡片 —— 每张都有自己的一段过程条、
// 以及同一份「本轮改动」chips（issue #29 的截图正是三张卡、chips 相同）。buildTurns 只在 user
// 记录处开新卡（steer 也会在中间留一个 user 气泡），所以"助手紧贴助手"只可能是分页缝，
// prependSessionPage 负责把两半接回一张卡。
//
// fixture（scenarios.go 的 longTurnPagesFixture）：第一轮 121 条记录（40 组 assistant+tool_call+
// tool_result），第二轮与第三轮的正文很长 —— 长不是为好看，是为了让转写**真的能滚动**，否则
// "滚到顶"这个动作根本不会发生（折叠起来的一轮只有一行高）。
//
// 断言都落在"卡片张数"上，不依赖过程条的文案：三轮 = 三张助手卡；修好之前，长回合被缝切成两张，
// 总数变成四。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const rowsBefore = smoke.allText('.session-title')
  const row = smoke.qa('.session').find((s) => smoke.text(s.querySelector('.session-title')) === '另一个会话')
  if (!row) return smoke.fail('侧栏里有「另一个会话」', rowsBefore.join(' | '))
  smoke.click(row)
  // 等转写**真的画出来**再量基线：刚点完那一刻两条都是 0，后面的"变多了"就变成了恒真。
  if (!(await smoke.waitFor(() => (smoke.q('.msg-assistant') ? true : null), '首访的转写渲染出来', 15000))) return smoke.finish()

  // 首屏只有最新一页：长回合的尾巴 + 后两轮。
  const users0 = smoke.qa('.msg-user').length
  const cards0 = smoke.qa('.msg-assistant').length
  smoke.log('首屏（最新一页）', `user=${users0} assistant=${cards0}`)
  smoke.check('首屏就有后两轮（最新一页铺满了）', users0 >= 2 && cards0 >= 2,
    `user=${users0} assistant=${cards0}`)

  // 滚到顶 → 触发上翻加载，更早的一页被 prepend。它落在长回合的**中间**。
  const chat = smoke.q('.chat')
  if (!chat) return smoke.fail('找到转写容器 .chat', '')
  const geo = () => {
    const el = smoke.q('.chat')
    return el ? `top=${Math.round(el.scrollTop)} h=${el.scrollHeight} c=${el.clientHeight}` : '(没有 .chat)'
  }
  smoke.log('滚动前', geo())
  const scrolled = await smoke.waitFor(() => {
    const el = smoke.q('.chat')
    if (!el || el.scrollHeight <= el.clientHeight + 10) return null // 不可滚就没得测
    el.scrollTop = 0
    el.dispatchEvent(new Event('scroll'))   // 合成一次，免得"本来就是 0"时没有事件
    return el.scrollTop <= 40 ? true : null
  }, '滚到转写顶部')
  smoke.log('滚到顶之后', geo())
  if (!scrolled) return smoke.fail('转写可以滚动（fixture 的正文够长）', `scrollHeight=${chat.scrollHeight} clientHeight=${chat.clientHeight}`)

  // 上翻加载是异步的：等更早那一页的消息真的出现（user 行数变多）。
  const loaded = await smoke.waitFor(() => {
    const users = smoke.qa('.msg-user').length
    return users > users0 ? users : null
  }, '更早的一页被加载', 10000)
  if (!loaded) return smoke.fail('上翻加载发生了', `user=${smoke.qa('.msg-user').length}（起始 ${users0}）`)

  const users = smoke.qa('.msg-user').length
  const cards = smoke.qa('.msg-assistant').length
  smoke.check('三轮 = 三张助手卡片（长回合没有被分页缝切开）', users === 3 && cards === 3,
    `user=${users} assistant=${cards}`)

  // 而且第一张卡片真的**装着整个长回合**：展开它的过程条，40 张步骤卡都在这一张里。
  const first = smoke.qa('.msg-assistant')[0]
  const strip = first ? first.querySelector('.process-head') : null
  if (!strip) return smoke.fail('第一张卡片有过程条', first ? first.innerHTML.slice(0, 200) : '(没有卡片)')
  smoke.click(strip)
  const steps = await smoke.waitFor(() => {
    const n = smoke.qa('.tool-status').length
    return n >= 40 ? n : null
  }, '长回合的 40 步都在第一张卡片里', 10000)
  smoke.check('第一张卡片独占了长回合的全部 40 步', !!steps,
    `${steps || smoke.qa('.tool-status').length} 张步骤卡`)

  smoke.finish()
})()
