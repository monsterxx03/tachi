// parallel-tools — 一个回复里同名工具被调用多次时，每张卡都要落上自己的参数。
//
// 事件的到达顺序是"先全部 start，后全部 args"（start 由消费流的一侧发、那时还没有参数；args 由执行
// 侧在动手前一起发）。前端原本按「工具名 + 最新未完成」认卡，于是四个同名的调用里：每个 args 都盖在
// 最新那张上 —— 只有最后一张有标题与参数，前三张永远停在「执行中…」（issue #39）。现在按 tool call
// id 认卡（agent:tool 载荷里带 toolCallId，tool_result 走 agent:event 的 ToolID）。
//
// 断言看两处：卡片标题（= 参数摘要，读的是这条调用自己的参数）与卡片正文里的「执行中…」。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  smoke.type('.composer-input', '把四份都读一遍')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant', '四份都读完了', 30000))) return smoke.finish()

  // 成功的一步收在过程条里，先像用户那样展开。
  const strip = smoke.q('.process-head')
  if (!strip) return smoke.fail('过程条存在', smoke.text('.chat-content').slice(-120))
  smoke.click(strip)
  if (!(await smoke.waitFor(() => (smoke.qa('.tool-status').length >= 4 ? true : null), '展开后四张工具卡都在'))) return smoke.finish()

  // 卡片的外层是 .tool-card；.tool-status 只是头部右边那个 ✓/✗。
  const cards = smoke.qa('.tool-card')
  smoke.check('四条 ReadFile 是四张卡（没有被合成一张）', cards.length === 4, cards.length + ' 张')

  const names = ['a.md', 'b.md', 'c.md', 'd.md']
  const titles = cards.map((c) => {
    const t = c.querySelector('.tool-title')
    return t ? smoke.text(t) : ''
  })
  // 每张卡显示的是**自己**的参数摘要。修好之前，只有最后一张有标题（args 全被写到了它身上），
  // 其余三张的标题为空 —— 这正是「前面三个点开都是执行中」。
  smoke.check('每张卡都显示自己的参数摘要（读的是哪个文件）',
    names.every((n, i) => (titles[i] || '').indexOf(n) >= 0), titles.map((t) => t || '(空)').join(' | '))
  smoke.check('卡片顺序与调用顺序一致', titles.join(',').indexOf('a.md') === 0, titles.join(' | '))

  const busy = cards.filter((c) => smoke.text(c).indexOf('执行中') >= 0)
  smoke.check('没有卡片停在「执行中…」（四条都收到了自己的结果）', busy.length === 0,
    busy.length + ' 张还在执行中')

  smoke.finish()
})()
