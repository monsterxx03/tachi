// perm-mcp-budget — 加载 MCP 工具会让整段 prompt 缓存失效，超过预算就先问一次。
//
// 这张卡的问题是**钱**，不是危险，所以它和 bash 那三张（perm-allow / perm-deny / perm-session）
// 有三处不同，这条场景把三处都走到：
//
//   1. 拒绝**不中止回合** —— 模型被告知原因之后继续干活（bash 那边拒绝是 errCancelled）；
//   2. 拒绝**记在本会话**上 —— 模型再调一次不再弹卡，而是直接收到同一个拒绝（第二次如果弹了卡，
//      回合会 park 在那张卡上，脚本里那句回复永远到不了）；
//   3. 卡片自己的话术：这里没有"命令"可看，只有一笔花费。
//
// 池子必须是**非空且未加载**的：沙箱的 mcp.json 指向 runner 起的 OAuth fixture，连接之后它的
// 一个工具停在 deferred 池里（scenario 把 min_tools_for_search 设成 -1）。池里没有未发现的工具
// 时，搜索不可能改变工具列表 —— 那时候不该问，也就看不到这张卡。
//
// Go 那一半负责驱动看不见的东西：拒绝真的写进了工具结果（模型收到的是"不要再调用"），以及第二次
// 调用同样被拒。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // ── 1. 先让池子真的有一个未加载的工具：授权这台 OAuth server ────────────────
  // 与 mcp-auth 的同一段路（那里才是它的主场，这里只为把池子填起来）：状态栏 → 面板 → 授权
  // （浏览器被 headless fetch 顶替）→ 连接上。
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
  smoke.click(row.querySelector('.mcp-auth-btn'))
  if (!(await smoke.waitFor('.mcp-auth', '出现授权进度卡片', 15000))) return smoke.finish()
  if (!(await smoke.waitFor('.mcp-auth-url input', '卡片里出现授权 URL'))) return smoke.finish()
  const connected = await smoke.waitFor(
    () => {
      const hit = smoke.qa('.mcp-server').filter((s) => smoke.text(s).indexOf('oauth-fixture') >= 0)
      return hit.length && smoke.text(hit[0]).indexOf('已连接') >= 0 ? hit[0] : null
    },
    '授权后 server 变成已连接',
    30000
  )
  if (!connected) return smoke.finish()
  // 工具必须停在「未加载」：这正是这次搜索会让缓存失效的前提。加载过它，后面那张卡就不该出现了。
  const count = connected.querySelector('.mcp-toolcount')
  smoke.check('连上之后一个工具都没加载（池里有未发现的工具）',
    !!count && /^0\/[1-9]/.test(smoke.text(count)), count ? smoke.text(count) : '找不到 .mcp-toolcount')
  smoke.click('.mcp-close')
  if (!(await smoke.waitFor('.composer-input', '回到编辑器'))) return smoke.finish()

  // ── 2. 一轮会去搜工具的回合：卡片出现在下一条回复之前 ──────────────────────
  smoke.type('.composer-input', '帮我找个能查数据的工具')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.perm-form', 'MCP 工具加载确认卡出现', 30000))) return smoke.finish()

  // 卡片的话术按 kind 分：这张不是「确认这条命令」，正文里也没有命令可读 —— 它要说的是一笔花费。
  const title = smoke.text('.perm-title')
  smoke.check('标题说的是花费，不是命令', title.indexOf('加载 MCP 工具会花掉一笔钱') >= 0, title)
  const preview = smoke.text('.perm-preview')
  smoke.check('预览报出整段重读的规模', preview.indexOf('当前上下文') >= 0 && preview.indexOf('30.0k tokens') >= 0, preview)
  smoke.check('预览报出这次多花多少', preview.indexOf('本次多花') >= 0 && preview.indexOf('¥2.700') >= 0, preview)
  smoke.check('预览报出预算阈值', preview.indexOf('预算阈值') >= 0 && preview.indexOf('¥1.000') >= 0, preview)
  smoke.check('预览说明池子里还有未加载的工具', preview.indexOf('未加载的 MCP 工具') >= 0, preview)
  const hint = smoke.text('.perm-hint')
  smoke.check('按钮提示说的是这次搜索，不是一条命令', hint.indexOf('这一次') >= 0 && hint.indexOf('MCP') >= 0, hint)

  const labels = smoke.qa('.perm-form .btn').map((b) => b.textContent.trim())
  smoke.check('三个选项：拒绝 / 本会话全部允许 / 允许一次',
    labels.length === 3 && labels[0].indexOf('拒绝') >= 0 && labels[1].indexOf('本会话全部允许') >= 0 && labels[2].indexOf('允许一次') >= 0,
    labels.join(' | '))

  const deny = smoke.qa('.perm-form .btn').find((b) => b.textContent.trim() === '拒绝')
  smoke.click(deny)
  if (!(await smoke.waitFor(() => !smoke.q('.perm-form'), '答复后确认卡消失'))) return smoke.finish()

  // 拒绝 ≠ 中止：回合必须继续跑完，模型给出回复。bash 那张卡在这里是 errCancelled。
  const reply = await smoke.waitText('.msg-assistant', '那用现有工具继续', 30000)
  smoke.check('拒绝后回合继续跑完了（不是 errCancelled）', !!reply, smoke.text('.chat-content').slice(-80))
  const status = smoke.qa('.tool-status')
  smoke.check('这次搜索是失败状态（工具没跑）',
    status.length > 0 && status.every((s) => s.className.indexOf('err') >= 0),
    status.map((s) => s.className).join(','))

  // ── 3. 同一个模型再搜一次：本会话已拒，不再弹卡 ────────────────────────────
  // 这一步的判据是"回复到了"本身：卡片会 park 住回合等人回答，所以只要第二轮的回复能出现，
  // 就说明**没有**第二张卡。
  smoke.type('.composer-input', '那再找一个工具吧')
  smoke.click('.send-btn')
  const reply2 = await smoke.waitText('.msg-assistant', '明白了，不搜了', 30000)
  smoke.check('第二次搜索没有被卡住（会话级的拒绝生效，没有再问）', !!reply2, smoke.text('.chat-content').slice(-80))
  smoke.check('全程只出现过一张确认卡', smoke.qa('.perm-form').length === 0, smoke.qa('.perm-form').length + ' 张卡还在')
  // 第二次调用的工具结果同样是失败：它必须已经在本会话里被记下了。
  const status2 = smoke.qa('.tool-status')
  smoke.check('第二次调用也是失败状态（被拒，而不是执行了）',
    status2.length >= 2 && status2.every((s) => s.className.indexOf('err') >= 0),
    status2.map((s) => s.className).join(','))

  smoke.finish()
})()
