// mcp-auth — 一个 OAuth2 保护的 HTTP MCP server:从「需要授权」到「已连接、工具有了」。
//
// 为什么会话之外还要有这个场景:这条链路没有任何一半在进程内可见 —— 面板要先把「需要授权」
// 显示出来(后端从连接失败里认出来),点授权之后真正的浏览器被 headless fetch 换掉
// (desktop/demo.go),DCR / PKCE / 回调 / 换 token 全在 agent/mcp 里跑,最后连接与工具入池才
// 回到面板。单测能钉住 Go 侧的每一步,却看不见「后端说了、前端没画」这一类错误 —— 而这正是
// 这个功能之前的形状:流程齐全,只是没人能触发它。
//
// Go 侧那一半断言真正的交换发生过(fixture 的注册/授权/token 记录)以及工具是否真的被
// 告知了模型(下一条请求里的 deferred tools 提示)。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  // ── 1. 面板:未授权的 server 要自己说出来 ───────────────────────────────
  // 启动时后台连接会撞上 401,后端据此把这一行标成「需要授权」。没有这个标记,用户看到的
  // 只是「未连接」——和一个连不上的 server 长得一模一样。
  if (!(await smoke.waitFor('.mcp-btn', '状态栏出现 MCP 按钮'))) return smoke.finish()
  smoke.click('.mcp-btn')
  if (!(await smoke.waitFor('.mcp-panel', 'MCP 面板打开'))) return smoke.finish()

  const row = await smoke.waitFor(
    () => {
      const hit = smoke.qa('.mcp-server').filter((s) => smoke.text(s).indexOf('oauth-fixture') >= 0)
      return hit.length && smoke.text(hit[0]).indexOf('需要授权') >= 0 ? hit[0] : null
    },
    'server 行标成「需要授权」',
    20000
  )
  if (!row) return smoke.finish()
  smoke.check('未连接时不谎报已连接', smoke.text(row).indexOf('已连接') < 0, smoke.text(row))
  smoke.check('行上说明为什么需要授权', smoke.text(row).indexOf('需要授权') >= 0, smoke.text(row))
  smoke.check('需要授权时给出「授权」按钮', !!row.querySelector('.mcp-auth-btn'), smoke.text(row))

  // 工具列表默认是折叠的（面板打开时每条 server 只占一行）。工具那一行只有展开之后才存在于
  // DOM 里，所以「现在看不到工具」是默认状态本身，不是错误 —— 这里把它钉住，否则以后有人把
  // 默认改回展开，也没有第二个断言会发现。
  const rowHead = row.querySelector('.mcp-server-row')
  smoke.check('默认折叠：server 行没有铺开工具', !row.querySelector('.mcp-tool'), '工具行一开始就在')
  smoke.check('折叠的行上有展开指示', !!rowHead.querySelector('.mcp-caret'), '找不到 .mcp-caret')

  // ── 2. 点授权:浏览器被 headless fetch 顶替 ─────────────────────────────
  smoke.click(row.querySelector('.mcp-auth-btn'))

  // 进度必须落在这一行上。授权是一段与用户的对话(打开浏览器、可能要粘回 URL),而面板唯一
  // 能说出「它走到哪一步」的地方就是这张卡。fixture 故意把授权端点拖慢几秒(见 runner 的
  // mcpConsentDelay):真实世界的授权大部分时间就停在这个状态上,而一个瞬间完成的流程
  // 只能被事后断言 —— 那就等于没有断言过这张卡。
  const card = await smoke.waitFor('.mcp-auth', '行下出现授权进度卡片', 15000)
  if (!card) return smoke.finish()

  const urlBox = await smoke.waitFor('.mcp-auth-url input', '卡片里出现授权 URL')
  if (!urlBox) return smoke.finish()
  smoke.check('授权 URL 是可打开的链接', String(urlBox.value || '').indexOf('http') === 0, urlBox.value)
  smoke.check('等待期间也给得出粘回 URL 的入口', !!smoke.q('.mcp-auth-paste-input'), '没有粘贴框')

  // ── 3. 换到 token 之后自己连上 ──────────────────────────────────────────
  // 授权成功不等于可用:流程结束后必须接上连接、工具入池,面板才会从「需要授权」变成
  // 「已连接」并列出工具。没接上就会永远停在这里 —— 那正是「授权成功但什么都没发生」。
  const connected = await smoke.waitFor(
    () => {
      const hit = smoke.qa('.mcp-server').filter((s) => smoke.text(s).indexOf('oauth-fixture') >= 0)
      return hit.length && smoke.text(hit[0]).indexOf('已连接') >= 0 ? hit[0] : null
    },
    '授权后 server 变成已连接',
    30000
  )
  if (!connected) return smoke.finish()

  smoke.check('已连接后不再显示需要授权', smoke.text(connected).indexOf('需要授权') < 0, smoke.text(connected))
  smoke.check('工具计数不为 0', smoke.text(connected).indexOf('0 工具') < 0, smoke.text(connected))

  // 工具列表要展开才看得到 —— 这正是默认折叠下的正常路径，driver 点一下行头即可。
  smoke.click(connected.querySelector('.mcp-server-row'))
  if (!(await smoke.waitFor('.mcp-tool-name', '展开后出现工具行'))) return smoke.finish()
  const tools = smoke.allText('.mcp-tool-name')
  smoke.check('列出来自 fixture 的工具', tools.some((t) => t.indexOf('echo') >= 0), tools.join(' | ') || '(没有工具行)')

  // ── 4. 授权之后的那一轮：模型要知道这些工具存在 ─────────────────────────
  // 工具进了池不等于模型知道它存在：deferred 提示一次会话只发一次，而这条 server 是会话
  // 开始之后才连上的 —— 不通知，模型就只能靠瞎猜去搜。这里真的发一条消息，让下一条请求
  // 把提示带出去；Go 侧断言它确实到了 LLM 边界（driver 看不到 prompt）。
  smoke.click('.mcp-close')
  if (!(await smoke.waitFor('.composer-input', '回到编辑器'))) return smoke.finish()
  smoke.type('.composer-input', '授权之后的第一轮')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant .msg-content', '收到。', 30000))) return smoke.finish()
  smoke.check('授权后的第一轮拿到了回复', true)

  smoke.finish()
})()
