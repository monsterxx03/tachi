// session-palette — ⌘K 会话搜索：按标题在对话之间跳。
//
// 与 project-palette 是同一个位置上的第二个浮层（⌘P 搜项目、⌘K 搜会话），所以要钉的六件事都在这
// 个浮层自己的契约上：
//   1. 列的是「对话」——一行一个，顺序就是 ListSessions 的顺序（最近更新在前），项目名写在行里
//      但不参与过滤（那是 ⌘P 的活，空态必须指向它，否则读者只会在同一个框里重打同一个词）；
//   2. 中文输入法：组合中的拼音**不参与过滤**（列表与高亮都不动），组合里的 Enter/Esc 属于输入法
//      —— 前者是选词、后者是取消候选，都不该切会话或关掉浮层；落字（compositionend）之后才搜；
//   3. 回车真的切会话，并把焦点交回输入框（与 ⌘P 同一条规则：浮层的输入框一卸载，焦点就落到
//      <body>，下一次敲键哪儿都不去）；
//   4. 选中的会话在**折叠的项目组**里时，那个组自己展开（同 ⌘P 的理由：折叠的组只剩一个组头，
//      刚变 active 的那一行藏在里面，等于没送到）；
//   5. Esc 什么都不做（不切会话、不增删会话）；
//   6. ⌘P 与 ⌘K 互斥：它们是同一个位置上的同一个框，不能同时挂在屏幕上。
//
// fixture 是三个会话：一个项目成员（冒烟会话）+ 两个无项目的（无项目会话、另一个会话）。启动时
// 打开的是最新的那个（项目成员），所以它的组是展开的、而「无项目」那一组是折叠的（分组默认折叠，
// 唯一被启动打开的是当前会话所在的那一组）—— 折叠那一步由 driver 自己按，因为「折叠」本身是读者
// 自己的动作。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()
  if (!(await smoke.waitFor('.session.active', '侧栏出现当前会话行', 10000))) return smoke.finish()

  const PROJECT = 'smoke-sesspal'
  // 会话浮层是按 aria-label 认的：两个浮层共用同一套 class（同一个框），问「哪一个开着」只能看这里。
  const boxLabel = () => {
    const b = smoke.q('.palette-box')
    return b ? b.getAttribute('aria-label') : ''
  }
  const boxOpen = () => boxLabel() === '会话搜索'
  const names = () => smoke.allText('.palette-name')
  const rowCount = () => smoke.qa('.palette-row').length
  const selected = () => smoke.text('.palette-row[aria-selected="true"] .palette-name')
  const sessionId = () => smoke.q('.app').getAttribute('data-session-id')
  const activeTitle = () => smoke.text('.session.active .session-title')
  // 侧栏里有多少条会话：**不能数 `.session`** —— 同时只开一组（聚焦规则），所以那只数得到展开那一组
  // 的行。数的是每个组头自己报的数（项目是它的成员数，桶是 looseMembers）—— 折着的组也读得到。
  const sidebarTotal = () => smoke.qa('.proj-count')
    .reduce((n, el) => n + (parseInt(smoke.text(el).replace(/[^0-9]/g, ''), 10) || 0), 0)
  const focusedComposer = () => document.activeElement === smoke.q('.composer-input')
  const activeDesc = () => {
    const a = document.activeElement
    return a ? (a.className || a.tagName) : '(没有焦点)'
  }
  // 组头 → 它那一组的行。折叠的组会把 .proj-rows 整个从 DOM 里摘掉，所以往后找时必须在下一个
  // .proj-head 处停下，否则会一路走到别的项目组的行里去（project-palette 里踩过这个）。
  const headOf = (name) => smoke.qa('.proj-head').find((h) => {
    const n = h.querySelector('.proj-name')
    return !!n && n.textContent.trim() === name
  })
  const expandedOf = (name) => {
    const h = headOf(name)
    const t = h && h.querySelector('.proj-toggle')
    return !!t && t.getAttribute('aria-expanded') === 'true'
  }
  const rowsOf = (name) => {
    const h = headOf(name)
    if (!h) return []
    let el = h.nextElementSibling
    while (el && !el.classList.contains('proj-rows')) {
      if (el.classList.contains('proj-head')) return []
      el = el.nextElementSibling
    }
    return el ? Array.from(el.querySelectorAll('.session')) : []
  }
  // 输入法：compositionstart / compositionend 是原生事件，React 在根上监听它们，driver 直接派发
  // 即可。真实输入法的顺序是「先把落字后的文本写进输入框，再发 compositionend」，这里照抄。
  const compose = (type, el) => el.dispatchEvent(new CompositionEvent(type, { bubbles: true }))

  const boot = sessionId()
  const bootTitle = activeTitle()
  smoke.check('起手：打开的是最新的那个会话', boot !== '' && bootTitle !== '', boot + ' / ' + bootTitle)

  // 起手的分组状态本身就是一条断言：屏幕上那个会话在项目里，所以只有项目组开着 —— 桶折着不是因为
  // 它空，而是因为读者不在里面（聚焦规则）。
  //
  // 它也是下面两次「没有增删会话」为什么不数 `.session` 的答案：同时只开一组，行数只是「展开那一组
  // 的行数」，而组头自己报的数（sidebarTotal）在折着的时候也读得到。
  smoke.check('起手：当前会话所在的项目组开着、「无项目」折着（同时只留一组）',
    expandedOf(PROJECT) && !expandedOf('无项目'),
    '项目=' + expandedOf(PROJECT) + ' · 桶=' + expandedOf('无项目'))
  smoke.check('起手：各组头报的会话数加起来是三个（下面两次「没有增删」的基准）',
    sidebarTotal() === 3, sidebarTotal() + ' 条')

  // ── 1. ⌘K 打开：列的是对话，一行一个，顺序就是列表自身的顺序 ──────────────────
  smoke.key(document.body, 'k', { metaKey: true })
  if (!(await smoke.waitFor(() => (boxOpen() ? true : null), '⌘K 打开会话搜索浮层', 8000))) return smoke.finish()
  smoke.check('三个会话各一行（一行一个对话）', rowCount() === 3, names().join(' | '))
  smoke.check('最近更新的排第一（就是启动时打开的那个）', names()[0] === bootTitle,
    names()[0] + ' vs ' + bootTitle)
  // 项目会话与无项目会话必须一眼分得开：有项目的挂**实心标签**，无项目的是**裸文字**（有标签这件事
  // 本身就是「属于某个项目」的信号），而且第二行必须一样高 —— 靠键盘走的列表里，一行比邻行高就会把
  // 整个列表挪到读者眼皮底下。
  const tagStyles = (sel) => smoke.qa(sel).map((el) => {
    const cs = getComputedStyle(el)
    return { border: cs.borderTopStyle, h: Math.round(el.getBoundingClientRect().height) }
  })
  const projTags = smoke.allText('.palette-proj')
  const looseTags = smoke.allText('.palette-loose')
  smoke.check('项目成员行挂着项目名的标签，无项目的行写着「无项目」',
    projTags.length === 1 && projTags[0] === PROJECT && looseTags.length === 2 && looseTags.every((t) => t === '无项目'),
    projTags.concat(looseTags).join(' | '))
  const projStyle = tagStyles('.palette-proj')
  const looseStyle = tagStyles('.palette-loose')
  smoke.check('有项目的挂标签、没有的是裸文字（不是「两种标签换个颜色」）',
    projStyle.every((s) => s.border === 'solid') && looseStyle.every((s) => s.border === 'none'),
    projStyle.map((s) => s.border).join('/') + ' vs ' + looseStyle.map((s) => s.border).join('/'))
  const heights = projStyle.concat(looseStyle).map((s) => s.h)
  smoke.check('挂标签和没挂的第二行一样高（行高不随内容变）',
    heights.length === 3 && new Set(heights).size === 1, heights.join(' | '))
  const rowHeights = smoke.qa('.palette-row').map((r) => Math.round(r.getBoundingClientRect().height))
  smoke.check('所有行一样高（键盘滚动时列表不会跳）',
    new Set(rowHeights).size === 1, rowHeights.join(' | '))
  smoke.check('当前会话那行标着「当前会话」', smoke.qa('.palette-here').length === 1,
    smoke.allText('.palette-here').join(' | '))
  const times = smoke.allText('.palette-meta')
  smoke.check('每行都写着上次更新的时间（顺序才不是凭空的）',
    times.length === 3 && times.every((t) => t.length > 0), times.join(' | '))

  // 输入框下面没有分隔线：框和列表是同一块面（框只是坐在这块面的最上面），一条细线会把浮层切成两半。
  // 这条只有 computed style 看得见 —— 它不影响任何文本或几何，所以没有这条检查的话，谁把它加回来
  // 都不会有东西响。
  const inputBorder = getComputedStyle(smoke.q('.palette-input')).borderBottomStyle
  smoke.check('搜索框下面没有分隔线（框和列表是同一块面）', inputBorder === 'none', inputBorder)

  // 高亮是**两重信号**：实心块 + 提亮的标题（没有左侧强调竖条）。两重都要在，因为一条靠键盘走的
  // 列表里，块本身只是一层淡填充 —— 在深色的面板上它与邻行的差别只有一点点，而「哪一行」是读者
  // 唯一要在这张列表上读出来的东西。这是「绝不只靠颜色（一层填充）」那条规则的机器可读形式：两条
  // 都按**相对**比较（选中 vs 邻行），所以它不锁住任何具体色值，换主题、调色都照过。
  const rowsEls = smoke.qa('.palette-row')
  // The highlight is cmdk's `aria-selected` now (the row is no longer tagged with a class): the
  // two signals themselves are unchanged, and this driver only looks for WHICH row carries them.
  const isSel = (r) => r.getAttribute('aria-selected') === 'true'
  const selEl = rowsEls.find(isSel)
  const plainEl = rowsEls.find((r) => !isSel(r))
  const bgOf = (el) => (el ? getComputedStyle(el).backgroundColor : '(没有这一行)')
  const inkOf = (el) => (el ? getComputedStyle(el.querySelector('.palette-name')).color : '(没有这一行)')
  smoke.check('选中的行是一个实心块：填充与邻行不同，且不是透明的',
    !!selEl && !!plainEl && bgOf(selEl) !== 'rgba(0, 0, 0, 0)' && bgOf(selEl) !== bgOf(plainEl),
    bgOf(selEl) + ' vs ' + bgOf(plainEl))
  smoke.check('…而它的标题同时被提亮（两重信号，不是「换一层底色」而已）',
    !!selEl && !!plainEl && inkOf(selEl) !== inkOf(plainEl), inkOf(selEl) + ' vs ' + inkOf(plainEl))

  // ── 2. 过滤的是**标题**；项目名不是标题 ────────────────────────────────────
  smoke.type('.palette-input', '冒烟')
  const hit = await smoke.waitFor(() => (rowCount() === 1 ? true : null), '按标题过滤到一个会话', 4000)
  smoke.check('按标题过滤：只剩冒烟会话', !!hit && names()[0] === '冒烟会话', names().join(' | '))
  smoke.type('.palette-input', PROJECT)
  const none = await smoke.waitFor('.palette-empty', '项目名搜不到会话', 4000)
  smoke.check('搜项目名没有结果，空态指向 ⌘P',
    !!none && smoke.text('.palette-empty').indexOf(smoke.MOD + 'P') >= 0, smoke.text('.palette-empty'))

  // ── 3. 中文输入法：组合不参与过滤，落字才参与 ───────────────────────────────
  // 这三条是「打字过程中不要有干扰」的全部内容：列表不动（拼音不是查询），输入框不动（不打断候选），
  // 组合里的 Enter 不动（那是选词，不是打开会话）。
  smoke.type('.palette-input', '')
  if (!(await smoke.waitFor(() => (rowCount() === 3 ? true : null), '清空后回到全量', 4000))) return smoke.finish()
  const box = smoke.q('.palette-input')
  compose('compositionstart', box)
  smoke.type('.palette-input', 'maoyan')
  await smoke.sleep(150)
  smoke.check('组合中的拼音不参与过滤（列表不动）', rowCount() === 3, names().join(' | '))
  smoke.check('组合中的拼音照常留在输入框里（没有把候选打断）', box.value === 'maoyan', box.value)
  smoke.key('.palette-input', 'Enter')
  await smoke.sleep(150)
  smoke.check('组合中的 Enter 不切会话、也不关浮层',
    boxOpen() && sessionId() === boot, (boxOpen() ? '浮层还在' : '浮层被关了') + ' / ' + sessionId())
  // 落字：真实输入法是先把提交后的文本写进输入框，再发 compositionend。
  smoke.type('.palette-input', '冒烟')
  compose('compositionend', box)
  const landed = await smoke.waitFor(() => (rowCount() === 1 && names()[0] === '冒烟会话' ? true : null),
    '落字之后才按它过滤', 4000)
  smoke.check('落字（compositionend）之后才按它过滤', !!landed, names().join(' | '))

  // ── 4. 回车切会话（Ctrl+N 走位），焦点交回输入框 ────────────────────────────
  // Ctrl+N / Ctrl+P 是 TUI 那套键位（readline 的 next/previous），在带输入框的浮层里顶替 ↑↓。
  smoke.type('.palette-input', '')
  if (!(await smoke.waitFor(() => (rowCount() === 3 ? true : null), '清空后回到全量', 4000))) return smoke.finish()
  smoke.key('.palette-input', 'n', { ctrlKey: true })
  smoke.key('.palette-input', 'n', { ctrlKey: true })
  const walked = await smoke.waitFor(() => (selected() === '无项目会话' ? true : null), 'Ctrl+N 走到最后一个会话', 4000)
  smoke.check('Ctrl+N 下移高亮（不靠鼠标）', !!walked, selected() || '(没有高亮行)')
  smoke.key('.palette-input', 'Enter')
  const switched = await smoke.waitFor(() => (sessionId() !== boot ? true : null), '回车切到选中的会话', 10000)
  smoke.check('回车切到选中的会话', !!switched, sessionId())
  smoke.check('切到的正是高亮的那一个（无项目会话）', activeTitle() === '无项目会话', activeTitle())
  await smoke.waitGone('.palette-box', '选完关掉浮层', 4000)
  smoke.check('选完浮层就关掉，不留着挡路', !smoke.q('.palette-box'), boxLabel() || '(已关)')
  const refocused = await smoke.waitFor(() => (focusedComposer() ? true : null), '焦点回到输入框', 3000)
  smoke.check('切完会话焦点落在输入框（可以直接打字）', !!refocused, activeDesc())
  smoke.check('⌘K 全程没有增删会话', sidebarTotal() === 3, sidebarTotal() + ' 条')

  // ── 5. 送读者进一个折着的组：那一组自己打开，别的组收起来 ────────────────────
  // 聚焦规则（屏幕上那个会话在哪个组，就只有那个组开着）在这一节被两个方向夹住。它同时证明了那条
  // 规则**不需要读者配合**：上一节把读者切到了桶里，所以项目组此刻本来就是折着的，而一个折着的组
  // 里那些行根本不在 DOM 里 —— 于是「⌘K 读的是会话列表，不是 DOM」这句话也有了着力点。
  const expandedLabel = (name) => {
    const h = headOf(name)
    return h ? 'aria-expanded=' + h.querySelector('.proj-toggle').getAttribute('aria-expanded') : '(没有这一组)'
  }
  const head = headOf(PROJECT)
  if (!head) return smoke.fail('找到项目组头', smoke.allText('.proj-name').join(' | '))
  smoke.check('读者在桶里：项目组自己是折着的，它的行也不在 DOM 里',
    !expandedOf(PROJECT) && rowsOf(PROJECT).length === 0,
    expandedLabel(PROJECT) + ' · 行 ' + rowsOf(PROJECT).length)
  smoke.check('而读者所在的那一组是开着的（同时只留一组）', expandedOf('无项目'), expandedLabel('无项目'))

  smoke.key(document.body, 'k', { metaKey: true })
  if (!(await smoke.waitFor(() => (boxOpen() ? true : null), '⌘K 再次打开', 8000))) return smoke.finish()
  smoke.check('折着的组里那一行照样在列表里（⌘K 读的是会话列表，不是 DOM）',
    names().indexOf('冒烟会话') >= 0, names().join(' | '))
  smoke.type('.palette-input', '冒烟')
  if (!(await smoke.waitFor(() => (rowCount() === 1 ? true : null), '过滤到成员会话', 4000))) return smoke.finish()
  smoke.key('.palette-input', 'Enter')
  const revealed = await smoke.waitFor(() => (expandedOf(PROJECT) ? true : null), '⌘K 打开后项目组展开', 8000)
  smoke.check('⌘K 送进折着的那一组：那一组自己打开', !!revealed, rowsOf(PROJECT).length + ' 行')
  smoke.check('…而桶跟着收起来（同时只留一组）', !expandedOf('无项目'), expandedLabel('无项目'))
  smoke.check('展开之后 active 就是那一行', activeTitle() === '冒烟会话', activeTitle())

  // 「无项目」那一组走同一条规则（它排在最后，也是唯一没有项目的那个）：这一半钉的是
  // `sessionGroupKey` —— 少了那一句，⌘K 会把行选中却让那一组一直折着，屏上什么都看不见。
  smoke.key(document.body, 'k', { metaKey: true })
  if (!(await smoke.waitFor(() => (boxOpen() ? true : null), '⌘K 再次打开（送进桶里）', 8000))) return smoke.finish()
  smoke.type('.palette-input', '无项目会话')
  if (!(await smoke.waitFor(() => (rowCount() === 1 ? true : null), '过滤到桶里的那个会话', 4000))) return smoke.finish()
  smoke.key('.palette-input', 'Enter')
  const bucketRevealed = await smoke.waitFor(() => (expandedOf('无项目') ? true : null), '⌘K 打开后「无项目」组也展开', 8000)
  smoke.check('⌘K 送进折着的「无项目」组：那一组自己打开',
    !!bucketRevealed, rowsOf('无项目').length + ' 行')
  smoke.check('…而项目组收起来了（同时只留一组）', !expandedOf(PROJECT), expandedLabel(PROJECT))
  smoke.check('展开之后 active 就是桶里那一行', activeTitle() === '无项目会话', activeTitle())

  // ── 6. ⌘P / ⌘K 互斥；Esc 什么都不做 ────────────────────────────────────────
  smoke.key(document.body, 'p', { metaKey: true })
  const projPalette = await smoke.waitFor(() => (boxLabel() === '项目搜索' ? true : null), '⌘P 打开项目搜索', 8000)
  smoke.check('⌘P 打开的是项目搜索（同一个框，另一个面板）', !!projPalette, boxLabel())
  smoke.key(document.body, 'k', { metaKey: true })
  const swapped = await smoke.waitFor(() => (boxOpen() ? true : null), '⌘K 顶掉 ⌘P', 8000)
  smoke.check('⌘K 打开时关掉 ⌘P：屏幕上只有一个框', !!swapped && smoke.qa('.palette-box').length === 1,
    smoke.qa('.palette-box').map((b) => b.getAttribute('aria-label')).join(' | '))
  const idBefore = sessionId()
  smoke.key('.palette-input', 'Escape')
  await smoke.waitGone('.palette-box', 'Esc 关闭浮层', 4000)
  smoke.check('Esc 关掉了浮层', !smoke.q('.palette-box'), boxLabel() || '(已关)')
  smoke.check('Esc 没有切会话', sessionId() === idBefore, sessionId())
  smoke.check('Esc 没有增删会话', sidebarTotal() === 3, sidebarTotal() + ' 条')

  smoke.finish()
})()
