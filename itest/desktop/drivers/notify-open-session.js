// notify-open-session: 点一条通知要落到它说的那个会话。
//
// 一次**真实**的通知点击驱动不了：判据里有一半是焦点，投递由系统做，而这条套件跑的时候就坐在
// 用户的机器上。所以驱动的是那次点击落到页面上的那一半 —— Go 通过 `openSession` 发出
// `ui:open_session`（desktop/notify.go），而 Wails 把 Go 的事件交给页面用的就是这个全局调用
// （window._wails.dispatchWailsEvent），所以这里派发的事件和真货走的是同一条路。
//
// 断言的是「前端真的照做了」：切到了那个会话，并且把它从磁盘读了出来（另一条会话的转写此前
// 从没被显示过）；不是只把 data-session-id 挪一格。至于「点了通知才发这个事件」，那在
// desktop/notify_test.go 里（id → 会话的记忆），因为运行期没有通知服务可问。
;(async () => {
  if (!(await smoke.waitForSession())) return smoke.finish()

  const shown = () => {
    const el = smoke.q('.app')
    return (el && el.getAttribute('data-session-id')) || ''
  }
  const current = shown()

  // 另一条会话：按行自己携带的 data-sid 取，绝不按下标 —— 下标会在任何一次渲染变化之后指到
  // 别的行身上（而这里要的是一条窗口没在显示的会话）。
  const other = smoke
    .qa('.session')
    .map((row) => row.getAttribute('data-sid') || '')
    .filter((id) => id && id !== current)[0]
  if (!other) {
    smoke.check('侧栏里有第二条会话可供切换', false, '只有 ' + current)
    return smoke.finish()
  }
  if (!current) {
    smoke.check('窗口起始显示着一条会话', false, '（app 上没有 data-session-id）')
    return smoke.finish()
  }

  // 派发放进等待循环里，而不是只发一次：页面订阅 ui:open_session 的时刻晚于「会话已打开」，
  // 一条早到的消息会被丢掉 —— 而它丢得毫无痕迹（等 15 秒，然后失败）。真实的通知点击不会
  // 落在这个窗口里（人点不到一个还没起来的窗口），这里重发只是把那个竞态还原成一次送达。
  const switched = await smoke.waitFor(() => {
    window._wails.dispatchWailsEvent({ name: 'ui:open_session', data: other })
    return shown() === other
  }, '切到通知说的那个会话')
  if (!switched) return smoke.finish()

  // 真的读了它，而不是只换了 id：那条会话的记录此前从没被渲染过，所以它只能来自一次 LoadSession。
  const loaded = await smoke.waitFor(
    () => smoke.allText('.chat-content').join(' ').indexOf('另一个会话里的答案') >= 0,
    '目标会话的转写被读出来'
  )
  smoke.check('打开的是它说的那条对话，不只是把窗口带到前台', !!loaded, smoke.text('.chat-content').slice(0, 120))

  smoke.finish()
})()
