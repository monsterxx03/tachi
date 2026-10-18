// preview-dismiss — 预览浮层「点空白处关闭」到底关在哪儿。
//
// 这个场景先报事实再断言：真实的点击是有命中测试的（合成 el.click() 没有），所以先问
// document.elementFromPoint「这个位置上是哪个元素」，再在那儿派发一次真的 click —— 那才是
// 读者点下去会发生的事。
//
// 三处「空白」各问一次：
//   1. 浮层自己的边（backdrop，浮层的 padding 环）；
//   2. 舞台里、白卡外面的空处（文档比窗口窄时两侧的留白）；
//   3. 白卡里面、文字下面的空处（读一篇短文档时最大的一片「空白」）。
//
// 前两处本来就该关；第三处是这一次要改的 —— 但先量出来，别凭猜。
;(async () => {
  if (!(await smoke.waitForSession())) return smoke.finish()
  // 文档正文里的一句话（与 scenarios.go 的 previewAskLine 同一串字）。
  const LINE = '这是一份可以全屏打开、然后选中提问的文档。'

  const openPreview = async () => {
    if (!smoke.q('.file-card')) {
      smoke.type('.composer-input', '把 PREVIEW.md 发给我')
      smoke.click('.send-btn')
      if (!(await smoke.waitFor('.file-card', '文件卡片出现', 30000))) return false
    }
    if (!smoke.q('.file-preview-body')) {
      if (!smoke.click('.file-card .file-btn')) return false
      if (!(await smoke.waitFor('.file-preview-body', '卡片里展开预览', 8000))) return false
    }
    if (!smoke.click('.file-preview-body .file-expand')) return false
    return !!(await smoke.waitFor('.viewer-overlay', '全屏浮层出现', 8000))
  }

  const at = (x, y) => document.elementFromPoint(x, y)
  const describe = (el) => el
    ? el.tagName.toLowerCase() + (el.className && typeof el.className === 'string' ? '.' + el.className.trim().split(/\s+/).join('.') : '')
    : '(空)'
  // clickAt 在那个位置上派发一次真的点击（命中那个元素，冒泡上去）。
  const clickAt = (x, y) => {
    const el = at(x, y)
    if (!el) return false
    el.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, clientX: x, clientY: y }))
    return true
  }

  if (!(await openPreview())) return smoke.finish()

  const ov = smoke.q('.viewer-overlay').getBoundingClientRect()
  const st = smoke.q('.viewer-stage').getBoundingClientRect()
  const doc = smoke.q('.viewer-stage .viewer-doc')
  const dr = doc ? doc.getBoundingClientRect() : null
  smoke.log('几何', [
    `浮层 ${Math.round(ov.left)},${Math.round(ov.top)} ${Math.round(ov.width)}x${Math.round(ov.height)}`,
    `舞台 ${Math.round(st.left)},${Math.round(st.top)} ${Math.round(st.width)}x${Math.round(st.height)}`,
    dr ? `白卡 ${Math.round(dr.left)},${Math.round(dr.top)} ${Math.round(dr.width)}x${Math.round(dr.height)}` : '白卡 (无)',
  ].join(' · '))

  // 三个候选点，按几何挑：浮层边、舞台留白、白卡内空白。
  const ring = { x: Math.round(ov.left + 8), y: Math.round(ov.top + 8) }
  const gutter = dr && dr.left - st.left > 20
    ? { x: Math.round((st.left + dr.left) / 2), y: Math.round(st.top + st.height / 2) }
    : null
  const paperBlank = dr && dr.height > 40
    ? { x: Math.round(dr.left + dr.width / 2), y: Math.round(dr.bottom - 10) }
    : null
  smoke.log('三个点上的元素', [
    `浮层边 ${JSON.stringify(ring)} → ${describe(at(ring.x, ring.y))}`,
    gutter ? `舞台留白 ${JSON.stringify(gutter)} → ${describe(at(gutter.x, gutter.y))}` : '舞台留白 (这个窗口没有)',
    paperBlank ? `白卡内空白 ${JSON.stringify(paperBlank)} → ${describe(at(paperBlank.x, paperBlank.y))}` : '白卡内空白 (没有)',
  ].join(' · '))

  // ── 1. 点浮层边 → 关 ──────────────────────────────────────────────────────
  clickAt(ring.x, ring.y)
  const closedByRing = await smoke.waitFor(() => (!smoke.q('.viewer-overlay') ? true : null), '点浮层边关掉', 3000)
  smoke.check('点浮层自己的边（backdrop）就关掉', !!closedByRing,
    '元素=' + describe(at(ring.x, ring.y)) + ' · 还开着=' + !!smoke.q('.viewer-overlay'))

  // ── 2. 点舞台留白 → 关 ────────────────────────────────────────────────────
  if (gutter) {
    if (!(await openPreview())) return smoke.finish()
    const g2 = (() => {
      const st2 = smoke.q('.viewer-stage').getBoundingClientRect()
      const d2 = smoke.q('.viewer-stage .viewer-doc').getBoundingClientRect()
      return { x: Math.round((st2.left + d2.left) / 2), y: Math.round(st2.top + st2.height / 2) }
    })()
    clickAt(g2.x, g2.y)
    const closedByGutter = await smoke.waitFor(() => (!smoke.q('.viewer-overlay') ? true : null), '点舞台留白关掉', 3000)
    smoke.check('点舞台里、白卡外面的留白也关掉', !!closedByGutter,
      '元素=' + describe(at(g2.x, g2.y)) + ' · 还开着=' + !!smoke.q('.viewer-overlay'))
  }

  // ── 3. 点白卡里面、文字下面的空白 → 也该关 ────────────────────────────────
  if (!(await openPreview())) return smoke.finish()
  const b2 = (() => {
    const d2 = smoke.q('.viewer-stage .viewer-doc').getBoundingClientRect()
    return { x: Math.round(d2.left + d2.width / 2), y: Math.round(d2.bottom - 10) }
  })()
  smoke.log('白卡内空白那一处是', describe(at(b2.x, b2.y)))
  clickAt(b2.x, b2.y)
  const closedByPaper = await smoke.waitFor(() => (!smoke.q('.viewer-overlay') ? true : null), '点白卡内空白关掉', 3000)
  smoke.check('点白卡里文字下面的空白也关掉', !!closedByPaper,
    '元素=' + describe(at(b2.x, b2.y)) + ' · 还开着=' + !!smoke.q('.viewer-overlay'))

  // ── 4. 但点在正文上不能关（「空白处」不等于「任何地方」）──────────────────
  // 这条和上面第 3 条是一对：只放宽到白卡这一层，正文里的字仍然是可以点、可以拖选的。
  if (!(await openPreview())) return smoke.finish()
  const para = smoke.qa('.viewer-stage .viewer-doc *')
    .filter((e) => e.children.length === 0 && smoke.text(e).indexOf(LINE) >= 0)[0]
  if (para) {
    const pr = para.getBoundingClientRect()
    const px = Math.round(pr.left + pr.width / 2)
    const py = Math.round(pr.top + pr.height / 2)
    smoke.log('正文那一点是', describe(at(px, py)))
    clickAt(px, py)
    // 给一次渲染的机会再判：关掉是会立刻发生的，没关就是没关。
    await smoke.sleep(300)
    smoke.check('点正文里的字不关（那里是读的地方，不是空白）', !!smoke.q('.viewer-overlay'),
      '元素=' + describe(at(px, py)) + ' · 还开着=' + !!smoke.q('.viewer-overlay'))
  } else {
    smoke.check('找到正文里的一段字', false, smoke.allText('.viewer-stage').slice(0, 120))
  }

  smoke.finish()
})()
