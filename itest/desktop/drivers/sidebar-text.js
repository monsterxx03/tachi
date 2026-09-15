// sidebar-text — the sidebar's type scale and its disclosure carets.
//
// Two reports live here: "项目字体太小了" (the group headers were 14px against 13.5px rows, a
// hierarchy that held on paper and was invisible on screen) and "折叠箭头小的看不清" (the group
// caret was 9px of ▾ — a dot). The assertions are about INTENT, not numbers: a floor, a strict
// order, and one shared caret size — so retuning the scale later is not a failure.
;(async () => {
  if (!(await smoke.waitFor('.sidebar .proj-name', '侧栏渲染出项目组', 15000))) return smoke.finish()

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

  smoke.finish()
})()
