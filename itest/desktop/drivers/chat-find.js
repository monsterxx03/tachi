// chat-find — ⌘F 搜消息区（issue #35）。
//
// 这个 driver 要钉的两件事，都是"只有真机才看得见"的：
//
//   1. **搜的到底是什么**。一个会话里绝大多数文字是工具的参数与结果，而读者搜自己的对话时指的
//      是"我说过的话"和"模型回答的话"。fixture 里同一个 token 出现三次（Bash 命令里、它的结果
//      里、模型正文里），数出来的条数就是这条断言的判据：只有正文那一次算命中。
//   2. **落点**。转写开屏钉在底部，而 token 在第一条消息里 —— 找到它必须把消息滚进视野，也就是
//      "直接定位到消息"。
//
// 顺带钉住入口：焦点在**输入框**时 ⌘F 也要能用（读者刚打完字就想找上面某句话，这是最常见的一刻），
// 以及 Esc 关掉那一条（不关别的）。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  // 种子会话加载完：最后一条助手消息上屏（fixture 的最后一轮）。
  if (!(await smoke.waitFor(() => smoke.text('.chat').indexOf('第 5 轮的回答') >= 0, '种子转写上屏'))) return smoke.finish()

  const chat = smoke.q('.chat')
  const bar = () => smoke.q('.find-bar.is-chat')
  const count = () => smoke.text('.find-bar.is-chat .find-count')
  const ranges = () => (CSS.highlights.get('tachi-find')?.size || 0)
  // 命中是不是"在视野里"：文字的高亮 Range 落在某个元素上，量那个元素的框，和滚动容器比。
  const hitInView = () => {
    const hl = CSS.highlights.get('tachi-find-current')
    if (!hl || !hl.size) return null
    const el = Array.from(hl)[0]?.startContainer?.parentElement
    if (!el) return null
    const box = el.getBoundingClientRect()
    const view = chat.getBoundingClientRect()
    return box.bottom > view.top && box.top < view.bottom
  }
  const search = async (q) => {
    if (!bar()) smoke.key('.chat', 'f', { metaKey: true })
    if (!(await smoke.waitFor(() => bar(), '查找条出现'))) return false
    smoke.type('.find-bar.is-chat .find-input', q)
    await smoke.sleep(300) // 收集 + 高亮都在 requestAnimationFrame 之后
    return true
  }

  // ── 开屏在底部：要定位的那条消息不在视野里 ────────────────────────────────
  smoke.check('开屏钉在底部（要定位的第一条消息不在视野里）',
    chat.scrollTop > 0 && chat.scrollHeight > chat.clientHeight + 100,
    `scrollTop=${Math.round(chat.scrollTop)} / 可滚 ${Math.round(chat.scrollHeight - chat.clientHeight)}px`)

  // ── ⌘F：焦点在输入框里也要能触发 ────────────────────────────────────────
  // 读者刚打完字就想找上面说过的一句话 —— 这是最常按下 ⌘F 的一刻，所以焦点不该是前提。
  smoke.q('.composer-input').focus()
  smoke.key('.composer-input', 'f', { metaKey: true })
  if (!(await smoke.waitFor(() => bar(), '输入框里按 ⌘F 也打开查找条', 3000))) return smoke.finish()
  smoke.check('焦点在输入框时 ⌘F 也能打开（不用先点消息区）', !!bar(), '')
  smoke.check('打开时光标已经在查找框里（打完字接着敲的就是它）',
    document.activeElement === smoke.q('.find-bar.is-chat .find-input'),
    'activeElement=' + (document.activeElement ? document.activeElement.className : '(无)'))

  // ── 前提：工具的参数与结果真的在 DOM 里 ──────────────────────────────────
  // 成功的调用折进过程条，参数与结果**默认不在 DOM 里** —— 不展开的话，"不搜工具卡"这条断言
  // 是空的（搜不到的东西当然不会被算进去；第一版 A/B 就是这么骗过我的）。所以先像读者那样把
  // 过程条和卡片展开，把那些文字真的放进 DOM。
  smoke.click(smoke.q('.process-head'))
  if (!(await smoke.waitFor('.tool-card', '过程条展开后工具卡出现'))) return smoke.finish()
  smoke.click(smoke.q('.tool-card .tool-head'))
  if (!(await smoke.waitFor(
    () => (smoke.q('.tool-card .tool-args') || smoke.q('.tool-card .tool-summary') ? true : null),
    '工具卡的参数与结果展开'))) return smoke.finish()
  const occurrences = (smoke.text('.chat').match(/CHATFIND-TOOL/g) || []).length
  smoke.check('前提：这个词在 DOM 里出现多次（工具参数 + 工具结果 + 模型正文）',
    occurrences >= 3, `出现 ${occurrences} 次`)

  // ── 范围：用户输入算，模型正文算，工具的参数与结果不算 ──────────────────
  if (!(await search('CHATFIND-USER'))) return smoke.finish()
  smoke.check('用户输入里的词能搜到（1 处）', count() === '1/1', count())
  smoke.check('搜到的那处高亮画在消息里', ranges() === 1, String(ranges()))

  if (!(await search('CHATFIND-TOOL'))) return smoke.finish()
  // 这个 token 在页面上一共出现四次（上面那条前提数过）：卡片标题里的命令、展开后的参数、展开后的
  // 结果，以及模型正文。**只有正文那一次算命中** —— 数出来多于 1 就是工具卡也进了搜索范围
  // （issue #35 的原文要求）。
  smoke.check('同一个词出现在工具的参数与结果里时不计入（只算模型正文那一处）',
    count() === '1/1', `count=${count()}（3 就是工具卡也被搜了）`)
  smoke.check('工具卡里的命中没有被高亮', ranges() === 1, String(ranges()))

  // ── 落点：直接定位到那条消息 ────────────────────────────────────────────
  // 命中的是**第一条**消息（CHATFIND-USER 只在第一条里）。先滚回底部，让"它不在视野里"这件事
  // 成立，再看搜索是否把它带回来 —— 判据是滚动位置真的动了，而不是"碰巧在视野里"。
  chat.scrollTop = chat.scrollHeight
  await smoke.sleep(200)
  const beforeJump = chat.scrollTop
  smoke.check('前提：命中的那条消息此刻不在视野里',
    chat.scrollTop > 200 && !hitInView(), `scrollTop=${Math.round(chat.scrollTop)}`)
  smoke.type('.find-bar.is-chat .find-input', 'CHATFIND-USER')
  const landed = await smoke.waitFor(() => (hitInView() ? true : null), '命中滚进视野', 3000)
  smoke.check('搜到的消息被滚进视野（直接定位到消息，不是只报个数）', !!landed,
    `inView=${hitInView()} scrollTop=${Math.round(beforeJump)} → ${Math.round(chat.scrollTop)}`)
  smoke.check('滚动真的动了，而且是往回走（不是本来就看得见）',
    chat.scrollTop < beforeJump - 50, `${Math.round(beforeJump)} → ${Math.round(chat.scrollTop)}`)
  smoke.check('定位之后自动跟随已让位（读者是来看历史的）',
    chat.scrollTop < chat.scrollHeight - chat.clientHeight - 10,
    `距底 ${Math.round(chat.scrollHeight - chat.clientHeight - chat.scrollTop)}px`)

  // ── 走一遍命中：↓ 在命中之间移动，计数跟着走 ──────────────────────────────
  smoke.type('.find-bar.is-chat .find-input', '第')
  await smoke.sleep(300)
  const many = count()
  smoke.check('常见字命中多处（计数给出位置）', /^\d+\/\d+$/.test(many) && Number(many.split('/')[1]) > 1, many)
  smoke.key('.find-bar.is-chat .find-input', 'Enter')
  await smoke.sleep(300)
  smoke.check('Enter 走到下一个命中', count() !== many && count().indexOf('/') > 0, `${many} → ${count()}`)

  // ── 没有匹配时说的是「无匹配」，不是 0/0 ────────────────────────────────
  smoke.type('.find-bar.is-chat .find-input', '这个词绝对不在会话里-CHATFIND-NOPE')
  await smoke.sleep(300)
  smoke.check('找不到时说「无匹配」', count() === '无匹配', count())
  smoke.check('没有命中时不留高亮', ranges() === 0, String(ranges()))

  // ── Esc 只收起查找条 ───────────────────────────────────────────────────
  smoke.key('body', 'Escape')
  const closed = await smoke.waitFor(() => (!bar() ? true : null), 'Esc 收起查找条', 3000)
  smoke.check('Esc 收起查找条', !!closed, '')
  smoke.check('Esc 没有动别的东西（对话还在，输入框还在）',
    !!smoke.q('.chat') && !!smoke.q('.composer-input') && !!smoke.q('.msg-assistant'), '')
  smoke.check('收起后不留高亮', ranges() === 0, String(ranges()))

  smoke.finish()
})()
