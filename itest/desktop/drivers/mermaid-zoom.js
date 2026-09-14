// mermaid-zoom — 消息区里的 mermaid 图点开后，必须比在消息里更大，而不是缩成一个小方块。
//
// 这里量三件事：消息里的 svg 尺寸、浮层里同一个 svg 的尺寸（含 `.viewer-zoom` 上的 CSS zoom）、
// 以及承载它的 stage 尺寸。三者一起才能说明「默认特别小」是哪一步出了问题——是 fit 算错了，
// 还是图在浮层里根本没有拿到它该有的固有尺寸。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '画一张流程图')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.chat .mermaid svg', '图表在消息里渲染完成', 30000))) return smoke.finish()
  await smoke.waitFor(() => !smoke.q('.stop-btn'), '这一轮结算', 15000)
  await smoke.sleep(200)

  // 这个 WebKit 对 CSS zoom 子树报的 rect 是「预缩放」单位：手动设 zoom=3 时纸张读 303，而它的父
  // （stage，没被 zoom）读 909 = 303×3。所以屏幕上看到的尺寸 = rect × zoom —— 断言按这个口径写，
  // 否则 zoom 有多大就等于看不见（第一版断言就是这么被 0.245 骗过去的）。
  const box = (el, zoom = 1) => {
    const r = el.getBoundingClientRect()
    return { w: Math.round(r.width * zoom), h: Math.round(r.height * zoom) }
  }
  const inlineSvg = smoke.q('.chat .mermaid svg')
  const inline = box(inlineSvg)
  smoke.log('消息里的图', `${inline.w}×${inline.h}`)

  // 点图 = 全屏查看（图本身可点，右上角还有一个 ⤢）。
  smoke.click('.chat .mermaid')
  if (!(await smoke.waitFor('.viewer-overlay .viewer-zoom', '浮层打开', 8000))) return smoke.finish()
  await smoke.sleep(400) // fit() 在 rAF 里量尺寸，等它落地

  const zoomEl = smoke.q('.viewer-overlay .viewer-zoom')
  const stage = smoke.q('.viewer-overlay .viewer-stage')
  const overlay = smoke.q('.viewer-overlay')
  const shownSvg = smoke.q('.viewer-overlay .viewer-zoom svg')
  const snap = (tag) => {
    const z = Number(zoomEl.style.zoom || getComputedStyle(zoomEl).zoom) || 1
    smoke.log(`浮层尺寸 @${tag}`, JSON.stringify({
      zoom: z,
      overlay: { w: overlay.clientWidth, h: overlay.clientHeight },
      stage: { w: stage.clientWidth, h: stage.clientHeight, maxW: getComputedStyle(stage).maxWidth },
      paper: box(zoomEl, z),
      paperW: getComputedStyle(zoomEl).width,
      svg: shownSvg ? box(shownSvg, z) : null,
      svgAttr: shownSvg ? { width: shownSvg.getAttribute('width'), style: shownSvg.getAttribute('style') } : null,
    }))
  }
  snap('fit（刚打开）')
  // 决定性探针：CSS zoom 到底影不影响视觉/布局？手动设 1 与 3，各量一次 svg 的 rect。

  // 归 1 看固有尺寸，再 fit 一次看它算成什么——把「固有尺寸错」与「fit 公式错」分开。
  smoke.key(document.body, '0')
  await smoke.sleep(300)
  snap('zoom=1')
  smoke.key(document.body, '1')
  await smoke.sleep(300)
  snap('再 fit')

  // 判定用「屏幕上实际多大」：svg 自己的 rect 已经含了祖先的 CSS zoom（WebKit 会把它算进
  // getBoundingClientRect），这正是读者看到的大小。
  const zoomNow = Number(zoomEl.style.zoom || getComputedStyle(zoomEl).zoom) || 1
  const shown = shownSvg ? box(shownSvg, zoomNow) : { w: 0, h: 0 }
  const areaW = Math.round(overlay.clientWidth - parseFloat(getComputedStyle(overlay).paddingLeft) - parseFloat(getComputedStyle(overlay).paddingRight))
  smoke.log('最终', `zoom=${zoomNow} 视觉 svg=${shown.w}×${shown.h} 可用宽=${areaW} 消息里=${inline.w}×${inline.h}`)
  smoke.check('浮层里的图比消息里的大（点开是为了看细节）', shown.w > inline.w,
    `消息 ${inline.w}×${inline.h} → 浮层 ${shown.w}×${shown.h}（zoom=${zoomNow}）`)
  smoke.check('浮层里的图没有被压成小方块（占满可用宽度的大半）',
    shown.w >= areaW * 0.6,
    `图 ${shown.w}px vs 可用 ${areaW}px（zoom=${zoomNow}）`)
  // 宽高比按 RELATIVE 判：这个 fixture 的图在消息里只有 ~40px 高，1px 的取整就能让一个
  // ~17.7 的绝对比值动 0.45 —— 绝对容差于是变成彩票，取决于容器宽度和 mermaid 那一刻的快照。
  // 「拉坏」指的是形状变了，而真被拉坏是几十个百分点：3% 远松于任何真实畸变，又对取整免疫。
  const ratioGap = Math.abs(shown.w / shown.h - inline.w / inline.h) / (inline.w / inline.h)
  smoke.check('图的宽高比没被拉坏',
    ratioGap < 0.03,
    `消息 ${(inline.w / inline.h).toFixed(2)} vs 浮层 ${(shown.w / shown.h).toFixed(2)}（差 ${(ratioGap * 100).toFixed(1)}%）`)

  smoke.key(document.body, 'Escape')
  if (!(await smoke.waitFor(() => !smoke.q('.viewer-overlay'), 'Esc 关掉浮层', 3000))) return smoke.finish()
  smoke.finish()
})()
