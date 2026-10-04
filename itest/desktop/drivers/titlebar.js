// titlebar — 窗口顶部的结构：侧栏通高、工具栏属于内容列、工具栏写着当前会话。
//
// 这三件事是一起改的，所以一起断言：侧栏不再被一条全宽标题栏压着，而是自己从窗口顶部
// 开始、把红绿灯顶在它的顶带上；工具栏于是成了 CONTENT 列的东西，中间写着这一段对话的名字。
//
// 让位（红绿灯那一份）在两个状态下各由一方出，且只出一份：展开时是侧栏顶部那条带（红绿灯压在
// 侧栏上，所以是竖着让），折叠时是工具栏的左内边距（内容列滑到了窗口最左，于是横着让）。两边都让
// 是白扔一条栏的高度，两边都不让是控件钻进红绿灯底下 —— 所以这里既断言折叠后让到位，也断言展开
// 后让回来。
//
// 「让多少」是每个平台自己的答案：macOS 是三盏灯（78px），没有灯的平台上窗口干脆没有边框
// （窗口的把手就是这条带本身），于是折叠时只留工具栏自己的内边距。驱动不问平台，只读 CSS 里
// 那个有心有姓的量（--titlebar-fold-pad，base.css 按平台给它值）—— 读变量而不是复述数字，
// 是这条断言在两个平台上都能成立的原因。
//
// 「交接动画的中间帧也不过界」不在这里：smoke 的窗口是被盖住的，而 WebKit 在被盖住的窗口里
// 不做过渡（实测点下后约 40ms 两个值就都在终点），中间帧根本取不到。那条靠的是「两侧同曲线、
// 同方向、都不回头」这个结构（见 layout.css 的 .titlebar），这里只断言那两个量声明了过渡。
//
// 不在这里的：标题的来源（agentEvents 的 session_title → sessions）和侧栏行的文案规则 ——
// 这里只钉「两个地方说的是同一个名字」。
;(async () => {
  if (!(await smoke.waitFor('.sidebar .session-title', '侧栏渲染出会话行', 15000))) return smoke.finish()
  if (!(await smoke.waitFor('.titlebar-title', '工具栏渲染出标题位', 8000))) return smoke.finish()

  const box = (sel) => {
    const el = smoke.q(sel)
    return el ? el.getBoundingClientRect() : null
  }
  const pad = (sel, side) => {
    const el = smoke.q(sel)
    return el ? parseFloat(getComputedStyle(el)['padding' + side]) : -1
  }
  // 平台由页面自己写在根元素上（frontend 的 platform.ts，值就是后端的 GOOS），读它而不是读
  // 引擎字符串：断言要跟样式表读的是同一个事实。
  const mac = document.documentElement.dataset.platform === 'darwin'
  // 「让多少位」是平台给的值（见文件头的说明）—— 读变量，别在这里复述数字。
  const foldPad = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--titlebar-fold-pad'))
  const side = box('.sidebar')
  if (!side) return smoke.finish()

  // ── 侧栏通高：红绿灯压在上面 ─────────────────────────────────────────────
  smoke.check('侧栏从窗口顶边开始（不再是「一条全宽标题栏下面的侧栏」）', Math.abs(side.top) <= 1,
    `top=${side.top.toFixed(1)}px`)

  // 让位的量就是红绿灯那一段，是 CSS 里一个有名有姓的数（--titlebar-h）；这里只断言
  // 「不低于一条栏高」，把这条带留着 —— 具体多高是设计的事。它是 .window-strip 这个元素
  // 本身（不是侧栏的 padding）：没有灯的平台上它还得当窗口的把手，而拖拽区必须是真的元素。
  const stripEl = smoke.q('.sidebar .window-strip')
  const strip = stripEl ? stripEl.getBoundingClientRect().height : -1
  smoke.check('侧栏顶部留着一条红绿灯的带（≥ 一条栏高）', strip >= 44,
    `strip=${strip}px`)

  // 那条带在没有红绿灯的平台上就是窗口的把手 —— 无边框窗口没有别的地方可拖（这条断言钉的是
  // 机制：--wails-draggable 认的是「事件落到哪个元素上」，所以把手必须是一个真元素）。
  if (!mac) {
    const drag = stripEl ? getComputedStyle(stripEl).getPropertyValue('--wails-draggable').trim() : ''
    smoke.check('无边框平台上这条带就是窗口的把手（--wails-draggable: drag）', drag === 'drag',
      `--wails-draggable=${drag}`)
  }

  // ── 工具栏属于内容列 ─────────────────────────────────────────────────────
  const bar = box('.titlebar')
  smoke.check('工具栏在内容列里（不是横跨整窗的一条）',
    !!smoke.q('.main > .titlebar') && bar && bar.left >= side.right - 1,
    `工具栏 left=${bar ? bar.left.toFixed(1) : '?'} · 侧栏右边=${side.right.toFixed(1)}`)

  // ── 两处说的是同一个名字 ─────────────────────────────────────────────────
  const rowTitle = smoke.text(smoke.q('.sidebar .session.active .session-title'))
  const barTitle = smoke.text(smoke.q('.titlebar-title'))
  smoke.check('工具栏写着当前会话的名字，和侧栏行同一份文案',
    rowTitle !== '' && barTitle === rowTitle, `工具栏「${barTitle}」/ 侧栏「${rowTitle}」`)

  // ── 折叠：让位的责任交给工具栏，且全程不过界 ─────────────────────────────
  if (!smoke.click('.sidebar-toggle')) return smoke.finish()
  const folded = await smoke.waitFor(() => (box('.sidebar') && Math.round(box('.sidebar').width) === 0 ? true : null),
    '侧栏折起', 4000)
  if (!folded) return smoke.finish()

  const barPadFolded = pad('.titlebar', 'Left')
  const firstCtrl = box('.titlebar .sidebar-toggle')
  smoke.check('折叠后工具栏自己让出窗口控件的位置（让位的量就是 --titlebar-fold-pad）',
    Math.abs(barPadFolded - foldPad) <= 0.5 && firstCtrl && firstCtrl.left >= foldPad - 0.5,
    `padding-left=${barPadFolded}px（--titlebar-fold-pad=${foldPad}px）· 第一个控件 left=${firstCtrl ? firstCtrl.left.toFixed(1) : '?'}`)

  // 让位是跟着侧栏的宽度一起动的（同一个 0.22s 曲线），所以交接的中间帧也不会让控件从灯下面过：
  // 控件左边 = 侧栏宽度 + 工具栏左内边距，两个量同曲线、同方向、都不回头，和只增不减。
  //
  // 这条只在纸上成立。smoke 的窗口是被盖住的，而 WebKit 对这种窗口会把过渡时长压成 ~0
  // （实测：这里读到的是 `0.00001s`，点下折叠/展开后约 40ms 两个值就都在终点），中间帧根本
  // 取不到。所以下面只断言两端（折叠时让到 78、展开时还回去）加「声明了过渡」这条结构断言；
  // 取样中间帧是假精确，不写。
  smoke.check('让位是过渡的（与侧栏宽度同一条曲线，不是瞬跳）',
    getComputedStyle(smoke.q('.titlebar')).transitionProperty.indexOf('padding-left') >= 0,
    getComputedStyle(smoke.q('.titlebar')).transition)

  smoke.click('.sidebar-toggle')
  const opened = await smoke.waitFor(() => (box('.sidebar').width > 100 ? true : null), '侧栏展开', 4000)
  if (!opened) return smoke.finish()
  const barPadOpen = pad('.titlebar', 'Left')
  // 展开时那一位还给侧栏。没有红绿灯的平台上没有要让的位，工具栏本来就只是自己的内边距 ——
  // 两种情况下它都等于「展开时该有的内边距」，而折叠时的那个量才是平台给的值。
  smoke.check(mac ? '展开后工具栏把位置还给侧栏（两边不会都让着）' : '展开后工具栏仍是它自己的左内边距（这个平台没有要让的位）',
    mac ? barPadOpen < foldPad : Math.abs(barPadOpen - foldPad) <= 0.5,
    `padding-left=${barPadOpen}px（侧栏已展开 ${Math.round(box('.sidebar').width)}px）`)

  // ── 设置页：也是通高的一页，红绿灯还是压在左栏上 ─────────────────────────
  if (!smoke.click('.sidebar-footer .footer-item')) return smoke.finish()
  if (!(await smoke.waitFor('.settings-view', '设置页出现', 5000))) return smoke.finish()

  const sv = box('.settings-view')
  smoke.check('设置页盖住整个窗口（上面不留一条对话区自己的工具栏）',
    sv && Math.abs(sv.top) <= 1 && sv.height >= window.innerHeight - 1,
    sv ? `top=${sv.top.toFixed(1)} height=${sv.height.toFixed(0)} / 窗口 ${window.innerHeight}` : '(没有设置页)')

  const back = box('.settings-rail .settings-back')
  const railStrip = box('.settings-rail .window-strip')
  // 让开的量就是那条带的高度（两个平台同高：它同时是各列首行的对齐高度）。
  smoke.check('设置左栏第一条控件在窗口控件那条带下面（返回还在，位置让开了）',
    back && railStrip && back.top >= railStrip.bottom - 1,
    back ? `返回 left=${back.left.toFixed(1)} top=${back.top.toFixed(1)}${railStrip ? ` · 带高=${railStrip.height.toFixed(0)}` : ' · 没有那条带'}` : '(没有返回按钮)')

  smoke.finish()
})()
