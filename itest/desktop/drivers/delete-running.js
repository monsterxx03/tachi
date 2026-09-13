// delete-running — 运行中的会话删不掉。
//
// 后端拒绝是权威的（desktop/agent_session.go 的 DeleteSession），前端做两件事：菜单里把删除项
// 禁掉，以及把后端的拒绝原因显示在确认框里。这里断言前一半（可观察、确定性的那一半），Go 侧
// 断言这一轮没有被多余地重启。
//
// 为什么是拒绝而不是「删除时顺手停掉」：跑着的回合有自己的 goroutine，它的会话写入
// （AppendMessage）和 run map 写入（setSessionState → getRun）都只认这个 id —— 在它底下删掉
// 目录，转录会静默丢失，还可能留下一个只有 meta.json 的重建目录。停不停是用户的决定。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '这一轮要跑久一点')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.stop-btn', '回合开始运行'))) return smoke.finish()
  // 菜单的禁用判断读的是前端的 runningSet，而侧边栏行上的运行圆点是它的来源
  // （后端 RunningSessions）。等它出现，这一半才真的在测「运行中」这个状态。
  if (!(await smoke.waitFor('.session.active .spin-dot', '侧边栏显示运行中', 10000))) return smoke.finish()
  smoke.check('运行中侧边栏有运行提示', true)

  // 打开当前会话行的右键菜单，删除项必须是禁用的，且点它不弹确认框。
  const openMenu = async (label) => {
    const row = smoke.q('.session.active')
    if (!row) {
      smoke.fail('找到当前会话行', '没有 .session.active')
      return null
    }
    row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 60, clientY: 200 }))
    return smoke.waitFor('.ctx-menu', label, 3000)
  }

  if (!(await openMenu('右键菜单出现'))) return smoke.finish()
  const del = smoke.qa('.ctx-item').find((b) => b.textContent.trim() === '删除')
  smoke.check('运行中时「删除」菜单项被禁用', !!del && del.disabled === true,
    del ? 'disabled=' + del.disabled : '菜单里没有删除项')
  if (del) del.click() // 禁用的 button 不会触发 onClick，确认框就不该出现
  const noBox = await smoke.waitFor(() => !smoke.q('.confirm-box'), '禁用状态下不弹确认框', 1500)
  smoke.check('运行中不会弹出删除确认框', !!noBox, noBox ? '' : smoke.text('.confirm-box'))

  // 关掉菜单（免得它盖住停止按钮），然后停掉这一轮。
  const menu = smoke.q('.ctx-menu')
  if (menu) menu.dispatchEvent(new MouseEvent('mouseout', { bubbles: true, relatedTarget: document.body }))

  const stop = smoke.q('.stop-btn')
  if (!stop) return smoke.fail('停止按钮还在', '找不到 .stop-btn')
  smoke.click(stop)
  if (!(await smoke.waitGone('.stop-btn', '回合停下来', 30000))) return smoke.finish()
  // 圆点消失 = 前端 runningSet 里已经没有它，拒绝不再是永久的。
  if (!(await smoke.waitGone('.session.active .spin-dot', '侧边栏运行提示消失', 8000))) return smoke.finish()
  smoke.check('停止后运行提示消失', true)

  // 再开一次菜单：这次删除项可用，点下去确认框出现。
  if (!(await openMenu('再次打开右键菜单'))) return smoke.finish()
  const del2 = smoke.qa('.ctx-item').find((b) => b.textContent.trim() === '删除')
  smoke.check('停止后「删除」菜单项可用', !!del2 && del2.disabled !== true,
    del2 ? 'disabled=' + del2.disabled : '菜单里没有删除项')
  if (del2) del2.click()
  if (!(await smoke.waitFor('.confirm-box', '停止后弹出了删除确认框', 3000))) return smoke.finish()
  smoke.check('停止后可以删除', true, smoke.text('.confirm-box').slice(0, 40))

  smoke.finish()
})()
