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
  const groupOf = (name) => {
    const head = smoke.qa('.proj-head').find((h) => {
      const n = h.querySelector('.proj-name')
      return !!n && n.textContent.trim() === name
    })
    if (!head) return null
    let el = head.nextElementSibling
    while (el && !el.classList.contains('proj-rows')) el = el.nextElementSibling
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

  // ── 4. 空项目：回车建出它的第一个会话 ───────────────────────────────────
  smoke.key(document.body, 'p', { metaKey: true })
  if (!(await smoke.waitFor('.palette-box', '浮层第三次打开', 8000))) return smoke.finish()
  smoke.key('.palette-input', 'ArrowDown')
  const moved = await smoke.waitFor(() => (selectedName() === 'smoke-pal-empty' ? true : null), '↓ 把高亮移到空项目', 4000)
  smoke.check('↓ 移动高亮（不靠鼠标）', !!moved, selectedName() || '(没有高亮行)')
  smoke.key('.palette-input', 'Enter')
  const created = await smoke.waitFor(() => (rowsOf('smoke-pal-empty').length === 1 ? true : null), '空项目里建出会话', 10000)
  smoke.check('空项目回车建出第一个会话', !!created, rowsOf('smoke-pal-empty').length + ' 行')
  smoke.check('新会话就是当前会话', activeIndex('smoke-pal-empty') === 0, 'active 下标 ' + activeIndex('smoke-pal-empty'))
  const chip = await smoke.waitFor(
    () => (smoke.text('.work-dir').indexOf('smoke-pal-empty') >= 0 ? true : null),
    'chip 指向新会话所属的项目', 8000)
  smoke.check('chip 显示空项目的名字（会话确实属于它）', !!chip, smoke.text('.work-dir'))
  await smoke.waitGone('.palette-box', '建完关掉浮层', 4000)

  smoke.finish()
})()
