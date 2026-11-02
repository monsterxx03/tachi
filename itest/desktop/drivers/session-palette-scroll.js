// session-palette-scroll — ⌘K 的列表比它的盒子长时，高亮必须跟着滚进视野。
//
// 这条钉的是**我们自己**那条走位：「持续 Ctrl+N 往下走，超过盒子以后屏幕不动」是一个真出现过的
// 故障 —— 选择的行程来自 paletteKeys（受控 value），而 cmdk 自己那次 scrollIntoView 只挂在它
// 自己的 setState 上；受控 value 被直接写进它的 store，绕过了那一步。于是高亮一路往下走、可见
// 区域纹丝不动，而 Enter 打开的是读者根本没看见的那一行。
//
// fixture 是 30 个无项目会话（scenario.seedLoose）+ 冒烟会话，共 31 行，远超 48vh 的列表盒。
// 断言全部量几何：选中行的矩形必须完整落在列表自己的框里，而盒子必须真的滚过（scrollTop > 0）——
// 只看「高亮变了」在坏掉的那一版里也是绿的。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const boxOpen = () => {
    const b = smoke.q('.palette-box')
    return !!b && b.getAttribute('aria-label') === '会话搜索'
  }
  const listEl = () => smoke.q('.palette-list')
  const rowsAll = () => smoke.qa('.palette-row')
  const selIdx = () => rowsAll().findIndex((r) => r.getAttribute('aria-selected') === 'true')
  const selName = () => {
    const r = rowsAll().find((x) => x.getAttribute('aria-selected') === 'true')
    return r ? smoke.text(r.querySelector('.palette-name')) : ''
  }
  // inView 是这条用例的正题：选中行的矩形要在列表盒的矩形里面（留 1px 给圆角/边框的取整）。
  // 注意读的是**列表**的盒子而不是 .palette-box —— 滚的是列表，框自己不动。
  const inView = () => {
    const l = listEl()
    const r = rowsAll().find((x) => x.getAttribute('aria-selected') === 'true')
    if (!l || !r) return null
    const lb = l.getBoundingClientRect()
    const rb = r.getBoundingClientRect()
    return rb.top >= lb.top - 1 && rb.bottom <= lb.bottom + 1
  }
  const scrolled = () => (listEl() ? listEl().scrollTop : -1)
  const step = (k, times) => {
    for (let i = 0; i < times; i++) smoke.key('.palette-input', k, { ctrlKey: true })
  }

  smoke.key(document.body, 'k', { metaKey: true })
  if (!(await smoke.waitFor(() => (boxOpen() ? true : null), '⌘K 打开会话搜索浮层', 8000))) return smoke.finish()
  if (!(await smoke.waitFor(() => (rowsAll().length > 0 ? true : null), '列表出现行', 4000))) return smoke.finish()

  // 前提：盒子是真的装不下。少了这条，下面每一次「滚了」都可能只是首屏本来就放得下。
  const l = listEl()
  smoke.check('前提：列表比它的盒子长（这个 fixture 装不下）',
    !!l && l.scrollHeight > l.clientHeight + 1,
    (l ? l.scrollHeight + ' > ' + l.clientHeight : '(没有列表)') + ' · ' + rowsAll().length + ' 行')
  smoke.check('起手高亮在第一行、盒子在顶部', selIdx() === 0 && scrolled() === 0,
    '选中第 ' + selIdx() + ' 行 · scrollTop=' + scrolled())

  // ── Ctrl+N 走到盒子外面：高亮跟着滚进视野 ─────────────────────────────────
  const DOWN = 20
  step('n', DOWN)
  const walked = await smoke.waitFor(() => (selIdx() === DOWN ? true : null), 'Ctrl+N 走了 20 行', 4000)
  smoke.check('Ctrl+N 把高亮走到了第 21 行（键盘确实在走位）', !!walked, selName() || '(没有高亮行)')
  const downTop = scrolled()
  smoke.check('…而那一行进视野了（列表自己滚了）', inView() === true && downTop > 0,
    '可见=' + inView() + ' · scrollTop=' + downTop)

  // ── Ctrl+P 走回第一行：盒子跟着回到顶部 ───────────────────────────────────
  // 不要求 scrollTop 精确等于 0：列表自己有 6px 内边距，`block: 'nearest'` 只滚到那一行刚好完整
  // 露出来，所以第一行在视野里的位置是 scrollTop=6。要断言的是「盒子回到顶上那一段」，它必须比
  // 在底部时靠上得多。
  step('p', DOWN)
  const back = await smoke.waitFor(() => (selIdx() === 0 ? true : null), 'Ctrl+P 走回第一行', 4000)
  smoke.check('Ctrl+P 走回第一行，盒子也跟着回到顶部', !!back && inView() === true && scrolled() < downTop,
    '选中第 ' + selIdx() + ' 行 · 可见=' + inView() + ' · scrollTop=' + scrolled() + '（底部时 ' + downTop + '）')

  // ── 从第一行往上：环回到末行，那一行同样要进视野（同一个滚动的另一端）────────
  step('p', 1)
  const last = await smoke.waitFor(() => (selIdx() === rowsAll().length - 1 ? true : null),
    'Ctrl+P 从第一行环回到末行', 4000)
  smoke.check('环回到最后一行时它也在视野里（底部不是死角）', !!last && inView() === true && scrolled() > 0,
    '选中第 ' + selIdx() + '/' + rowsAll().length + ' 行 · 可见=' + inView() + ' · scrollTop=' + scrolled())

  smoke.finish()
})()
