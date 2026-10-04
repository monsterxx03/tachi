// tray-new-session — 菜单栏（状态栏图标）的「新会话」。
//
// 菜单栏那一下本身 driver 点不到：图标是 Go 侧建的（desktop/tray.go 的 setupTray），不在 DOM 里。
// 所以这里补的是它的**另一半** —— 那条 `ui:new_session` 事件。事件从运行时自己的 Events 对象发出
// （object 3 / method 0 = Emit），和 Go 侧 `app.Event.Emit` 落到同一个地方，收到的前端该做的就是
// Go 侧指望它做的那件事：真的建一个会话，并把它带到眼前。
//
// 三件事是这条链的全部，也都是界面上看得见的：
//   · 会话真的建出来了，并成为当前会话（磁盘上那一半在 Go 侧数 meta.json）；
//   · 新会话是「像眼前这个一样」的 —— 起手的会话是项目成员，所以新行必须落在同一个项目组里
//     （⌘N 的规则，两个入口走的是同一个 newChatLikeCurrent）；
//   · 设置页让位。⌘N 在设置页上是**被屏蔽**的（那是一次误触按键），而菜单栏那一下不是：不换回
//     对话，用户在设置页上什么都看不见，等于没反应。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // 运行时自己的兜底调用：POST /wails/runtime。x-wails-client-id 是运行时用来区分客户端的头，
  // 而 args 就是方法自己的参数对象 —— Events.Emit(name, data) 的参数是那个 WailsEvent。
  const emit = (name) =>
    fetch('/wails/runtime', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'x-wails-client-id': 'smoke-tray' },
      body: JSON.stringify({ object: 3, method: 0, args: { name: name, data: null } }),
    })
      .then((r) => r.text())
      .catch(() => null)

  // 显示中的会话 id：标题栏不再写了之后，它是页面上唯一读得到的地方（挂在 app 根上）。
  const curID = () => (smoke.q('.app') ? smoke.q('.app').getAttribute('data-session-id') : '') || ''
  const projRows = () => smoke.qa('.proj-rows .session')

  // 起手：当前会话是 fixture 的「冒烟会话」——项目成员，项目组默认展开。
  const head = await smoke.waitFor('.proj-head', '侧栏出现项目组头', 10000)
  if (!head) return smoke.finish()
  const toggle = head.querySelector('.proj-toggle')
  if (!toggle) return smoke.fail('组头上有折叠开关', smoke.text(head))
  if (toggle.getAttribute('aria-expanded') !== 'true') toggle.click()
  if (!(await smoke.waitFor(() => (projRows().length === 1 ? true : null), '项目组里有一行（起手）', 6000)))
    return smoke.finish()
  const before = curID()
  smoke.check('起手：当前会话是项目里的那一行',
    !!before && projRows().some((r) => r.classList.contains('active')),
    'id=' + before + ' 行=' + projRows().map((r) => smoke.text(r.querySelector('.session-title'))).join(' | '))

  // 先把设置页打开（侧栏底部那个入口），菜单栏那一下必须把它换成对话 —— 见文件头第 3 条。
  const settingsBtn = smoke.qa('.footer-item').find((b) => smoke.text(b) === '设置')
  if (!settingsBtn) return smoke.fail('侧栏底部有「设置」入口', smoke.allText('.footer-item').join(' / '))
  smoke.click(settingsBtn)
  if (!(await smoke.waitFor('.settings-view', '设置页出现', 8000))) return smoke.finish()

  await emit('ui:new_session')

  // 1. 设置页让位，回到对话。
  const backToChat = await smoke.waitFor(
    () => (!smoke.q('.settings-view') && smoke.q('.composer-input') ? true : null),
    '设置页让位，回到对话', 10000)
  smoke.check('菜单栏的新会话把设置页换成了对话', !!backToChat,
    smoke.q('.settings-view') ? '设置页还在' : '')

  // 2. 真的多了一个会话，而且它就是当前会话。
  const fresh = await smoke.waitFor(
    () => (curID() && curID() !== before ? curID() : null), '当前会话换成新建的那个', 10000)
  smoke.check('新建的会话成为当前会话', !!fresh,
    '之前=' + JSON.stringify(before) + ' 现在=' + JSON.stringify(curID()))
  const grew = await smoke.waitFor(() => (projRows().length === 2 ? true : null), '项目组里多一行', 8000)
  smoke.check('新会话落在当前会话所属的项目组里', !!grew, projRows().length + ' 行')
  smoke.check('新会话那一行是 active（不是底下多了一行、选中还停在原地）',
    !!(grew && projRows().some((r) => r.classList.contains('active'))),
    smoke.qa('.proj-rows .session.active').length + ' 行 active')

  // 3. 它是一段空对话，且工作区还是那个项目 —— 「再来一个跟眼前这个一样的」的全部含义。
  const welcome = await smoke.waitFor('.welcome', '空对话的欢迎页', 6000)
  smoke.check('新会话是一段空对话', !!welcome, smoke.text('.transcript').slice(0, 80))
  const chip = await smoke.waitFor(
    () => (smoke.text('.work-dir').indexOf('smoke-tray') >= 0 ? true : null), '工作区 chip 还是那个项目', 8000)
  smoke.check('工作区还是当前会话所在的项目（没掉成无项目）', !!chip, smoke.text('.work-dir'))

  // 4. 光标在输入框里：侧栏的 ＋ 和 ⌘N 都把光标放到那儿，菜单栏那一次不该例外。
  const focused = await smoke.waitFor(
    () => (document.activeElement === smoke.q('.composer-input') ? true : null), '光标落在输入框', 5000)
  smoke.check('新会话建好之后可以直接开始输入', !!focused,
    document.activeElement ? String(document.activeElement.className) : '(没有焦点元素)')

  smoke.finish()
})()
