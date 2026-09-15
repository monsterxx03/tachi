// zoom-fit — the lightbox must never MAGNIFY content, and must always keep it inside the space.
//
// fit() opens a diagram "as large as it fits", which used to apply in both directions: a diagram
// narrower than the window was blown up by whatever the empty space allowed (measured: a two-node
// graph at 277%, and the 6x clamp reachable for anything smaller still) while a wide one opened at
// 94%. The reader's gesture was identical, so the report is "sometimes it opens huge" with no
// trigger anywhere in their own actions — the trigger is the diagram's natural size. Magnifying is
// what the wheel and + are for, so fit stops at 1:1.
//
// Three diagrams make both halves visible: one that fits with room to spare (1:1), one wider than
// the window and one taller (both shrunk, both still inside the area).
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '画三张图')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.chat .mermaid svg', '图表渲染完成', 30000))) return smoke.finish()
  await smoke.waitFor(() => !smoke.q('.stop-btn'), '这一轮结算', 15000)
  await smoke.sleep(400)

  const svgs = smoke.qa('.chat .mermaid svg')
  if (!smoke.check('消息里有三张图', svgs.length === 3, `${svgs.length} 张`)) return smoke.finish()

  // Same accounting as the mermaid-zoom driver: this WebKit reports a rect inside a CSS-zoom
  // subtree in PRE-zoom units, so on-screen size = rect × zoom.
  const box = (el, zoom = 1) => {
    const r = el.getBoundingClientRect()
    return { w: Math.round(r.width * zoom), h: Math.round(r.height * zoom) }
  }

  const names = ['小的（装得下）', '宽的（比窗口宽）', '高的（比窗口高）']
  for (let i = 0; i < svgs.length; i++) {
    const inline = box(svgs[i])
    smoke.click(svgs[i].closest('.mermaid'))
    if (!(await smoke.waitFor('.viewer-overlay .viewer-zoom', `第 ${i + 1} 张浮层打开`, 8000))) return smoke.finish()
    await smoke.sleep(450) // fit() lands in a rAF

    const zoomEl = smoke.q('.viewer-overlay .viewer-zoom')
    const shownSvg = smoke.q('.viewer-overlay .viewer-zoom svg')
    const overlay = smoke.q('.viewer-overlay')
    if (!zoomEl || !shownSvg || !overlay) return smoke.fail('浮层结构', '缺少 viewer-zoom / svg / overlay')
    const z = Number(zoomEl.style.zoom || getComputedStyle(zoomEl).zoom) || 1
    const shown = box(shownSvg, z)
    const label = smoke.text('.viewer-overlay .viewer-zoom-label')
    const cs = getComputedStyle(overlay)
    const areaW = Math.round(overlay.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight))
    const areaH = Math.round(overlay.clientHeight - parseFloat(cs.paddingTop) - parseFloat(cs.paddingBottom))

    smoke.log(`第 ${i + 1} 张图（${names[i]}）`,
      `消息 ${inline.w}×${inline.h} → 浮层 ${shown.w}×${shown.h}（zoom=${z.toFixed(3)} 标签=${label} 可用 ${areaW}×${areaH}）`)
    smoke.check(`第 ${i + 1} 张：点开不会把图放大（zoom ≤ 1）`, z <= 1.0001, `zoom=${z.toFixed(3)}`)
    smoke.check(`第 ${i + 1} 张：点开后装得下（不溢出可用区）`, shown.w <= areaW && shown.h <= areaH,
      `${shown.w}×${shown.h} vs 可用 ${areaW}×${areaH}`)

    smoke.key(document.body, 'Escape')
    if (!(await smoke.waitFor(() => !smoke.q('.viewer-overlay'), '关掉浮层', 3000))) return smoke.finish()
    await smoke.sleep(200)
  }
  smoke.finish()
})()
