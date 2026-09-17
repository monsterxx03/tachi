// project-palette — ⌘P 项目搜索：一个键盘入口把人送进项目里。
//
// 这个场景要钉的是三件在界面上看不出来的事：
//   1. 回车打开的是该项目「最近」的会话，不是碰巧正开着的那个。所以先用 ⌘N 现造一个成员
//      （它同时是 createdAt 与 updatedAt 最新的那个，两种「最新」的定义在这里重合，断言
//      才只有一个正确答案），再切回旧成员，然后 ⌘P 回车：active 必须回到项目组的第一行。
//   2. 空项目回车会真的建出会话，而且**绑在该项目上** —— 前端只看得见行数变了，绑定关系
//      由 Go 侧读 meta.json 的 projectId 断言（NewSession 在项目不能驱动时会静默建一个
//      不属于任何项目的会话，那正是这一条要防的）。
//   3. Esc 什么也不做：不建会话、不切会话。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitFor('.proj-head', '侧栏出现项目组头', 10000))) return smoke.finish()

  // 组头 → 它那一组的行。两个项目名一个包含另一个（smoke-pal / smoke-pal-empty），所以按
  // .proj-name 精确比，不能拿组头文本找子串。
  //
  // 折叠的组会把自己的 .proj-rows 整个从 DOM 里摘掉，所以往后找时必须在下一个 .proj-head 处停下：
  // 不停的话会一路走到**别的项目**的 .proj-rows，把它的行报成本组的（实测：折叠后 smoke-pal 的
  // 行数变成了 smoke-pal-empty 的 1 行）。折叠与否只认 aria-expanded，不认行数。
  const headOf = (name) => smoke.qa('.proj-head').find((h) => {
    const n = h.querySelector('.proj-name')
    return !!n && n.textContent.trim() === name
  })
  const expandedOf = (name) => {
    const h = headOf(name)
    const t = h && h.querySelector('.proj-toggle')
    return !!t && t.getAttribute('aria-expanded') === 'true'
  }
  const groupOf = (name) => {
    const head = headOf(name)
    if (!head) return null
    let el = head.nextElementSibling
    while (el && !el.classList.contains('proj-rows')) {
      if (el.classList.contains('proj-head')) return null
      el = el.nextElementSibling
    }
    return el
  }
  const rowsOf = (name) => {
    const g = groupOf(name)
    return g ? Array.from(g.querySelectorAll('.session')) : []
  }
  const activeIndex = (name) => rowsOf(name).findIndex((r) => r.classList.contains('active'))
  const selectedName = () => {
    const sel = smoke.q('.palette-row.is-sel')
    const n = sel && sel.querySelector('.palette-name')
    return n ? n.textContent.trim() : ''
  }
  // 焦点在哪儿：「活动元素就是输入框」。⌘P 是唯一一个关掉浮层再去换会话的入口，浮层的输入框一卸载，
  // 焦点就落到 <body>，下一次敲键哪儿都不去——所以这条得等（waitFor），不能只看一眼。
  const focusedComposer = () => document.activeElement === smoke.q('.composer-input')
  const activeDesc = () => {
    const a = document.activeElement
    return a ? (a.className || a.tagName) : '(没有焦点)'
  }

  // ── 1. 起手：显式进项目会话（谁被启动时选中是 fixture 的顺序细节），再 ⌘N 添一个成员 ──
  const seeded = rowsOf('smoke-pal')[0]
  if (!seeded) return smoke.fail('找到项目里的成员行', rowsOf('smoke-pal').length + ' 行')
  seeded.click()
  if (!(await smoke.waitFor(() => (activeIndex('smoke-pal') === 0 ? true : null), '当前会话是项目成员', 10000))) {
    return smoke.finish()
  }
  smoke.key(document.body, 'n', { metaKey: true })
  const grew = await smoke.waitFor(() => (rowsOf('smoke-pal').length === 2 ? true : null), '⌘N 之后项目里有两个成员', 10000)
  smoke.check('⌘N 在项目里造出新成员', !!grew, rowsOf('smoke-pal').length + ' 行')

  // 退到旧的那个成员：不这样做，「回车打开最近的会话」就没有可观测的差别。
  const older = rowsOf('smoke-pal')[1]
  if (!older) return smoke.fail('拿到旧成员行', rowsOf('smoke-pal').length + ' 行')
  older.click()
  const away = await smoke.waitFor(() => (activeIndex('smoke-pal') === 1 ? true : null), '当前会话切到旧成员', 10000)
  smoke.check('先切到旧成员（active 下标 1，不是最近那个）', !!away, 'active 下标 ' + activeIndex('smoke-pal'))

  // ── 2. ⌘P：每一行都要说清回车会打开什么，而不是只列名字 ──────────────────
  smoke.key(document.body, 'p', { metaKey: true })
  if (!(await smoke.waitFor('.palette-box', '项目搜索浮层打开', 8000))) return smoke.finish()
  const names = smoke.allText('.palette-name')
  smoke.check('两个项目都在列表里', names.length === 2, names.join(' | '))
  smoke.check('有会话的项目在前（后端按创建时间倒序）', names[0] === 'smoke-pal', names.join(' | '))
  const rows = smoke.qa('.palette-row')
  const act0 = rows[0] ? smoke.text(rows[0].querySelector('.palette-action')) : ''
  const act1 = rows[1] ? smoke.text(rows[1].querySelector('.palette-action')) : ''
  smoke.check('有会话的项目写着会打开哪个会话', act0.indexOf('→') === 0 && act0.indexOf('新建') < 0, act0)
  smoke.check('空项目写着会新建会话', act1.indexOf('新建会话') >= 0, act1)
  smoke.check('空项目说明自己还没有会话', smoke.text(rows[1]).indexOf('还没有会话') >= 0, smoke.text(rows[1]))

  smoke.key('.palette-input', 'Enter')
  const back = await smoke.waitFor(() => (activeIndex('smoke-pal') === 0 ? true : null), '回车后回到项目里最近的会话', 10000)
  smoke.check('回车切到项目最近的会话（组里第一行）', !!back, 'active 下标 ' + activeIndex('smoke-pal'))
  await smoke.waitGone('.palette-box', '浮层关闭', 4000)
  smoke.check('打开之后就关掉，不留着挡路', !smoke.q('.palette-box'), smoke.text('.palette-box') || '(已关)')
  const refocused = await smoke.waitFor(() => (focusedComposer() ? true : null), '焦点回到输入框', 3000)
  smoke.check('切完会话焦点落在输入框（可以直接打字）', !!refocused, activeDesc())

  // ── 3. 过滤；Esc 什么都不做 ─────────────────────────────────────────────
  smoke.key(document.body, 'p', { metaKey: true })
  if (!(await smoke.waitFor('.palette-box', '浮层再次打开', 8000))) return smoke.finish()
  smoke.type('.palette-input', 'zzz')
  const none = await smoke.waitFor('.palette-empty', '说明没有匹配的项目', 4000)
  smoke.check('没有匹配就说没有，而不是一片空白', !!none, smoke.text('.palette-empty'))
  smoke.type('.palette-input', 'empty')
  const one = await smoke.waitFor(() => (smoke.qa('.palette-row').length === 1 ? true : null), '按项目名过滤', 4000)
  smoke.check('过滤到空项目那一个', !!one && selectedName() === 'smoke-pal-empty', smoke.allText('.palette-name').join(' | '))
  smoke.key('.palette-input', 'Escape')
  await smoke.waitGone('.palette-box', 'Esc 关闭浮层', 4000)
  smoke.check('Esc 没有建会话', rowsOf('smoke-pal-empty').length === 0, rowsOf('smoke-pal-empty').length + ' 行')
  smoke.check('Esc 没有切会话', activeIndex('smoke-pal') === 0, 'active 下标 ' + activeIndex('smoke-pal'))

  // ── 4. 空项目：回车建出它的第一个会话（高亮靠 Ctrl+N / Ctrl+P 走） ────────────
  // Ctrl+N / Ctrl+P 是 TUI 那套键位（readline 的 next/previous），在有输入框的浮层里顶替 ↑↓。
  // Cocoa 默认把这两个键当成光标上下移动，所以 handler 必须自己 preventDefault——驱动只能证明
  // 高亮动了，够钉住「这两个键确实被这一步接管了」；键盘焦点之外的效果看不到。
  smoke.key(document.body, 'p', { metaKey: true })
  if (!(await smoke.waitFor('.palette-box', '浮层第三次打开', 8000))) return smoke.finish()
  smoke.key('.palette-input', 'n', { ctrlKey: true })
  const moved = await smoke.waitFor(() => (selectedName() === 'smoke-pal-empty' ? true : null), 'Ctrl+N 把高亮下移', 4000)
  smoke.check('Ctrl+N 下移高亮（不靠鼠标）', !!moved, selectedName() || '(没有高亮行)')
  smoke.key('.palette-input', 'p', { ctrlKey: true })
  const up = await smoke.waitFor(() => (selectedName() === 'smoke-pal' ? true : null), 'Ctrl+P 把高亮上移', 4000)
  smoke.check('Ctrl+P 上移高亮', !!up, selectedName() || '(没有高亮行)')
  smoke.key('.palette-input', 'n', { ctrlKey: true })
  if (!(await smoke.waitFor(() => (selectedName() === 'smoke-pal-empty' ? true : null), '高亮回到空项目', 4000))) {
    return smoke.finish()
  }
  smoke.key('.palette-input', 'Enter')
  const created = await smoke.waitFor(() => (rowsOf('smoke-pal-empty').length === 1 ? true : null), '空项目里建出会话', 10000)
  smoke.check('空项目回车建出第一个会话', !!created, rowsOf('smoke-pal-empty').length + ' 行')
  smoke.check('新会话就是当前会话', activeIndex('smoke-pal-empty') === 0, 'active 下标 ' + activeIndex('smoke-pal-empty'))
  const chip = await smoke.waitFor(
    () => (smoke.text('.work-dir').indexOf('smoke-pal-empty') >= 0 ? true : null),
    'chip 指向新会话所属的项目', 8000)
  smoke.check('chip 显示空项目的名字（会话确实属于它）', !!chip, smoke.text('.work-dir'))
  await smoke.waitGone('.palette-box', '建完关掉浮层', 4000)
  // 新建那一路走的是 newChat，它自己会在建完会话后聚焦输入框；但 ⌘P 这条路上浮层还在卸载，
  // 所以这里同样要等一次——同一个理由，两条分支都得钉。
  const refocusedNew = await smoke.waitFor(() => (focusedComposer() ? true : null), '焦点回到输入框（新建那一路）', 3000)
  smoke.check('新建出来的会话焦点也在输入框', !!refocusedNew, activeDesc())

  // ── 5. 折叠的项目组：⌘P 进去之后它必须自己展开 ──────────────────────────
  // ⌘P 的意思是「带我去那个项目」，而读者的下一个动作是看侧栏：折叠的组只剩一个组头，刚变 active
  // 的那一行藏在里面，等于没送到。但折叠是读者自己按出来的，所以这条规则只属于 ⌘P 这条入口——别的
  // 路径进来不该替他展开，否则「把正在用的项目收起来」就永远做不到。
  const head = headOf('smoke-pal')
  if (!head) return smoke.fail('找到 smoke-pal 的组头', smoke.allText('.proj-name').join(' | '))
  smoke.check('起手时该项目是展开的', expandedOf('smoke-pal'), 'aria-expanded=' + head.querySelector('.proj-toggle').getAttribute('aria-expanded'))
  head.querySelector('.proj-toggle').click()
  const folded = await smoke.waitFor(() => (expandedOf('smoke-pal') ? null : true), '手动折叠该项目', 4000)
  smoke.check('先手动把项目收起来（组头说自己收起了）', !!folded, 'aria-expanded=' + (folded ? 'false' : 'true'))
  if (!folded) return smoke.finish()
  smoke.check('收起后它的行不再留在 DOM 里', rowsOf('smoke-pal').length === 0,
    rowsOf('smoke-pal').length + ' 行（折叠时容器被摘掉）')

  smoke.key(document.body, 'p', { metaKey: true })
  if (!(await smoke.waitFor('.palette-box', '浮层第四次打开', 8000))) return smoke.finish()
  if (selectedName() !== 'smoke-pal') return smoke.fail('高亮在 smoke-pal 上（回车才会打开它）', selectedName())
  smoke.key('.palette-input', 'Enter')
  const revealed = await smoke.waitFor(() => (expandedOf('smoke-pal') ? true : null), '⌘P 打开后项目展开', 8000)
  smoke.check('⌘P 打开的项目组自动展开', !!revealed, rowsOf('smoke-pal').length + ' 行')
  smoke.check('展开之后行又看得见（2 个成员）', rowsOf('smoke-pal').length === 2, rowsOf('smoke-pal').length + ' 行')
  smoke.check('展开之后 active 就在那一组里', activeIndex('smoke-pal') === 0, 'active 下标 ' + activeIndex('smoke-pal'))

  smoke.finish()
})()
