// sidebar-text — the sidebar's type scale, its disclosure carets, and the rail its rows hang off.
//
// Two reports live here: "项目字体太小了" (the group headers were 14px against 13.5px rows, a
// hierarchy that held on paper and was invisible on screen) and "折叠箭头小的看不清" (the group
// caret was 9px of ▾ — a dot). The assertions are about INTENT, not numbers: a floor, a strict
// order, and one shared caret size — so retuning the scale later is not a failure.
//
// Every group wears the same header (a project's and the 无项目 bucket's alike), so each half below
// covers all of them rather than the first one it finds.
;(async () => {
  if (!(await smoke.waitFor('.sidebar .proj-name', '侧栏渲染出项目组', 15000))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const px = (sel) => {
    const el = smoke.q(sel)
    return el ? parseFloat(getComputedStyle(el).fontSize) : 0
  }
  const parts = {
    '组头 .proj-name': '.sidebar .proj-name',
    '组计数 .proj-count': '.sidebar .proj-count',
    '行标题 .session-title': '.sidebar .session-title',
    '行时间 .session-meta': '.sidebar .session-meta',
    '桶组头 .proj-head.is-loose .proj-name': '.sidebar .proj-head.is-loose .proj-name',
    '新建项目 .new-project': '.sidebar .new-project',
    '组头 ＋ .proj-add': '.sidebar .proj-add',
    '页脚 .footer-item': '.sidebar .footer-item',
  }
  const sizes = {}
  for (const [k, sel] of Object.entries(parts)) sizes[k] = px(sel)
  smoke.log('侧栏字号', JSON.stringify(sizes))

  const missing = Object.entries(sizes).filter(([, v]) => v === 0).map(([k]) => k)
  if (!smoke.check('每一块都在（选择器没写错）', missing.length === 0, missing.join(' / '))) return smoke.finish()

  const tooSmall = Object.entries(sizes).filter(([, v]) => v < 12)
  smoke.check('侧栏里没有小于 12px 的文字', tooSmall.length === 0,
    tooSmall.length ? JSON.stringify(tooSmall) : `最小 ${Math.min(...Object.values(sizes))}px`)

  // The hierarchy the complaint was about: a group heading must read as the PARENT of the rows
  // below it, not as one more row. The bucket's heading is the same rule — it is a group heading
  // like a project's, not a quieter caption.
  smoke.check('组头严格大于行标题（层级看得出来）', sizes['组头 .proj-name'] > sizes['行标题 .session-title'],
    `组头 ${sizes['组头 .proj-name']}px vs 行标题 ${sizes['行标题 .session-title']}px`)
  smoke.check('「无项目」的组头跟项目的组头同一档',
    sizes['桶组头 .proj-head.is-loose .proj-name'] === sizes['组头 .proj-name'],
    `${sizes['桶组头 .proj-head.is-loose .proj-name']}px vs ${sizes['组头 .proj-name']}px`)

  // Which group a row belongs to is read from the INDENT, and nothing else: every group's rows sit
  // one step inside their container, and the container is the only thing that says whose they are.
  //
  // There used to be a rail here as well (2px of --border-2 down the left of .proj-rows), on the
  // argument that indentation alone is ambiguous. It was removed on 2026-10-17: in a dense list it
  // drew a vertical line down the whole sidebar, one per group, which read as a second column of
  // separators rather than as grouping. So what this pins is that the indent EXISTS and is a real
  // step (not 1–2px), measured on the row's own parent — the rows themselves are the same box in
  // every group, which is exactly why a margin-only difference was once called ambiguous.
  const indentOf = (el) => {
    const p = el.parentElement
    return p ? parseFloat(getComputedStyle(p).marginLeft) : 0
  }
  const projRows = smoke.qa('.sidebar .proj-rows:not(.is-loose) .session')
  smoke.check('项目组的行缩进了一档（读得出它属于这个组）', projRows.length > 0 && projRows.every((r) => indentOf(r) >= 8),
    `${projRows.length} 行，缩进 ${projRows.map((r) => indentOf(r)).join('/')}px`)
  smoke.check('组容器上没有竖线（2026-10-17 去掉了那条导轨）',
    projRows.length > 0 && (() => {
      const rail = getComputedStyle(smoke.qa('.sidebar .proj-rows')[0])
      return !rail.borderLeftStyle || rail.borderLeftStyle === 'none' || parseFloat(rail.borderLeftWidth) === 0
    })(),
    projRows.length ? getComputedStyle(smoke.qa('.sidebar .proj-rows')[0]).borderLeftWidth : '没有项目组')

  // The bucket's half of the rail rule needs a row in it, and this fixture's only project-less session
  // is not the one the app opened — so the driver makes another through the bucket's own ＋. That is
  // the same click a reader makes, and it carries the second half of the rule with it: the group it
  // creates a row in has to OPEN, or the new row lands out of sight (a folded group renders no rows
  // at all, and this one starts folded — the boot session is a project member).
  //
  // The new row is identified by the id the app is now showing, not by counting: counting from a
  // FOLDED group starts at zero (the rows are not in the DOM), so "+1" would be measuring the fold
  // rather than the creation. State before/after, for the same reason this driver waits rather than
  // sleeps.
  const looseRows = () => smoke.qa('.sidebar .proj-rows.is-loose .session')
  const shownId = () => (smoke.q('.app') ? smoke.q('.app').getAttribute('data-session-id') : '') || ''
  const beforeCreate = shownId()
  if (!smoke.click('.sidebar .proj-head.is-loose .proj-add'))
    return smoke.fail('「无项目」组头上有 ＋', smoke.allText('.sidebar .proj-head').join(' | '))
  const created = await smoke.waitFor(
    () => {
      const id = shownId()
      if (!id || id === beforeCreate) return null
      return looseRows().some((r) => r.getAttribute('data-sid') === id) ? looseRows() : null
    }, '在「无项目」组里建出一条会话', 8000)
  smoke.check('桶的 ＋ 建出的行就落在桶里（那一组自己展开）',
    !!created, (created ? created.length + ' 行' : '没有那一行') + '，当前会话 ' + shownId())
  smoke.check('无项目桶的行也缩进了一档（跟项目组是同一套壳）',
    !!created && created.every((r) => indentOf(r) >= 8),
    created ? created.map((r) => indentOf(r)).join('/') + 'px' : '(没有行)')
  smoke.check('一行挂的是它自己那组的容器（没串到别的组）',
    !!created && created.every((r) => r.closest('.proj-rows').classList.contains('is-loose')),
    created && created[0].closest('.proj-rows')
      ? created[0].closest('.proj-rows').className : '(没有行)')

  // The carets: an SVG icon (not the ▸/▾ glyphs, whose size follows the font and draws a dot at
  // any size a sidebar can afford), every one of them the same box, and that box is big enough.
  const token = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--caret-size'))
  smoke.log('共用 token', `--caret-size = ${token}px`)
  const toggles = smoke.qa('.sidebar .proj-toggle')
  smoke.check('每个组头都有自己的折叠箭头（项目与桶一起）', toggles.length >= 3, toggles.length + ' 个')
  toggles.forEach((t, i) => {
    const el = t.querySelector('svg.caret')
    if (!smoke.check(`第 ${i + 1} 个组头的箭头是个图标（不是文字字形）`, !!el,
      el ? el.tagName.toLowerCase() : '找不到 svg.caret')) return
    const boxPx = el.getBoundingClientRect().width
    smoke.check(`第 ${i + 1} 个组头的箭头用的是共用尺寸`, Math.abs(boxPx - token) <= 1, `${Math.round(boxPx)}px（token ${token}px）`)
    // What decides whether a chevron can be made out is how big it is DRAWN: the box times the
    // chevron's share of the viewBox (getBBox is in viewBox units, so the box has to come in). A dot
    // inside a correctly-sized box passes every box check there is — the first version of this icon
    // drew a 3.8-unit chevron in a 12-unit viewBox and would have looked fine on paper. The floor is
    // about the drawn size only, which leaves the exact number to the designer (13px was too heavy
    // in the diff's dense rows; 11px is not, and an 8–9px glyph was a dot).
    const draw = el.getBBox()
    const v = el.viewBox.baseVal
    const drawnH = draw.height * (boxPx / v.height)
    const fill = Math.max(draw.width / v.width, draw.height / v.height)
    smoke.check(`第 ${i + 1} 个组头的箭头画出来看得清`, drawnH >= 7,
      `${drawnH.toFixed(1)}px 高（盒 ${Math.round(boxPx)}px，占 viewBox ${(fill * 100).toFixed(0)}%）`)
  })

  // The other half of the rail rule: an EMPTY group keeps the indent but has no rail to hang rows
  // off. Its group is folded at boot (nothing was ever in it — only the boot session's project is
  // revealed), so it has to be opened first.
  // The empty group: no rows to indent, so the container must not draw anything of its own either
  // (that was the rail's old exception — with the rail gone, the check is that no group draws one).
  const emptyHead = smoke.qa('.sidebar .proj-head')
    .find((h) => smoke.text(h.querySelector('.proj-name')) === 'sidebar-empty')
  if (emptyHead) emptyHead.querySelector('.proj-toggle').click()
  const emptyHint = await smoke.waitFor('.sidebar .proj-rows:not(.is-loose) .proj-empty', '空项目展开', 4000)
  smoke.check('空项目的容器上也没有竖线',
    !!emptyHint && (() => {
      const rail = getComputedStyle(emptyHint.parentElement)
      return !rail.borderLeftStyle || rail.borderLeftStyle === 'none' || parseFloat(rail.borderLeftWidth) === 0
    })(),
    emptyHint ? getComputedStyle(emptyHint.parentElement).borderLeftWidth : '(没展开)')

  smoke.finish()
})()
