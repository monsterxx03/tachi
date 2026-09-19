// sidebar-width — 左侧会话列表的宽度：读者拖出来，而且被记得住。
//
// 三件事：拖右边缘真的改宽度；宽度被钳在"对话区至少 560px 或一半"这条线内（越界的拖拽不许把对话区
// 挤没）；拖完的宽度写进 desktop_ui.json（Go 侧读回文件断言）。
//
// 拖拽的合成方式照抄 oneoff-panel：pointerdown 打在 6px 的手柄上，pointermove/up 打在 **window**
// 上 —— 拖动监听在那儿（离开手柄也要继续拖）。手柄在侧栏内部靠右边缘，所以拖 x 就是拖宽度。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  const sidebarWidth = () => Math.round(smoke.q('.sidebar').getBoundingClientRect().width)
  const chatWidth = () => Math.round(smoke.q('.chat').getBoundingClientRect().width)
  const handle = smoke.q('.sidebar-resizer')
  if (!handle) return smoke.fail('侧栏有拖拽手柄', smoke.q('.sidebar') ? smoke.q('.sidebar').className : '(没有侧栏)')

  const box = handle.getBoundingClientRect()
  const at = { x: box.left + 3, y: box.top + 120 }
  const press = () => handle.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, cancelable: true, pointerId: 1, clientX: at.x, clientY: at.y }))
  const moveTo = (x) => window.dispatchEvent(new PointerEvent('pointermove', { bubbles: true, pointerId: 1, clientX: x, clientY: at.y }))
  const release = (x) => window.dispatchEvent(new PointerEvent('pointerup', { bubbles: true, pointerId: 1, clientX: x, clientY: at.y }))

  const initial = sidebarWidth()
  smoke.log('起始宽度', `侧栏 ${initial}px / 对话区 ${chatWidth()}px`)
  smoke.check('起始宽度是默认的 280px', initial === 280, initial + 'px')
  smoke.check('拖拽期间关掉过渡（否则宽度会落后于指针）',
    getComputedStyle(smoke.q('.sidebar')).transitionProperty.indexOf('width') >= 0, '')

  // 往右拖 90px：宽度跟着变（拖拽期间是直接写元素，不走 state）。
  press()
  smoke.check('按下手柄即进入拖拽态', document.body.classList.contains('is-resizing'),
    `body="${document.body.className}"`)
  moveTo(at.x + 90)
  const wider = sidebarWidth()
  smoke.check('往右拖 90px → 侧栏宽了 90px（拖动中就已经生效）', Math.abs(wider - (initial + 90)) <= 2,
    `${initial} → ${wider}`)
  release(at.x + 90)
  const settled = await smoke.waitFor(() => (sidebarWidth() === initial + 90 ? sidebarWidth() : null),
    '松手后宽度落到 state（并写盘）', 3000)
  smoke.check('松手后宽度稳定在拖到的值', !!settled, (settled || sidebarWidth()) + 'px')
  smoke.check('拖拽态在松手后清掉', !document.body.classList.contains('is-resizing'), document.body.className)

  // 再往右拖到荒谬的位置：钳在"对话区至少一半 / 560px"这条线内，对话区不许被挤没。
  const box2 = smoke.q('.sidebar-resizer').getBoundingClientRect()
  press()
  moveTo(box2.left + 3000)
  release(box2.left + 3000)
  const clamped = await smoke.waitFor(() => (sidebarWidth() > initial + 90 ? sidebarWidth() : null),
    '拖到极限后宽度被钳住', 3000)
  smoke.check('拖到极限时宽度被钳住', !!clamped, (clamped || sidebarWidth()) + 'px')
  const chat = chatWidth()
  smoke.check('对话区仍然留着至少 560px（660px 那次是被钳住的）',
    chat >= 560, `侧栏 ${sidebarWidth()}px / 对话区 ${chat}px`)
  smoke.log('钳住之后', `侧栏 ${sidebarWidth()}px / 对话区 ${chat}px`)
  smoke.check('钳住的值没有超过上限 600px', sidebarWidth() <= 600, sidebarWidth() + 'px')

  // ← → 是同一套 commit 的另一条入口（拖拽不是唯一的调宽方式）。
  const before = sidebarWidth()
  smoke.key('.sidebar-resizer', 'ArrowLeft')
  const narrowed = await smoke.waitFor(() => (sidebarWidth() < before ? sidebarWidth() : null), '← 收窄', 3000)
  smoke.check('← 也能收窄（拖拽不是唯一入口）', !!narrowed, `${before} → ${sidebarWidth()}`)

  smoke.finish()
})()
