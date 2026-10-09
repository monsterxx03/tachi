// md-widgets — 正文里的富文本控件：代码块的头栏（语言 / 复制 / 折行）与图片的放大。
//
// 这三样都是"看得见、点得动"的东西，而驱动只能读 DOM、几何与计算样式 —— 所以每一条都钉一个读者
// 真能感知的事实，而不是"元素在"：
//   - 头栏在代码之上（它的底边不越过 pre 的顶边）：头栏替换掉的正是那个浮在代码第一行上的复制
//     按钮，改回浮动就会重新盖住它复制的那行字；
//   - 折行开关真的改变换行：长行在关着时横向溢出、开着时不溢出（`white-space` 只是手段，溢出才是
//     读者看到的事实）。计算样式要等它落帧再读，所以这里用 waitFor 轮询它；
//   - 图片是放大入口（点它开浮层），而取不到的那一张会说明自己取不到，不是留一片空白。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  smoke.type('.composer-input', '给我一段代码和两张图')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant .assistant-text', '再看两张图', 30000))) return smoke.finish()

  // ── 代码块的头栏 ───────────────────────────────────────────────────────────
  const head = await smoke.waitFor('.assistant-text .code-wrap .code-head', '代码块带头栏', 8000)
  if (!head) return smoke.finish()
  smoke.check('头栏写的是这块代码的语言', smoke.text(head).indexOf('go') >= 0, smoke.text(head))

  const pre = smoke.q('.assistant-text .code-wrap pre')
  if (!pre) return smoke.fail('代码块渲染出来', '找不到 .code-wrap pre')
  const rHead = head.getBoundingClientRect()
  const rPre = pre.getBoundingClientRect()
  smoke.check('头栏在代码之上，不压在代码上（它替掉的就是那个浮动按钮）',
    rHead.bottom <= rPre.top + 1,
    `头栏底边 ${Math.round(rHead.bottom)} / 代码顶边 ${Math.round(rPre.top)}`)

  const white = () => getComputedStyle(pre).whiteSpace
  const overflows = () => pre.scrollWidth > pre.clientWidth + 2
  smoke.check('默认不折行：长行横向溢出（这正是要折行的理由）',
    white() === 'pre' && overflows(),
    `white-space=${white()} scrollW=${pre.scrollWidth} clientW=${pre.clientWidth}`)

  const toggle = smoke.q('.code-head .code-tool')
  if (!toggle) return smoke.fail('头栏有折行开关', smoke.text(head))
  smoke.check('开关自己说得出状态（aria-pressed=false）', toggle.getAttribute('aria-pressed') === 'false',
    String(toggle.getAttribute('aria-pressed')))
  smoke.click(toggle)
  const folded = await smoke.waitFor(
    () => (white() === 'pre-wrap' && !overflows() ? true : null),
    '折行之后长行换行显示、不再横向滚动', 5000)
  smoke.check('点「折行」之后长行换行显示（不再横向滚动）', !!folded,
    `white-space=${white()} scrollW=${pre.scrollWidth} clientW=${pre.clientWidth}`)
  smoke.check('开关的状态跟着变（aria-pressed=true）',
    smoke.q('.code-head .code-tool').getAttribute('aria-pressed') === 'true',
    String(smoke.q('.code-head .code-tool').getAttribute('aria-pressed')))

  // 复制是头栏里那个按钮：它点下去要有反馈（反馈就是"真的按到了"的唯一现场证据 —— 驱动读不到
  // 剪贴板）。
  const copy = smoke.q('.code-head .code-copy')
  if (!copy) return smoke.fail('头栏有复制按钮', smoke.text(head))
  smoke.click(copy)
  const copied = await smoke.waitFor(
    () => (smoke.text('.assistant-text .code-head .code-copy').indexOf('已复制') >= 0 ? true : null),
    '复制按钮给出反馈', 3000)
  smoke.check('头栏里的复制按钮可用（点了给出反馈）', !!copied, smoke.text('.code-head'))

  // ── 正文图片 ───────────────────────────────────────────────────────────────
  const thumb = await smoke.waitFor('.assistant-text .md-img img', '正文里的图片渲染出来', 8000)
  if (!thumb) return smoke.finish()
  smoke.check('图片自己就是放大入口（不是一段静态的图）',
    smoke.q('.assistant-text .md-img').getAttribute('role') === 'button',
    smoke.q('.assistant-text .md-img').getAttribute('role') || '(没有 role)')
  smoke.click(smoke.q('.assistant-text .md-img'))
  const shown = await smoke.waitFor('.viewer-overlay .viewer-image', '点图片打开全屏预览', 5000)
  smoke.check('点正文里的图片打开浮层（复用附件那套查看器）', !!shown, '')
  if (!shown) return smoke.finish()
  smoke.key(document.body, 'Escape')
  if (!(await smoke.waitFor(() => !smoke.q('.viewer-overlay'), 'Esc 关掉浮层', 4000))) return smoke.finish()

  const broken = await smoke.waitFor('.assistant-text .md-img-failed', '取不到的那张图有说明', 6000)
  smoke.check('取不到的图片说明自己取不到（否则 alt 文字读起来像图注）',
    !!broken && smoke.text(broken).indexOf('图片无法加载') >= 0,
    broken ? smoke.text(broken) : '(没有 .md-img-failed)')

  // ── 正文里的长串不许冲出阅读列 ─────────────────────────────────────────────
  // 模型写下的东西没有"一定有空格"这回事：路径、URL、长标识符都能是一整个词。默认的换行规则会
  // 让它横着跑出阅读列（阅读列是 flex 的，跑出去就是被裁掉，读者看不到尾巴）。
  const prose = smoke.q('.msg-assistant .assistant-text')
  smoke.check('没有空格的长串被折断在阅读列里（不是横着跑出去）',
    !!prose && prose.scrollWidth <= prose.clientWidth + 1,
    prose ? `scrollW=${prose.scrollWidth} clientW=${prose.clientWidth}` : '(找不到 .assistant-text)')

  // ── 消息节奏与入场动画（要两轮才量得出来） ─────────────────────────────────
  smoke.type('.composer-input', '收到')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant', '收到。', 30000))) return smoke.finish()
  if (!(await smoke.waitFor(() => (smoke.qa('.chat-content > .msg').length === 4 ? true : null), '两轮四条消息', 8000))) return smoke.finish()

  const rows = smoke.qa('.chat-content > .msg')
  const box = (el) => el.getBoundingClientRect()
  const gapAfter = (i) => box(rows[i + 1]).top - box(rows[i]).bottom
  // 问题 → 它的回答：一段对话，挨得近。回答 → 下一轮的问题：两段阅读，离得远。两者不能是同一个值 ——
  // 那正是"这段回答属于谁"读不出来的原因。这里判的是关系（谁更近），不是那两个像素数。
  const within = gapAfter(0)
  const between = gapAfter(1)
  smoke.check('问题与它的回答挨得比两轮之间近（一段对话 vs 两段阅读）',
    within < between - 6, `问-答间距 ${Math.round(within)}px / 轮次间距 ${Math.round(between)}px`)

  // 入场动画只属于最新那条消息：换会话 / 翻历史 / 压缩后重载都是一次 mount 一屏消息，全都滑一遍
  // 就是闪成一片。早先的消息必须是静止的。
  const animOf = (el) => getComputedStyle(el).animationName
  const last = rows[rows.length - 1]
  smoke.check('最新那条消息才有入场动画', animOf(last) === 'msg-in', animOf(last))
  smoke.check('更早的消息不再重复滑入（重载/换会话不会整屏闪）',
    rows.slice(0, -1).every((r) => animOf(r) === 'none'),
    rows.slice(0, -1).map(animOf).join(','))

  // ── 回答尾部：一行，加一个 ⋯ 菜单 ─────────────────────────────────────────
  // 尾部原来是三四个平铺的入口（含两条写着评审范围的整句），现在收成一行 + 一个菜单；这一轮的耗时/
  // 迭代也搬进了菜单，不再占一行。这里钉的是"回答下面有几行"和"复制回答在不在这行上"。
  const bubbles = smoke.qa('.chat-content > .msg-assistant')
  const foot = bubbles[bubbles.length - 1] && bubbles[bubbles.length - 1].querySelector('.msg-footer')
  smoke.check('完成的回答下面只有一行操作栏', !!foot && !!foot.querySelector('.footer-btn'),
    foot ? smoke.text(foot) : '(没有 .msg-footer)')
  const copyBtn = foot && Array.from(foot.querySelectorAll('.footer-btn')).find((b) => smoke.text(b) === '复制回答')
  smoke.check('复制回答就在那一行上', !!copyBtn, foot ? smoke.text(foot) : '')
  if (copyBtn) {
    smoke.click(copyBtn)
    const copiedReply = await smoke.waitFor(
      () => (smoke.text(copyBtn) === '已复制' ? true : null), '复制回答给出反馈', 3000)
    smoke.check('点「复制回答」给出反馈', !!copiedReply, smoke.text(copyBtn))
  }
  const trigger = foot && foot.querySelector('.turn-menu-btn')
  smoke.check('尾部有 ⋯ 菜单入口，且没有把评审入口平铺在外面',
    !!trigger && !foot.querySelector('.diff-chip'),
    foot ? smoke.text(foot) : '(没有 .msg-footer)')
  const menuRows = await smoke.waitFor(
    () => (smoke.turnMenu(foot).length ? smoke.turnMenu(foot) : null), '⋯ 菜单打得开', 4000)
  smoke.check('这一轮的耗时/迭代在那个菜单里（不再占一行）',
    !!menuRows && smoke.text(foot).indexOf('本轮') >= 0,
    menuRows ? menuRows.map((r) => smoke.text(r)).join(' | ') : '(菜单没有行)')

  smoke.finish()
})()
