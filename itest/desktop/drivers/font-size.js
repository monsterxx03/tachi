// font-size — ⌘= / ⌘- / ⌘0 调的是「阅读字号」，不是整界面缩放。
//
// 三件事：键真的改消息区正文的大小；阶梯是乘在 `--fs-scale` 上的（正文变了，消息里自己 markdown
// 的标题按同一比例跟着走，而时间戳那类固定尺寸的铬一动不动）；值同时落进 localStorage（Go 侧
// after 读 desktop_ui.json，那是 durable 的那份）。
//
// 反过来的那一半 —— 原生菜单不再抢这三个键 —— driver 证明不了：它派发的是合成 KeyboardEvent，
// 直接进页面，根本不经过菜单栏。那条守卫是 desktop/menu_test.go（按 accelerator 断言）。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()
  await smoke.waitFor(() => !smoke.q('.chat-loading'), '会话加载完成', 10000)

  // 要量字号，得先有一段正文上屏。
  smoke.type('.composer-input', '字号测试')
  const send = await smoke.waitFor(
    () => { const b = smoke.q('.send-btn'); return b && !b.disabled ? b : null },
    '输入后发送按钮可用', 5000)
  if (!send) return smoke.finish()
  smoke.click(send)
  if (!(await smoke.waitText('.msg-assistant .assistant-text', '字号标尺', 30000))) return smoke.finish()

  const px = (sel) => {
    const el = smoke.q(sel)
    return el ? parseFloat(getComputedStyle(el).fontSize) : NaN
  }
  // 正文、它自己的 markdown 标题（em 跟着正文走）、以及同一块里的固定尺寸文字。
  const prose = () => px('.msg-assistant .assistant-text')
  const heading = () => px('.msg-assistant .assistant-text h2')
  const stamp = () => px('.msg-assistant .msg-ts')
  const apply = (key) => smoke.key(document.body, key, { metaKey: true })

  // 归零，再量起点。开屏那一帧读的是 localStorage，而它是**这个 webview** 的存储——不随场景的
  // HOME 沙箱隔离（bundle id 每次都一样），所以上一次运行留下的档位会出现在这里（实测过起点
  // 18.6px = 120%，于是后面每一条断言都在跟错的东西比）。
  //
  // 键是同步的，渲染不是：--fs-scale 的新值要到下一帧才出现在 getComputedStyle 里。不等这一帧
  // 就量 base，量到的是归零**之前**那一帧的字号，而 ⌘= 之后那句「正文变大」就永远等不到（它比的
  // 是同一个数）。所以等的是「页面已经画成样式表自己的大小」。
  apply('0')
  if (!(await smoke.waitFor(() => (Math.abs(prose() - 15.5) < 0.01 ? true : null), '⌘0 已经画到样式表自己的大小', 3000))) return smoke.finish()

  const base = { prose: prose(), heading: heading(), stamp: stamp() }
  smoke.log('起始', JSON.stringify(base))
  smoke.check('起始就是样式表自己的大小（--fs-scale: 1）', Math.abs(base.prose - 15.5) < 0.01, base.prose + 'px')
  smoke.check('正文里的标题按 em 跟着正文', Math.abs(base.heading / base.prose - 1.065) < 0.01,
    `h2 ${base.heading}px / 正文 ${base.prose}px`)
  // 这里钉的从来不是某个具体数字，而是「时间戳属于不跟着 --fs-prose 放大的那一类字」。所以它比
  // 的是样式表自己的刻度 token（--fs-micro），而不是写死的 10.5 —— 那个数字在字号刻度收敛时
  // 变成了一条假的失败，而读者看到的界面完全正确。
  const microPx = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--fs-micro'))
  smoke.check('时间戳这类铬取固定刻度（--fs-micro），不跟着 --fs-prose',
    Math.abs(base.stamp - microPx) < 0.01 && base.stamp < base.prose, `${base.stamp}px / 刻度 ${microPx}px`)

  // ⌘= 放大一档：正文 +10%，标题按同一比例，铬不动。
  apply('=')
  if (!(await smoke.waitFor(() => (prose() > base.prose ? true : null), '⌘= 之后正文变大', 3000))) return smoke.finish()
  const up = { prose: prose(), heading: heading(), stamp: stamp() }
  smoke.check('⌘= 放大一档（+10%）', Math.abs(up.prose / base.prose - 1.1) < 0.005,
    `${base.prose}px → ${up.prose}px`)
  smoke.check('标题与正文同一个比例（字号是乘出来的，不是各改各的）',
    Math.abs(up.heading / base.heading - up.prose / base.prose) < 0.01,
    `正文 ×${(up.prose / base.prose).toFixed(3)} / 标题 ×${(up.heading / base.heading).toFixed(3)}`)
  smoke.check('铬还是原来那个尺寸', up.stamp === base.stamp, `${base.stamp}px → ${up.stamp}px`)
  smoke.check('这一步同时写进了本 webview 的存储（开屏那帧读的就是它）',
    (() => { try { return localStorage.getItem('tachi.desktop.fontScale') === '110' } catch (e) { return false } })(),
    (() => { try { return String(localStorage.getItem('tachi.desktop.fontScale')) } catch (e) { return '(storage 不可用)' } })())

  // ⌘- 收回来：一步就是回到起点。
  apply('-')
  if (!(await smoke.waitFor(() => (prose() < up.prose ? true : null), '⌘- 之后正文变小', 3000))) return smoke.finish()
  smoke.check('⌘- 收一档（回到起点）', Math.abs(prose() - base.prose) < 0.01, `${up.prose}px → ${prose()}px`)

  // 一路按到底：钳在下界，再按也不动。
  for (let i = 0; i < 20; i++) apply('-')
  await smoke.waitFor(() => (prose() < base.prose ? true : null), '按下界', 3000)
  const floor = prose()
  smoke.check('收到底停在 80%（不是无限缩小）', Math.abs(floor / base.prose - 0.8) < 0.005,
    `正文 ${floor}px（起点 ×0.8 = ${(base.prose * 0.8).toFixed(2)}）`)
  apply('-')
  await smoke.sleep(50)
  smoke.check('已经到底之后再按也没变化', prose() === floor, `${floor}px → ${prose()}px`)

  // 留一个可辨认的终值给 Go 侧读文件：先用 ⌘0 回到样式表自己的大小，再上两档 = 120。
  apply('0')
  if (!(await smoke.waitFor(() => (Math.abs(prose() - base.prose) < 0.01 ? true : null), '⌘0 复位', 3000))) return smoke.finish()
  smoke.check('⌘0 回到样式表自己的大小', Math.abs(prose() - base.prose) < 0.01,
    `${floor}px → ${prose()}px（起点 ${base.prose}px）`)
  for (let i = 0; i < 2; i++) apply('=')
  const settled = await smoke.waitFor(() => (Math.abs(prose() / base.prose - 1.2) < 0.005 ? prose() : null),
    '终值 120%', 3000)
  smoke.check('两档之后停在 120%', !!settled, prose() + 'px')

  // 让最后一笔落盘走完再收场。apply 不等 Go（那是一次 fire-and-forget 的绑定调用），而场景
  // 一结束 app 就被杀——不等的话最后一次 ⌘= 只落在 webview 里，Go 侧读到的文件停在上一档
  // （实测过：界面 120%、文件 110%，只在套件满载时出现）。等的不是一个 UI 状态，而是一个
  // 已经在路上的副作用；界面此时已经对了，所以没有别的可等。
  await smoke.sleep(500)

  smoke.finish()
})()
