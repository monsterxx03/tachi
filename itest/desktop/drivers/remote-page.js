// remote-page — 远端机器页盖住会话壳时，壳的绘制状态对不对。
//
// 这一条钉的是一个**看得见但量不出来**的 bug：远端页（和设置页、定时任务页一样）盖住整个
// 窗口，而它自己带一条左侧栏；会话壳的侧栏在 macOS 上是一层半透明罩（--rail-veil，透出窗口
// 背后的桌面霜）。底下的壳如果照常画，两层罩叠在同一处就是一块近乎实色的方块；如果只是把罩
// 改透明，壳自己的会话标题就会从页面的导轨里透出来 —— 那是读者报上来的样子：一列会话标题和
// 「远端机器」叠在同一个像素上。
//
// 驱动量不到霜（WebKit 画的东西 getComputedStyle 看不见），但它量得到那条决定：整页在的时候
// app-body 是 `content-visibility: hidden`（不画、但仍挂载），回来之后会话标题还在、还可见
// （issue #27 的现场：隐藏这个祖先曾让 WebKit 永久丢掉带省略号的标题）。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const titlesBefore = smoke.allText('.session-title')
  if (titlesBefore.length === 0) return smoke.fail('侧栏有会话标题（回来时要对照）', '')

  // ── 进远端页 ─────────────────────────────────────────────────────────
  const entry = smoke.qa('.sidebar-actions .sidebar-search')
    .find((b) => b.getAttribute('aria-label') === '远端机器')
  if (!entry) return smoke.fail('侧栏搜索旁有远端机器入口', smoke.allText('.sidebar-actions'))
  smoke.click(entry)
  if (!(await smoke.waitFor('.remote-page', '远端机器页出现'))) return smoke.finish()

  // 壳仍然挂载（状态、滚动位置、草稿都靠它活着），但不再画。
  const shell = smoke.q('.app-body')
  smoke.check('整页盖住时会话壳仍然挂载', !!shell && !!smoke.q('.app-body .chat'), '')
  const paint = shell ? getComputedStyle(shell).contentVisibility : '(没有 app-body)'
  smoke.check('整页盖住时会话壳不画（content-visibility: hidden）', paint === 'hidden', paint)
  const vis = shell ? getComputedStyle(shell).visibility : '(没有 app-body)'
  smoke.check('…但不是 visibility: hidden 那种「藏起来」（那会丢标题，issue #27）',
    vis === 'visible', vis)

  // ── 回来：标题必须原样还在 ──────────────────────────────────────────
  smoke.click(smoke.q('.remote-back'))
  if (!(await smoke.waitGone('.remote-page', '回到会话'))) return smoke.finish()

  const paintBack = (() => {
    const el = smoke.q('.app-body')
    return el ? getComputedStyle(el).contentVisibility : '(没有 app-body)'
  })()
  smoke.check('回来后会话壳重新开始画', paintBack === 'visible' || paintBack === 'auto', paintBack)

  const titlesAfter = smoke.allText('.session-title')
  smoke.check('回来后侧栏会话标题还在（issue #27 的现场）',
    titlesAfter.length === titlesBefore.length && titlesAfter[0] === titlesBefore[0],
    titlesBefore.length + ' → ' + titlesAfter.length + ' · ' + titlesAfter.slice(0, 2).join(' | '))

  // 量一下标题自己的宽度：WebKit 丢掉重绘时，DOM 里的文字还在但盒子会塌成 0 —— 这正是那条
  // 历史 bug 的形状，所以宽高各断言一次。
  const firstTitle = smoke.qa('.session-title')[0]
  const box = firstTitle ? firstTitle.getBoundingClientRect() : { width: 0, height: 0 }
  smoke.check('标题真的画出了宽度（不是塌成一条线）',
    box.width > 20 && box.height > 0,
    box.width.toFixed(0) + '×' + box.height.toFixed(0))

  smoke.finish()
})()
