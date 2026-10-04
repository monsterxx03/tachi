// sidebar-text — the sidebar's type scale and its disclosure carets.
//
// Two reports live here: "项目字体太小了" (the group headers were 14px against 13.5px rows, a
// hierarchy that held on paper and was invisible on screen) and "折叠箭头小的看不清" (the group
// caret was 9px of ▾ — a dot). The assertions are about INTENT, not numbers: a floor, a strict
// order, and one shared caret size — so retuning the scale later is not a failure.
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
    '分段标签 .session-section': '.sidebar .session-section',
    '新建项目 .new-project': '.sidebar .new-project',
    '新建会话 .new-chat': '.sidebar .new-chat',
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
  // below it, not as one more row.
  smoke.check('组头严格大于行标题（层级看得出来）', sizes['组头 .proj-name'] > sizes['行标题 .session-title'],
    `组头 ${sizes['组头 .proj-name']}px vs 行标题 ${sizes['行标题 .session-title']}px`)

  // Which group a row belongs to has to be visible, not inferred from a 12px difference in left
  // margin ("项目里的会话和无项目的会话看不出区别"): the project's rows hang off a RAIL, the 无项目
  // bucket's do not. Measured on the row's own parent, because that is where the rail lives — the
  // rows themselves are the same box in both cases, which is exactly why the old shape was
  // ambiguous. Intent, not a number: the rail only has to EXIST on one side.
  const railOf = (el) => {
    const p = el.parentElement
    return p ? parseFloat(getComputedStyle(p).borderLeftWidth) : 0
  }
  const projRows = smoke.qa('.sidebar .proj-rows .session')
  const looseRows = smoke.qa('.sidebar .session-list > .session')
  smoke.check('项目组的行挂在一根竖线上', projRows.length > 0 && projRows.every((r) => railOf(r) > 0),
    `${projRows.length} 行，竖线 ${projRows.map((r) => railOf(r)).join('/')}px`)
  smoke.check('无项目的行没有竖线（差别不是「左边距窄一点」）',
    looseRows.length > 0 && looseRows.every((r) => railOf(r) === 0),
    `${looseRows.length} 行，竖线 ${looseRows.map((r) => railOf(r)).join('/')}px`)
  smoke.check('竖线是实线、够粗（不是一根看不清的发丝线）',
    projRows.length > 0 && (() => {
      const rail = getComputedStyle(smoke.qa('.sidebar .proj-rows')[0])
      return rail.borderLeftStyle === 'solid' && parseFloat(rail.borderLeftWidth) >= 2
    })(),
    projRows.length ? getComputedStyle(smoke.qa('.sidebar .proj-rows')[0]).borderLeftWidth : '没有项目组')

  // The carets: an SVG icon (not the ▸/▾ glyphs, whose size follows the font and draws a dot at
  // any size a sidebar can afford), every one of them the same box, and that box is big enough.
  const token = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--caret-size'))
  smoke.log('共用 token', `--caret-size = ${token}px`)
  const carets = { '组头折叠 .proj-toggle': '.sidebar .proj-toggle svg.caret' }
  for (const [k, sel] of Object.entries(carets)) {
    const el = smoke.q(sel)
    if (!smoke.check(`${k} 是个图标（不是文字字形）`, !!el, el ? el.tagName.toLowerCase() : '找不到 svg.caret')) continue
    const boxPx = el.getBoundingClientRect().width
    smoke.check(`${k} 用的是共用箭头尺寸`, Math.abs(boxPx - token) <= 1, `${Math.round(boxPx)}px（token ${token}px）`)
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
    smoke.check(`${k} 画出来的箭头看得清`, drawnH >= 7,
      `${drawnH.toFixed(1)}px 高（盒 ${Math.round(boxPx)}px，占 viewBox ${(fill * 100).toFixed(0)}%）`)
  }

  // The other half of the rail rule: an EMPTY group keeps the indent but has no rail to hang rows
  // off. Its group is folded at boot (nothing was ever in it — only the boot session's project is
  // revealed), so it has to be opened first.
  const emptyHead = smoke.qa('.sidebar .proj-head')
    .find((h) => smoke.text(h.querySelector('.proj-name')) === 'sidebar-empty')
  if (emptyHead) emptyHead.querySelector('.proj-toggle').click()
  const emptyHint = await smoke.waitFor('.sidebar .proj-rows .proj-empty', '空项目展开', 4000)
  smoke.check('空项目的「还没有会话」旁边没有竖线（没有行可挂）',
    !!emptyHint && getComputedStyle(emptyHint.parentElement).borderLeftColor.replace(/\s/g, '') === 'rgba(0,0,0,0)',
    emptyHint ? getComputedStyle(emptyHint.parentElement).borderLeftColor : '(没展开)')

  smoke.finish()
})()
