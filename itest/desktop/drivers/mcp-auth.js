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
  if (!(await smoke.waitForSession())) return smoke.finish()

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
  // 行上的计数是「已加载/共」：默认折叠的一行只报总数的话，看不出这个 server 有多少工具真的
  // 进了本会话 —— 那正是展开后一格一格才能读到的信息。刚连上、一个都没加载，所以分子是 0。
  // 每次都重新按名字取行：面板的这一次重取会换掉节点，攥着旧引用读的是上一版的 DOM。
  const readCount = () => {
    const row = smoke.qa('.mcp-server').find((s) => smoke.text(s).indexOf('oauth-fixture') >= 0)
    const el = row ? row.querySelector('.mcp-toolcount') : null
    const text = el ? smoke.text(el) : ''
    const m = /^(\d+)\/(\d+) 工具$/.exec(text)
    return m ? { loaded: Number(m[1]), total: Number(m[2]), text: text } : null
  }
  const count0 = readCount()
  smoke.check('server 行给出「已加载/共」的工具数，未加载时为 0', !!count0 && count0.loaded === 0 && count0.total > 0, count0 ? count0.text : '找不到 .mcp-toolcount')
  // 标题下的合计是各行之和：后台另给一个数字就会有两个说法，而它们迟早会不一样。
  const readTotal = () => {
    const el = smoke.q('.mcp-total')
    const text = el ? smoke.text(el) : ''
    const m = /^已加载 (\d+)\/(\d+) 工具$/.exec(text)
    return m ? { loaded: Number(m[1]), total: Number(m[2]), text: text } : null
  }
  const head0 = readTotal()
  smoke.check('标题下给出合计，且与各行之和一致',
    !!head0 && !!count0 && head0.loaded === count0.loaded && head0.total === count0.total,
    (head0 ? head0.text : '找不到 .mcp-total') + ' / 行：' + (count0 ? count0.text : '(无)'))

  // 工具列表要展开才看得到 —— 这正是默认折叠下的正常路径，driver 点一下行头即可。
  smoke.click(connected.querySelector('.mcp-server-row'))
  if (!(await smoke.waitFor('.mcp-tool-name', '展开后出现工具行'))) return smoke.finish()
  const tools = smoke.allText('.mcp-tool-name')
  smoke.check('列出来自 fixture 的工具', tools.some((t) => t.indexOf('echo') >= 0), tools.join(' | ') || '(没有工具行)')

  // 池里的工具与「本会话已加载」是两件事：刚连上的 server 一个工具都没加载，但工具必须照常
  // 列出来（置灰），否则在开启工具搜索时每个 server 看起来都是空的。这一条钉住"列出来"，
  // 下面第 5 步钉住"能区分"。
  const toolRow = smoke.q('.mcp-tool')
  smoke.check('未加载的工具仍列在面板里', !!toolRow, '没有任何工具行')
  smoke.check('未加载的工具行带置灰标记', !!toolRow && toolRow.classList.contains('unloaded'), toolRow ? toolRow.className : '')
  smoke.check('未加载的工具显示「未加载」', !!toolRow && smoke.text(toolRow).indexOf('未加载') >= 0, smoke.text(toolRow))

  // 点开一个工具：描述与参数 schema 要能看到（列表本身只带名字与状态，schema 是点开时按需取的
  // —— 一个 server 可能有几十个工具，而这份列表每次开面板、每次授权后都会重取）。
  smoke.click(toolRow.querySelector('.mcp-tool-row'))
  const detail = await smoke.waitFor('.mcp-tool-detail', '工具行展开出描述与 schema', 15000)
  if (!detail) return smoke.finish()
  const detailText = smoke.text(detail)
  smoke.check('展开后显示工具描述', detailText.indexOf('echo from the mcpauth fixture') >= 0, detailText)
  smoke.check('显示参数名', detailText.indexOf('text') >= 0, detailText)
  smoke.check('显示参数类型', detailText.indexOf('string') >= 0, detailText)
  smoke.check('显示必填标记', !!detail.querySelector('.mcp-param-req'), detailText)
  smoke.check('显示参数说明', detailText.indexOf('what to echo back') >= 0, detailText)
  smoke.click(toolRow.querySelector('.mcp-tool-row'))
  const gone = await smoke.waitFor(() => !smoke.q('.mcp-tool-detail'), '再次点击后收起', 8000)
  smoke.check('描述与 schema 只在展开时出现', !!gone, '')

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

  // ── 5. 「已加载 / 未加载」分得开 ─────────────────────────────────────────
  // 上面那几条证明未加载的工具列着且置灰；这里把它加载起来，证明两种状态在界面上真的不同
  // （灰 → 正常色、○ → ✓、启用 → 禁用）—— 只灰不区分，等于没区分。
  // 放在最后一轮之后：先让那一轮的请求断言到「模型被告知了未加载的工具」，再把它加载。
  smoke.click('.mcp-btn')
  const row2 = await smoke.waitFor(
    () => smoke.qa('.mcp-server').find((el) => smoke.text(el).indexOf('oauth-fixture') >= 0) || null,
    '再次打开面板'
  )
  if (!row2) return smoke.finish()
  // 面板重开，折叠状态是新的（不记忆），展开一次。
  if (!row2.querySelector('.mcp-tool')) smoke.click(row2.querySelector('.mcp-server-row'))
  const row3 = await smoke.waitFor('.mcp-tool', '展开工具列表')
  if (!row3) return smoke.finish()
  smoke.check('重开面板后仍是未加载（没有自己变成已加载）', row3.classList.contains('unloaded'), row3.className)

  smoke.click(row3.querySelector('.mcp-toggle'))
  const loaded = await smoke.waitFor(
    () => {
      const el = smoke.q('.mcp-tool')
      return el && !el.classList.contains('unloaded') ? el : null
    },
    '工具变成已加载（灰标消失）'
  )
  if (!loaded) return smoke.finish()
  smoke.check('已加载的工具不再是灰的', !loaded.classList.contains('unloaded'), loaded.className)
  smoke.check('已加载的工具显示 ✓', smoke.text(loaded).indexOf('✓') >= 0, smoke.text(loaded))
  smoke.check('已加载的工具按钮变成「禁用」', smoke.text(loaded).indexOf('禁用') >= 0, smoke.text(loaded))
  // 行上那个「已加载/共」必须跟着动 —— 它是折叠状态下唯一能看到这件事的地方，一个停在 0/N
  // 的计数比没有计数更坏：它让加载看起来没发生。
  const count1 = readCount()
  smoke.check('加载一个工具后行上的分子跟着动',
    !!count1 && !!count0 && count1.loaded === count0.loaded + 1 && count1.total === count0.total,
    (count0 ? count0.text : '(无)') + ' → ' + (count1 ? count1.text : '(无)'))
  const head1 = readTotal()
  smoke.check('标题合计跟着一起动',
    !!head1 && !!head0 && head1.loaded === head0.loaded + 1 && head1.total === head0.total,
    (head0 ? head0.text : '(无)') + ' → ' + (head1 ? head1.text : '(无)'))

  // ── 6. server 级开关写回 mcp.json ───────────────────────────────────────
  // 禁用这个 server：界面上是断开，磁盘上是它来自的那个 mcp.json 里多出 "enabled": false
  // （fixture 写的文件本来没有这个键，所以这里同时验证了「补上一个键」这条路径）。
  // 写回的目标文件必须是「它来自的那个」—— Go 侧读沙箱里那个文件，并确认其余内容没被动过。
  const row4 = await smoke.waitFor(
    () => smoke.qa('.mcp-server').find((el) => smoke.text(el).indexOf('oauth-fixture') >= 0) || null,
    'server 行还在（准备禁用它）'
  )
  if (!row4) return smoke.finish()
  smoke.click(row4.querySelector('.mcp-toggle'))
  const off = await smoke.waitFor(
    () => {
      const el = smoke.qa('.mcp-server').find((s) => smoke.text(s).indexOf('oauth-fixture') >= 0)
      return el && smoke.text(el).indexOf('未连接') >= 0 ? el : null
    },
    '禁用后 server 变成未连接'
  )
  if (!off) return smoke.finish()
  smoke.check('禁用后不再显示已连接', smoke.text(off).indexOf('已连接') < 0, smoke.text(off))

  smoke.finish()
})()
