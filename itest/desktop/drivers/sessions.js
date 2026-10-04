// sessions — per-session numbers, the live sidebar title, and where the caret goes.
//
// Two of the three leaks in docs/agents/desktop.md live here: a brand-new session must not inherit the
// previous session's cache ring, and the row must pick up the generated title when the
// session_title event arrives (without it the row says 未命名会话 forever). And creating a
// session is the one moment the composer takes focus by itself — the click leaves it on the
// button, so a new session has to start with the user able to type.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // 第一条消息带 @-file 引用：模型收到的是展开后的文件内容，但**转写里显示的**必须是用户打的
  // 原文 —— 切回旧会话时（最后那段）会从磁盘重建转写，那里钉住这件事。
  smoke.type('.composer-input', '@README.md 里的说明')
  // 打了 "@" 会弹出选择器，而 Enter 会被它拿去"接受补全"而不是发送；Esc 先把它关掉，
  // 这样提交的只有发送按钮（真实用户也是这么做的：先 Esc，或直接点发送）。
  smoke.key('.composer-input', 'Escape')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('svg.ctx-ring[aria-label^="缓存命中率"]', '老会话出现用量环', 30000))) {
    // The numbers live in the status row, so dump it: "the ring never appeared" is only
    // half the story (empty row? wrong ring? no reply at all?). This line is what found
    // the turn-end usage gap.
    smoke.log('诊断：用量行 / 环 / 助手气泡', [
      JSON.stringify(smoke.text('.usage-meta')),
      smoke.qa('.ctx-ring').map((e) => e.getAttribute('aria-label') || 'aria-hidden').join('|') || '(无)',
      smoke.qa('.msg-assistant').length,
    ].join(' · '))
    return smoke.finish()
  }
  smoke.check('老会话有缓存命中率', true, smoke.text('.usage-meta'))
  const oldTitle = smoke.text('.session.active .session-title')
  smoke.check('侧边栏标题已生成', oldTitle !== '' && oldTitle !== '未命名会话', oldTitle)

  // Hand the caret to the transcript before creating the session: the assertion below only
  // proves anything if the composer was NOT focused beforehand.
  smoke.q('.chat').focus()

  if (!smoke.click('.new-chat')) return smoke.fail('点击新建会话', '按钮找不到')
  if (!(await smoke.waitFor(() => smoke.text('.session.active .session-title') !== oldTitle, '切到新会话', 10000))) return smoke.finish()
  // Focus was moved off the composer above, so this says the app did it, not that it was
  // already there. No waitFor: the composer is focused before the sidebar is refreshed, so
  // by the time the title changed the caret is settled — and a waitFor that failed here
  // would only add a second, redundant failure line.
  const active = document.activeElement
  smoke.check('新建会话后光标自动落在输入框', active === smoke.q('.composer-input'),
    'activeElement=' + (active ? (active.className || active.tagName) : '(无)'))
  const ringGone = await smoke.waitFor(() => !smoke.q('svg.ctx-ring[aria-label^="缓存命中率"]'), '新会话的用量环清空', 8000)
  smoke.check('新会话不继承上一个会话的缓存环', !!ringGone, smoke.text('.usage-meta'))
  smoke.check('新会话状态行是空的', smoke.text('.usage-meta') === '', JSON.stringify(smoke.text('.usage-meta')))

  // Sending in the new session gives it its own numbers…
  smoke.type('.composer-input', '新会话第一条')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('svg.ctx-ring[aria-label^="缓存命中率"]', '新会话自己的用量环', 30000))) return smoke.finish()
  smoke.check('新会话跑完拿到自己的用量', true, smoke.text('.usage-meta'))

  // …and switching back restores the old session's.
  const back = smoke.qa('.session').filter((s) => smoke.text(s.querySelector('.session-title')) === oldTitle)
  if (!back.length) return smoke.fail('旧会话还能在侧边栏找到', oldTitle)
  smoke.click(back[0])
  if (!(await smoke.waitFor(() => smoke.text('.session.active .session-title') === oldTitle, '切回旧会话', 10000))) return smoke.finish()
  const restored = await smoke.waitFor('svg.ctx-ring[aria-label^="缓存命中率"]', '切回后用量环回来', 10000)
  smoke.check('切回旧会话用量数字回来', !!restored, restored ? restored.getAttribute('aria-label') : '')

  // 这一轮的 @-file 留在记录里的两份文本由 Go 侧断言（读 messages.jsonl）；"从磁盘渲染出来的
  // 气泡长什么样"是 at-file-reload 的事 —— 在这里切会话用的是内存副本，断言不到重建路径。

  // The sidebar row's right-click menu. The 打开会话目录 item is asserted but NOT clicked: it
  // launches the real Finder, which would take the foreground and suspend this webview
  // mid-run (the suspension trap — see docs/agents/desktop.md). Which path it hands over is
  // pinned in Go instead (TestOpenSessionDirOpensTheSessionDirectory), so this half only has
  // to prove the row really offers it.
  const row = smoke.q('.session.active')
  row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 60, clientY: 200 }))
  const menu = await smoke.waitFor('.ctx-menu', '会话行的右键菜单出现', 3000)
  const items = smoke.allText('.ctx-item')
  smoke.check('右键菜单列出「打开会话目录」', !!menu && items.indexOf('打开会话目录') >= 0, items.join(' / '))
  // Dismiss it the way a reader does — the menu's own rule, and the DOM artifact then shows
  // the page rather than a menu frozen over it.
  if (menu) menu.dispatchEvent(new MouseEvent('mouseout', { bubbles: true, relatedTarget: document.body }))
  const closed = await smoke.waitFor(() => !smoke.q('.ctx-menu'), '右键菜单失焦关闭', 2000)
  smoke.check('右键菜单移开后关闭', !!closed, closed ? '' : smoke.text('.ctx-menu'))

  // ── 置顶：排到本组最前 ──────────────────────────────────────────────────
  // 「右键 → 置顶 → 这一行升到最前」是一个完整的因果断言，只有在排序真的按 pinned 走时才成立：
  // ListSessions 本身是 newest-first，而这些行全在「无项目」组里，老会话本来排在第二行。菜单里
  // 多一项不算数，行的位置变了才算。取消置顶同样要验 —— 只能置顶不能取消比没有更糟。
  const rowTitles = () => smoke.qa('.session-list .session').map((r) => smoke.text(r.querySelector('.session-title')))
  const findRow = (title) => smoke.qa('.session-list .session').find((r) => smoke.text(r.querySelector('.session-title')) === title)
  // menuAction 打开某一行的右键菜单并点掉指定文字的那一项。任一半缺失都返回 false，调用方报告
  // 它要的那两个文字，而不是报一个选择器。
  const menuAction = async (el, label) => {
    el.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 60, clientY: 200 }))
    if (!(await smoke.waitFor('.ctx-menu', '右键菜单（' + label + '）', 3000))) return false
    const item = smoke.qa('.ctx-item').find((b) => smoke.text(b) === label)
    if (!item) return false
    return smoke.click(item)
  }
  // 会话菜单里有一项「复制 session id」—— 标题栏不再显示 id 之后，这是拿到它的唯一入口（原来那
  // 个"点一下复制"在标题栏上）。这里要把**剪贴板真读回来**：第一版只钉了"菜单里有这一项、点下去
  // 菜单关掉"，而那一项当时调的是 `navigator.clipboard?.writeText(...)` —— 这一页不是 secure
  // context，那个调用读起来像空操作，剪贴板一直是空的，测试却是绿的。
  //
  // 读回来的路走 Wails 运行时自己的 clipboard 对象（`POST /wails/runtime`，object 1 /
  // method 0=SetText、1=Text），也就是 copyText 的首选路径 —— 它是唯一不依赖 webview 权限的那条，
  // 顺便也证明它在这个 app 里真的通。跑完把读者原来的剪贴板放回去。
  const clip = (method, args) => fetch('/wails/runtime', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'x-wails-client-id': 'smoke-clipboard' },
    body: JSON.stringify(args === undefined ? { object: 1, method } : { object: 1, method, args }),
  }).then((r) => r.text())
  const clipText = () => clip(1).catch(() => null)
  const clipSet = (t) => clip(0, { text: t }).catch(() => null)

  const idTarget = findRow(oldTitle)
  if (!idTarget) return smoke.fail('找到要右键的那一行', rowTitles().join(' | '))
  // 期望的 id 从 app 根上读（#41 之后它在那儿），也就是正要右键的那一行所属的会话 —— 此刻它就是
  // 活动会话（上面刚切回来）。
  const appRoot = smoke.q('.app')
  const wantId = (appRoot && appRoot.getAttribute('data-session-id')) || ''
  smoke.check('拿到期望的 session id（挂在 app 根上的 data-session-id）', wantId !== '', wantId)

  const beforeClip = await clipText()
  if (!(await menuAction(idTarget, '复制 session id'))) {
    return smoke.fail('菜单里有「复制 session id」', smoke.allText('.ctx-item').join(' / '))
  }
  smoke.check('点「复制 session id」后菜单关闭（那一项属于会话菜单）', !smoke.q('.ctx-menu'), '')
  // 写是异步的（走 bridge），所以轮询而不是等一个固定时长；`waitFor` 在这儿用不了 —— 它的判据是
  // 同步的真值，而"读剪贴板"本身就是一个 promise。
  let copied = ''
  for (let i = 0; i < 20 && copied !== wantId; i++) {
    await smoke.sleep(150)
    copied = (await clipText()) || ''
  }
  smoke.check('点「复制 session id」真的把 id 写进了系统剪贴板', copied === wantId,
    `剪贴板=${JSON.stringify(copied)} 期望=${JSON.stringify(wantId)}（之前=${JSON.stringify(beforeClip)}）`)
  if (beforeClip !== null) await clipSet(beforeClip)

  const beforePin = rowTitles()
  if (beforePin[0] === oldTitle) return smoke.fail('置顶前老会话不在第一行', beforePin.join(' | '))
  smoke.check('置顶前老会话在第二行（列表是 newest-first）', beforePin.indexOf(oldTitle) > 0, beforePin.join(' | '))

  const pinTarget = findRow(oldTitle)
  if (!pinTarget) return smoke.fail('找到要置顶的那一行', beforePin.join(' | '))
  if (!(await menuAction(pinTarget, '置顶'))) return smoke.fail('点得到「置顶」', smoke.allText('.ctx-item').join(' / '))
  const afterPin = await smoke.waitFor(
    () => (rowTitles()[0] === oldTitle ? rowTitles() : null), '置顶的会话排到最前', 5000)
  smoke.check('置顶后老会话升到第一行', !!afterPin, (afterPin || rowTitles()).join(' | '))
  const pinnedRow = smoke.qa('.session-list .session')[0]
  smoke.check('置顶的行有置顶标记', !!pinnedRow && !!pinnedRow.querySelector('.session-pin'),
    pinnedRow ? pinnedRow.innerHTML.slice(0, 80) : '(没有行)')

  if (!(await menuAction(pinnedRow, '取消置顶'))) return smoke.fail('点得到「取消置顶」', smoke.allText('.ctx-item').join(' / '))
  const afterUnpin = await smoke.waitFor(
    () => (rowTitles()[0] !== oldTitle ? rowTitles() : null), '取消置顶后回到原位', 5000)
  smoke.check('取消置顶后老会话回到第二行',
    !!afterUnpin && afterUnpin[1] === oldTitle, (afterUnpin || rowTitles()).join(' | '))
  smoke.check('取消置顶后置顶标记消失', !smoke.q('.session-pin'), smoke.text('.session-list'))

  // 再置顶一次，把「置顶落到了 meta.json」留给 Go 侧验证：界面上的顺序活不过重启，磁盘上的
  // pinned 才是下次启动时行序的来源。
  const repin = findRow(oldTitle)
  if (!repin || !(await menuAction(repin, '置顶'))) return smoke.fail('再次置顶', rowTitles().join(' | '))
  const repinned = await smoke.waitFor(
    () => (rowTitles()[0] === oldTitle ? rowTitles() : null), '再次置顶生效', 5000)
  smoke.check('再次置顶（留给 Go 侧验证落盘）', !!repinned, (repinned || rowTitles()).join(' | '))

  smoke.finish()
})()
