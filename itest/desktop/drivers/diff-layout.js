// diff-layout — diff 的 上下 / 左右 两套布局（issue #34）。
//
// 左右那一栏不是「把同一批行换个盒子」：它有两条自己的规则，这个 driver 分别钉住。
//
//   1. 配对：替换块里删的行和加的行**一行对一行**（one 对 ONE）。多出来的那一侧留空格子 ——
//      删三行加一行，第 2/3 组右边必须是空的；少了这一条，两列会从这里开始错位，后面每一行都错。
//   2. 两列各自横向滚动，而且是**整列一起动**：一列就是那个文件的一列，拖一行时同列的其他行要跟着
//      走，否则「并排对照」只在鼠标那一行成立。种子里那行很长的上下文行就是为了让它溢出来。
//
// 行号也各报各的：左边是旧文件的行号，右边是新文件的。中间那条 ctx 两侧同号看不出来，所以看最后
// 一行（omega：旧 6 / 新 4）—— 顺手也证明了左右两列报的不是同一个文件的行号。
//
// 布局偏好记在 localStorage（见 diff.tsx），所以这个 driver 不假设初始状态：先点「上下」再点
// 「左右」，两个方向都断言，最后停回「上下」（默认态）。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '把 README 里那三行换成一行')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant', '改好了', 30000))) return smoke.finish()
  await smoke.waitFor(() => !smoke.q('.stop-btn'), '这一轮结算', 15000)
  await smoke.sleep(300)

  const chip = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('完整 diff') >= 0) || null,
    'footer 出现「完整 diff」入口', 8000)
  if (!chip) return smoke.finish()
  chip.click()
  if (!(await smoke.waitFor('.viewer-overlay .viewer-doc.is-diff', '「完整 diff」打开宽浮层', 8000))) return smoke.finish()
  if (!(await smoke.waitFor('.viewer-overlay .diff-body .diff-line', 'diff 行渲染出来', 8000))) return smoke.finish()

  const SCOPE = '.viewer-overlay '
  const bodies = () => smoke.qa(SCOPE + '.diff-body')
  const rows = () => smoke.qa(SCOPE + '.diff-split-row')
  const cellsIn = (r) => Array.prototype.slice.call(r.querySelectorAll('.diff-side'))
  const btn = (label) => smoke.qa(SCOPE + '.diff-layout-btn').find((b) => smoke.text(b) === label) || null
  const pressed = (label) => { const b = btn(label); return !!b && b.getAttribute('aria-pressed') === 'true' }
  // 每行都是「tag · 填充…」（见 scenarios.go 的 diffLayoutLine），断言只看 tag —— 填充是给横向
  // 滚动用的，写进断言里只会让失败信息变成一屏中文。
  const tag = (s) => s.split(' · ')[0]
  const cellText = (c) => { const t = c.querySelector('.diff-text'); return t ? tag(t.textContent) : '(空)' }
  const cellFull = (c) => { const t = c.querySelector('.diff-text'); return t ? t.textContent : '' }
  const cellNum = (c) => { const n = c.querySelector('.diff-ln'); return n ? n.textContent.trim() : '' }
  const shape = () => rows().map((r) => cellsIn(r).map(cellText))
  const nums = () => rows().map((r) => cellsIn(r).map(cellNum))
  const leftCells = () => smoke.qa(SCOPE + '.diff-side.is-left')
  const rightCells = () => smoke.qa(SCOPE + '.diff-side.is-right')
  const scrollable = (c) => c.scrollWidth > c.clientWidth

  // ── 开关本身 ─────────────────────────────────────────────────────────────
  // 两颗按钮，各自说出「当前布局是不是我」—— 一个会换标签的切换按钮做不到这一点（要读两次才确定）。
  smoke.check('diff 头上有上下 / 左右两颗按钮', !!btn('上下') && !!btn('左右'),
    smoke.allText(SCOPE + '.diff-layout-btn').join(' | ') || '(没有 .diff-layout-btn)')
  if (!btn('上下') || !btn('左右')) return smoke.finish()
  smoke.check('两颗按钮用 aria-pressed 各报自己的状态（只亮一个）',
    Number(pressed('上下')) + Number(pressed('左右')) === 1,
    `上下 pressed=${btn('上下').getAttribute('aria-pressed')} / 左右 pressed=${btn('左右').getAttribute('aria-pressed')}`)
  smoke.check('按钮上写着字，不是两个图标', smoke.text(btn('上下')).indexOf('上下') >= 0
    && smoke.text(btn('左右')).indexOf('左右') >= 0, '')

  // ── 上下 ─────────────────────────────────────────────────────────────────
  smoke.click(btn('上下'))
  if (!(await smoke.waitFor(() => (bodies().length > 0 && !smoke.q(SCOPE + '.diff-body.is-split') ? true : null),
    '切到上下布局', 4000))) return smoke.finish()
  smoke.check('点「上下」之后按住的也是它', pressed('上下') && !pressed('左右'), '')
  const uniLines = smoke.qa(SCOPE + '.diff-body .diff-line')
  smoke.check('上下：一行一条 .diff-line（7 条：2 ctx + 3 删 + 1 加 + 1 ctx）',
    uniLines.length === 7, String(uniLines.length) + ' 条')
  smoke.check('上下：行号列旧/新并排，所以有 14 个行号',
    smoke.qa(SCOPE + '.diff-body .diff-ln').length === 14,
    String(smoke.qa(SCOPE + '.diff-body .diff-ln').length) + ' 个')
  const uniSigns = smoke.qa(SCOPE + '.diff-body .diff-sign').map((s) => s.textContent.trim()).join('|')
  smoke.check('上下：正负号是 空|空|−|−|−|+|空（顺序没乱：先删后加）',
    uniSigns === '||−|−|−|+|', uniSigns)

  // ── 左右 ─────────────────────────────────────────────────────────────────
  smoke.click(btn('左右'))
  if (!(await smoke.waitFor(() => (rows().length > 0 ? true : null), '切到左右布局', 4000))) return smoke.finish()
  smoke.check('点「左右」之后按住的换成它', pressed('左右') && !pressed('上下'), '')
  smoke.check('左右：每个文件的身体都换成两列（.diff-body.is-split），每行一对格子',
    bodies().every((b) => b.classList.contains('is-split')) && rows().every((r) => cellsIn(r).length === 2),
    `body=${bodies().length} 行=${rows().length} 列数=${rows().map((r) => cellsIn(r).length).join(',')}`)

  const sh = shape()
  smoke.check('左右：六行 —— 2 ctx + 1 对删加 + 2 多出来的删 + 1 ctx',
    sh.length === 6, JSON.stringify(sh))
  smoke.check('左右：上下文行两侧都画（alpha / omega），文字一样',
    sh.length === 6 && sh[0][0] === 'alpha' && sh[0][1] === 'alpha'
      && sh[5][0] === 'omega' && sh[5][1] === 'omega', JSON.stringify([sh[0], sh[5]]))
  smoke.check('左右：替换块的删行和加行一行对一行（one ↔ ONE）',
    sh.length === 6 && sh[2][0] === 'one' && sh[2][1] === 'ONE', JSON.stringify(sh[2] || null))
  smoke.check('左右：多出来的删行对面是空格子（two / three 右边没有行）',
    sh.length === 6 && sh[3][0] === 'two' && sh[3][1] === '(空)' && sh[4][0] === 'three' && sh[4][1] === '(空)',
    JSON.stringify([sh[3] || null, sh[4] || null]))
  smoke.check('左右：长的那条上下文行两侧也一样（它就是同一行，不是被切了两份）',
    sh.length === 6 && cellFull(cellsIn(rows()[1])[0]) === cellFull(cellsIn(rows()[1])[1])
      && cellFull(cellsIn(rows()[1])[0]).length > 100,
    sh.length === 6 ? `左 ${cellFull(cellsIn(rows()[1])[0]).length} 字 / 右 ${cellFull(cellsIn(rows()[1])[1]).length} 字` : '')

  const ns = nums()
  smoke.check('左右：每格只有自己的那一个行号（旧在左、新在右，不再并排两个）',
    rows().every((r) => cellsIn(r).every((c) => c.querySelectorAll('.diff-ln').length === 1)),
    JSON.stringify(ns))
  smoke.check('左右：左边报旧文件的行号、右边报新文件的（omega 旧 6 / 新 4）',
    ns.length === 6 && ns[5][0] === '6' && ns[5][1] === '4', JSON.stringify(ns[5] || null))
  smoke.check('左右：空格子没有行号（没有行就没有号）',
    ns.length === 6 && ns[3][1] === '' && ns[4][1] === '', JSON.stringify([ns[3] || null, ns[4] || null]))

  // ── 浮层铺满整窗，横向只有一根条 ─────────────────────────────────────────
  // 面板的 1100/1390px 阅读上限在浮层里被摘掉（那是给侧栏那种窄列的），pane 铺满 stage；长行不再
  // 由每个文件的 body 各自滚，而是由 pane 底部那一根共享条统一驱动（DiffFindingsPane 的 hscroll）。
  const pane = smoke.q(SCOPE + '.viewer-doc.is-diff')
  const stage = smoke.q(SCOPE + '.viewer-stage')
  const bar = smoke.q(SCOPE + '.diff-hscroll')
  const stageInner = stage ? stage.clientWidth - parseFloat(getComputedStyle(stage).paddingLeft || 0)
    - parseFloat(getComputedStyle(stage).paddingRight || 0) : 0
  smoke.check('浮层里的 diff 没有阅读宽度上限（max-width: none，铺满 stage）',
    !!pane && getComputedStyle(pane).maxWidth === 'none', pane ? getComputedStyle(pane).maxWidth : '(没有 pane)')
  smoke.check('pane 的宽度就是 stage 的内容宽度（窗口多宽它就多宽）',
    !!pane && !!stage && Math.abs(pane.getBoundingClientRect().width - stageInner) <= 1,
    pane && stage ? `pane ${Math.round(pane.getBoundingClientRect().width)}px / stage ${Math.round(stageInner)}px` : '')
  smoke.check('底部是共享的那一根横向滚动条', !!bar, bar ? smoke.text(bar) : '(没有 .diff-hscroll)')
  smoke.check('长行不再自己滚：body / 格子都不带自己的横滚条（overflow-x: hidden）',
    smoke.qa(SCOPE + '.diff-body').every((b) => getComputedStyle(b).overflowX === 'hidden')
      && smoke.qa(SCOPE + '.diff-side').every((c) => getComputedStyle(c).overflowX === 'hidden'),
    smoke.qa(SCOPE + '.diff-body').map((b) => getComputedStyle(b).overflowX).join(','))

  // 条的行程 = 最宽的溢出：条的长度是有含义的（"还有多少没看到"），不是随便一根。
  const widest = () => smoke.qa(SCOPE + '.diff-body, ' + SCOPE + '.diff-side')
    .reduce((m, el) => Math.max(m, el.scrollWidth - el.clientWidth), 0)
  const barRange = () => (bar ? bar.scrollWidth - bar.clientWidth : -1)
  smoke.check('共享条的行程就是最宽那一行的溢出（不多不少）',
    Math.abs(barRange() - widest()) <= 1,
    `条 ${barRange()}px / 最宽溢出 ${widest()}px`)

  // 拖条 → 所有盒子跟着走。左右模式下是两列一起动，所以配对在整份文件上都不会错位。
  // （这条断言也是唯一能抓住"条没接上"的：`overflow: visible` 的盒子照样会被量出行程，只是
  // scrollLeft 永远回 0 —— 行程那条断言看不出来，这条能。）
  bar.scrollLeft = 200
  const barMovesAll = await smoke.waitFor(
    () => (smoke.qa(SCOPE + '.diff-side.is-left').every((c) => c.scrollLeft === 200)
      && smoke.qa(SCOPE + '.diff-side.is-right').filter(scrollable).every((c) => c.scrollLeft === 200) ? true : null),
    '拖共享条后两列都跟到 200', 3000)
  smoke.check('左右：拖底部那根条，两列一起横移（配对不会错位）', !!barMovesAll,
    `左 ${smoke.qa(SCOPE + '.diff-side.is-left').map((c) => c.scrollLeft).join(',')} / 右 ${smoke.qa(SCOPE + '.diff-side.is-right').map((c) => c.scrollLeft).join(',')}`)

  // ── 两列仍然各自跟着自己那一列（侧栏面板里唯一的横滚方式）───────────────
  // 浮层里由底部那根条统一驱动，但这份逻辑是面板唯一的横滚手段（那边没有共享条：窄列里没有余地），
  // 所以这里仍然钉住它：把一个格子推到 120，同一列的其他格子要跟着走。
  // 先探一下"程序设置 scrollLeft 会不会派发 scroll 事件"：mirrorScroll 全靠它，而这套引擎里
  // 不冒泡的事件（scroll 就是）行为各不相同（捕获阶段的监听在这里就什么都没收到）。
  const probe = leftCells()[0]
  let fired = 0
  const countScroll = () => { fired++ }
  probe.addEventListener('scroll', countScroll)
  probe.scrollLeft = 20
  await smoke.sleep(300)
  probe.removeEventListener('scroll', countScroll)
  smoke.check('程序设置 scrollLeft 会派发 scroll 事件（列同步就靠它）', fired > 0,
    `收到 ${fired} 次 scroll`)

  probe.scrollLeft = 120
  const syncedLeft = await smoke.waitFor(
    () => (leftCells().every((c) => c.scrollLeft === 120) ? true : null),
    '左边那一列每一格都跟着滚到 120', 3000)
  smoke.check('推一格横着滚，左边这一列一起走（一列是一个文件，不是一组各行其是的行）',
    !!syncedLeft, leftCells().map((c) => c.scrollLeft).join(','))

  rightCells()[1].scrollLeft = 60
  const syncedRight = await smoke.waitFor(
    () => (rightCells().filter(scrollable).every((c) => c.scrollLeft === 60) ? true : null),
    '右边那一列每一格都跟着滚到 60', 3000)
  smoke.check('反过来也一样：推右边，右边整列一起走', !!syncedRight,
    rightCells().map((c) => c.scrollLeft).join(','))
  smoke.check('左边仍然停在 120（两列是两个版本，各滚各的）',
    leftCells().every((c) => c.scrollLeft === 120), leftCells().map((c) => c.scrollLeft).join(','))

  // ── 偏好是记下来的 ───────────────────────────────────────────────────────
  // 下次打开还是这个布局：值写在 localStorage（theme 也在那儿；宽度那种要给 Go 看的几何才进
  // desktop_ui.json）。同步读，所以渲染前就知道该用哪套，不会先画上下再翻成左右。
  const stored = () => { try { return localStorage.getItem('tachi.desktop.diffLayout') } catch (e) { return '(读不到: ' + e.message + ')' } }
  smoke.check('切到左右后，偏好落盘（tachi.desktop.diffLayout=split）', stored() === 'split', String(stored()))

  smoke.click(btn('上下'))
  const backToUnified = await smoke.waitFor(
    () => (smoke.q(SCOPE + '.diff-body.is-split') ? null : true), '切回上下', 4000)
  smoke.check('切回上下：格子没了，行又合成一条 .diff-line',
    !!backToUnified && rows().length === 0
      && smoke.qa(SCOPE + '.diff-body .diff-line').length === 7,
    `${rows().length} 行对 / ${smoke.qa(SCOPE + '.diff-body .diff-line').length} 条行`)
  smoke.check('切回上下后偏好也跟着改成 unified', stored() === 'unified', String(stored()))

  smoke.finish()
})()
