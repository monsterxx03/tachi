// fork — 在一条 user 消息上右键「从这里分支」：把这一轮**之前**的对话拷成一个新会话，工作区、检查点
// 与原会话一律不动。
//
// 与 rewind 是同一条切线上的两个方向，所以 fixture 也是那种形状，只是要证明的东西相反：
//   * 分支里**只有**第一轮的对话（第二轮的回答不在），而它落在新会话上（`data-session-id` 换了、
//     标题行带着「· 分支」）；
//   * 原会话**一点没动**：切回去，第二轮还在。
//   * 工作区也没动 —— 那一半界面上看不见，由 Go 侧查（第一轮用 bash 写的文件仍在）。
//
// 切口是「第二轮的开头」，所以这一轮的提示词回到输入框里（与回退同一个动作：改一改再问一次）。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const sessionId = () => smoke.q('.app').getAttribute('data-session-id')
  const bubbles = () => smoke.qa('.msg-user')
  const assistantText = () => smoke.allText('.assistant-text').join(' | ')
  const sidebarTitles = () => smoke.allText('.session-title')
  const titlebar = () => smoke.text('.titlebar-title')
  const inputValue = () => (smoke.q('.composer-input') || {}).value || ''

  // 一轮：发送，然后等它**结算**（等 stop 按钮出现是个陷阱 —— 脚本化的 mock 比下一次轮询还快，
  // 一轮可以开始了又结束而没被看见过「在跑」；而两次提示词之间必须有这个间隔，否则第二条会作为
  // 插话排进第一轮）。
  const runTurn = async (text, expect) => {
    smoke.type('.composer-input', text)
    smoke.click('.send-btn')
    if (!(await smoke.waitText('.msg-assistant', expect, 30000))) return false
    await smoke.waitFor(() => !smoke.q('.stop-btn'), '这一轮结算', 10000)
    await smoke.sleep(300)
    return true
  }

  if (!(await runTurn('用 bash 写文件', '写好了。'))) return smoke.finish()
  if (!(await runTurn('第二轮', '第二轮回复。'))) return smoke.finish()
  const src = sessionId()
  smoke.check('起手：两次对话都在屏幕上', bubbles().length === 2, String(bubbles().length))

  // ── 右键第二条消息：这一轮的边界就是分叉点 ────────────────────────────────
  const second = bubbles()[1]
  second.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 80, clientY: 260 }))
  if (!(await smoke.waitFor('.ctx-menu', '气泡菜单出现', 5000))) return smoke.finish()
  const items = () => smoke.qa('.ctx-menu .ctx-item')
  smoke.check('气泡菜单里「回退到这里」与「从这里分支」并列',
    items().some((b) => b.textContent.indexOf('回退到这里') >= 0) &&
    items().some((b) => b.textContent.indexOf('从这里分支') >= 0),
    smoke.allText('.ctx-menu .ctx-item').join(' | '))
  const forkItem = items().find((b) => b.textContent.indexOf('从这里分支') >= 0)
  smoke.check('这一轮有检查点，所以分支项可用', !!forkItem && !forkItem.disabled,
    forkItem ? (forkItem.title || '可点') : '找不到')
  if (!forkItem || forkItem.disabled) return smoke.finish()
  forkItem.click()

  // ── 落在新会话里，历史截止到那一轮之前 ────────────────────────────────────
  const switched = await smoke.waitFor(() => (sessionId() !== src ? true : null), '切到新分支的会话', 15000)
  smoke.check('分支创建之后落在新会话上（不是原地）', !!switched, sessionId() + ' vs ' + src)
  const listed = await smoke.waitFor(() => (sidebarTitles().some((t) => t.indexOf('· 分支') >= 0) ? true : null),
    '侧栏出现分支那一行', 8000)
  smoke.check('侧栏多出一行，标题带着「· 分支」（同一个工作区里两条同名的行分不开）',
    !!listed, sidebarTitles().join(' | '))
  smoke.check('工具栏标题也换成了分支的', titlebar().indexOf('· 分支') >= 0, titlebar())
  const shown = await smoke.waitFor(() => (assistantText().indexOf('写好了。') >= 0 ? true : null),
    '分支里能看到第一轮的回答', 8000)
  smoke.check('分支里带着分叉点之前的那一轮', !!shown, assistantText())
  smoke.check('分支里没有分叉点那一轮的回答（切口就在它之前）',
    assistantText().indexOf('第二轮回复。') < 0, assistantText())
  smoke.check('分支里只有一个用户气泡（第二次提问不在里面）', bubbles().length === 1, String(bubbles().length))
  smoke.check('这一轮的提示词回到输入框（改一改再问一次）', inputValue() === '第二轮', inputValue())
  smoke.check('分支是空的新对话：没有在跑的一轮', !smoke.q('.stop-btn'), 'stop=' + !!smoke.q('.stop-btn'))

  // ── 原会话一点没动 ────────────────────────────────────────────────────────
  const back = sidebarTitles().indexOf('冒烟会话')
  smoke.check('分支这一行与原会话那一行并存（原会话没有被搬走）', back >= 0, sidebarTitles().join(' | '))
  const srcRow = smoke.qa('.session').find((s) => smoke.text(s.querySelector('.session-title')) === '冒烟会话')
  if (!srcRow) return smoke.fail('找到原会话那一行', sidebarTitles().join(' | '))
  smoke.click(srcRow)
  const returned = await smoke.waitFor(() => (sessionId() === src ? true : null), '切回原会话', 15000)
  smoke.check('切回原会话', !!returned, sessionId())
  smoke.check('原会话的对话完整（第二轮还在）', assistantText().indexOf('第二轮回复。') >= 0, assistantText())
  smoke.check('原会话仍是两个用户气泡', bubbles().length === 2, String(bubbles().length))

  smoke.finish()
})()
