// agents-panel — 状态栏的 Agents 按钮与节点面板。
//
// 现场有两个 peer，各代表面板要说的一半（fixture 见 scenarios.go 的 agents-panel 与
// peerfixture.go）：
//
//   - **连不上的那个**（`smoke-nas` → https://127.0.0.1:1，那个端口上没人听，所以探测是立刻被拒而
//     不是等超时）。它给面板三样东西可看，而且每一样都不能靠猜：行里三件事实各就各位（本机配的别名、
//     本机 pin 的身份 —— 这里从来没有连过，所以是「身份未记录」、上次探测的结论）；展开后的详情里有
//     本机用来够它的地址（配置的，不是 card 自称的）、它自己的说法（这里没有，因为它没回答）、以及
//     它在这条消息里该怎么被引用（`#a:smoke-nas`）；刷新是一次真往返，所以它最终说得出「联系不上」，
//     而不是永远停在「检查中…」。
//
//   - **会答话的那个**（`smoke-peer`，runner 起的 fixture）。另一半只有答话才看得到，而它是**卡片自己
//     的字段**：自称（名字 + 版本）、描述、能力、入口，以及技能那张**列表**。列表那三条断言都是排版
//     上的主张 —— 标签只写一次、每条技能一行、名字在行首更亮 —— 也正是没有这个 peer 时没人看得见的
//     东西（一个每条都重复「技能」两个字的版本就这样留在树里）。
//
// 「联系不上」与「技能只写一次」是这一场景的两个关键断言：一个点完刷新仍然停在原地的面板会让读者以为
// 是自己的问题；一个把列表标签按行重复的面板会把每一行的宽度花在重复的话上。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  smoke.click('.agents-btn')
  if (!(await smoke.waitFor('.agents-panel', 'Agents 面板打开'))) return smoke.finish()
  smoke.check('面板标题是 Agents', smoke.text('.mcp-title').indexOf('Agents') >= 0, smoke.text('.mcp-head'))

  // 一个 peer 一块（`.mcp-server`），按别名找它 —— 两个 peer 都在屏幕上，按位置取会读到另一个的字段。
  const boxOf = (alias) => smoke.qa('.agents-panel .mcp-server').find((box) => {
    const row = box.querySelector('.mcp-server-row')
    return !!row && smoke.text(row).indexOf(alias) >= 0
  }) || null
  const rowText = (alias) => {
    const box = boxOf(alias)
    return box ? smoke.text(box.querySelector('.mcp-server-row')) : '(没有这一行)'
  }
  // 展开某一行的详情。details 是按 alias 记的，所以两块可以同时开着 —— 每次只开自己要读的那个。
  const detailOf = (alias) => {
    const box = boxOf(alias)
    return box ? box.querySelector('.agents-detail') : null
  }
  const expand = async (alias) => {
    const box = boxOf(alias)
    if (!box) return null
    if (!detailOf(alias)) box.querySelector('.mcp-server-row').click()
    return smoke.waitFor(() => detailOf(alias), '展开 ' + alias, 4000)
  }

  if (!(await smoke.waitText('.agents-panel', 'smoke-nas', 10000))) return smoke.finish()
  smoke.check('两个 peer 都列出来了（连不上 + 会答话）', !!boxOf('smoke-nas') && !!boxOf('smoke-peer'),
    smoke.allText('.mcp-server-name').join(' | '))
  const row = rowText('smoke-nas')
  smoke.check('行里有本机配的别名', row.indexOf('smoke-nas') >= 0, row)
  // 从来没有出站连过它，所以本机没有可展示的身份 —— 而那个"未知"是如实回答，不是缺数据。
  smoke.check('身份未记录（本机还没跟它连过）', row.indexOf('身份未记录') >= 0, row)
  // 打开面板只读一次（后台那次探测是给它下一次读用的），所以刚打开时行说的是「检查中…」——
  // 那是真的：此刻本机手里还没有结论。让它变新的是那个按钮，下面就按它。
  smoke.check('刚打开时行如实说「检查中…」（面板不轮询，结论要等一次刷新）',
    rowText('smoke-nas').indexOf('检查中') >= 0 && rowText('smoke-peer').indexOf('检查中') >= 0,
    rowText('smoke-nas') + ' / ' + rowText('smoke-peer'))

  // ── 连不上的那一个：地址是配置的，引用是选择器会插的那种拼写 ───────────────
  if (!(await expand('smoke-nas'))) return smoke.finish()
  const detail = smoke.text(detailOf('smoke-nas'))
  smoke.check('展开后给出本机够它的地址', detail.indexOf('https://127.0.0.1:1') >= 0, detail)
  smoke.check('展开后给出该用的引用拼写', detail.indexOf('#a:smoke-nas') >= 0, detail)

  // ── 刷新是一次真的往返：两个节点各自拿到结论（一个有卡、一个没有） ─────────
  smoke.click('.agents-refresh')
  if (!(await smoke.waitText('.agents-panel', '联系不上', 30000))) return smoke.finish()
  smoke.check('刷新之后那个连不上的节点说得出「联系不上」', true, rowText('smoke-nas'))
  const reachable = await smoke.waitFor(
    () => (rowText('smoke-peer').indexOf('可用') >= 0 ? true : null), '会答话的节点变成「可用」', 15000)
  smoke.check('答话的节点报「可用」', !!reachable, rowText('smoke-peer'))
  smoke.check('头部把两个节点的结论合计在一起（1/2 可达）',
    smoke.text('.mcp-head').indexOf('1/2 可达') >= 0, smoke.text('.mcp-head'))
  smoke.check('面板说得出这批结论是什么时候取的',
    smoke.text('.mcp-head').indexOf('上次检查') >= 0, smoke.text('.mcp-head'))
  // 行上说的是结论（「联系不上」），详情里那一行说的是**为什么**。今天它是 Go 那句原话（带着被够的
  // 地址、路径和被拒的原因，一整句英文），所以断言只钉机制：有一句事实可读，而且它**不是**一个编出来的
  // 卡片字段（「它没有回答」与「一个不存在的名字」的区别，正是这一行的意思）。
  const cardLine = (() => {
    const line = Array.from(detailOf('smoke-nas').querySelectorAll('.agents-line'))
      .find((el) => smoke.text(el.querySelector('.agents-k')) === '卡片')
    return line ? smoke.text(line.querySelector('.agents-v')) : ''
  })()
  smoke.check('没有 card 的时候说的是原因，而不是编一个名字',
    cardLine.length > 0 && (cardLine.indexOf('它没有回答') >= 0 || cardLine.indexOf('did not answer with a card') >= 0)
    && cardLine.indexOf('smoke-peer-node') < 0, cardLine)

  // ── 会答话的那一个：卡片自己的字段 ─────────────────────────────────────────
  if (!(await expand('smoke-peer'))) return smoke.finish()
  const card = detailOf('smoke-peer')
  const cardText = smoke.text(card)
  smoke.check('自称：card 自己的名字与版本', cardText.indexOf('smoke-peer-node') >= 0 && cardText.indexOf('v9.9.9') >= 0,
    cardText.slice(0, 120))
  smoke.check('描述是 card 自己写的那句', cardText.indexOf('沙箱里的假节点') >= 0, cardText.slice(0, 200))
  smoke.check('能力按 card 声明的那两项翻成中文', cardText.indexOf('流式响应') >= 0 && cardText.indexOf('推送通知') >= 0,
    cardText.slice(0, 240))
  smoke.check('入口带着 card 的地址与协议', cardText.indexOf('HTTP+JSON 1.0') >= 0, cardText.slice(0, 300))

  // 技能是一张 LIST：标签只写一次，每条技能自己一行，名字在行首更亮。
  const keys = Array.from(card.querySelectorAll('.agents-k')).map((el) => smoke.text(el))
  smoke.check('「技能」只写一次（它是整张列表的标签，不是每行的前缀）',
    keys.filter((t) => t === '技能').length === 1, keys.join(' | '))
  const skills = Array.from(card.querySelectorAll('.agents-skill'))
  const names = skills.map((el) => smoke.text(el.querySelector('.agents-skill-name')))
  smoke.check('三条技能各占一行，顺序就是 card 里的顺序',
    names.join(' · ') === 'docs-read · log-search · snapshot-backfill', names.join(' | '))
  smoke.check('每条技能的名字与它的说明在同一行里（说明跟在名字后面）',
    skills.length === 3 && skills.every((el) => smoke.text(el).indexOf('—') > 0),
    skills.map((el) => smoke.text(el).slice(0, 40)).join(' | '))
  // 排版：一行一条（各自占一整行），并且都挂在「技能」那个标签的右边 —— 缩进是「这些属于上面那个
  // 字段」的表达，挤到标签左边就等于没有归属。
  if (skills.length === 3) {
    const tops = skills.map((el) => Math.round(el.getBoundingClientRect().top))
    smoke.check('三条技能各占自己的一行（不是挤成一段）', new Set(tops).size === 3, tops.join(' | '))
    const label = Array.from(card.querySelectorAll('.agents-k')).find((el) => smoke.text(el) === '技能')
    const labelRight = label ? label.getBoundingClientRect().right : 0
    smoke.check('技能都挂在「技能」标签的右侧（同一列）',
      skills.every((el) => el.getBoundingClientRect().left >= labelRight - 1),
      '标签右缘 ' + Math.round(labelRight) + ' · 技能左缘 ' +
        skills.map((el) => Math.round(el.getBoundingClientRect().left)).join('/'))
  }

  // 点面板外面关掉它（自然行为，且必须真的关）。这里派发的是 mousedown 而不是 click：面板听的
  // 是 mousedown（一次按下才算「点进去」），而 smoke.click 只发 click 事件。
  smoke.q('.chat-content').dispatchEvent(new MouseEvent('mousedown', { bubbles: true }))
  await smoke.sleep(150)
  smoke.check('点外面关掉面板', !smoke.q('.agents-panel'), '')
  smoke.finish()
})()
