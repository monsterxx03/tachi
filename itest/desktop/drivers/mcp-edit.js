// mcp-edit — 设置页里的 MCP server 编辑器：这一页唯一一个不是 config.yaml 叶子的区块。
//
// 它和页面其余部分的行为**不一样**，也正是端到端值得跑一遍的地方：它不经过 SaveSettings，每条自己的
// 「保存」直接写 mcp.json 并立即装进运行中的进程。单测（Go）钉得住「写文件 + 装 config」，看不见的是
// **声明有没有送到界面**：区块画没画在 MCP 分组里、列表里是不是文件里那几条、来源标签写的是哪个文件、
// 以及「这一块立即生效」那句话在不在。
//
// 磁盘那一半由 Go 侧断言（scenarios.go 的 mcp-edit）：fixture 那条逐字节没动、新条目在、写出来的文件
// 是装载器认得的形状。driver 只说界面。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  const catByName = (name) =>
    smoke.qa('.settings-cat').find((b) => smoke.text(b.firstElementChild) === name)

  // ── 进设置页 → MCP 分组 ──────────────────────────────────────────────────
  const settingsBtn = smoke.qa('.footer-item').find((b) => smoke.text(b) === '设置')
  if (!settingsBtn) return smoke.fail('侧栏底部有「设置」入口', smoke.allText('.footer-item').join(' / '))
  smoke.click(settingsBtn)
  if (!(await smoke.waitFor('.settings-view', '设置页出现'))) return smoke.finish()

  const mcpCat = catByName('MCP')
  if (!mcpCat) return smoke.fail('左栏有「MCP」分组', smoke.allText('.settings-cat').join(' / '))
  smoke.click(mcpCat)
  if (!(await smoke.waitFor('.mcp-servers', 'MCP 分组里出现 server 编辑器'))) return smoke.finish()

  // ── 列出来的就是 mcp.json 里那几条 ───────────────────────────────────────
  // 这个区块不是配置叶子：它不出现在 .set-row 里，而是自己一块（.set-section.mcp-servers）。
  // 位置也说一句 —— 它必须落在 MCP 分组**里面**，落在别处就是「声明没送到界面」。
  const names = () => smoke.qa('.mcp-entry-name').map((e) => smoke.text(e))
  smoke.check('MCP 分组里列出文件里那条 server', names().join('|') === 'echo-fixture', names().join(' | '))
  const src = smoke.text(smoke.qa('.mcp-entry-src')[0] || '')
  smoke.check('来源标签写的是它来自哪个文件', src === 'mcp.json', src)

  // 这一块与页面底部的「保存」无关，必须自己说清楚 —— 否则读者会以为改了要按下面那个按钮。
  const hint = smoke.text('.mcp-servers-hint')
  smoke.check('区块说清了自己立即写 mcp.json', hint.indexOf('立即') >= 0 && hint.indexOf('mcp.json') >= 0, hint)

  // ── 加一条：填名字与 command，并且**取消勾选启用** ────────────────────────
  // 取消勾选是必须的：smoke 跑在真机上，一个启用的 stdio server 会被真的拉起来。写下来也是
  // 「这个键不出现才是启用」那条规矩的正面用例 —— 界面上取消勾选必须落成 enabled: false。
  const add = smoke.qa('.set-add .btn').find((b) => smoke.text(b) === '＋ 添加 server')
  if (!add) return smoke.fail('有「＋ 添加 server」', smoke.allText('.set-add .btn').join(' / '))
  smoke.click(add)
  if (!(await smoke.waitFor('.mcp-form', '新 server 的表单出现'))) return smoke.finish()

  const field = (k) => smoke.q('.mcp-form [data-field="' + k + '"]')
  if (!field('name') || !field('command')) return smoke.fail('表单里有 name 与 command', '')
  smoke.type(field('name'), 'smoke-added')
  smoke.type(field('command'), '/bin/true')
  const enabled = field('enabled')
  if (!enabled) return smoke.fail('表单里有「启用」开关', '')
  if (enabled.checked) smoke.click(enabled)
  smoke.check('表单上的启用被取消（真机上不能拉起进程）', field('enabled').checked === false, '')

  const saveBtn = smoke.qa('.mcp-form-actions .btn').find((b) => smoke.text(b) === '保存')
  if (!saveBtn) return smoke.fail('表单里有「保存」', smoke.allText('.mcp-form-actions .btn').join(' / '))
  smoke.click(saveBtn)

  // ── 保存之后：新条目出现在列表里，来源还是 base 那个文件 ──────────────────
  if (!(await smoke.waitFor(() => names().indexOf('smoke-added') >= 0, '新 server 出现在列表里'))) {
    return smoke.finish()
  }
  smoke.check('保存后列表里两条都在', names().join('|') === 'echo-fixture|smoke-added', names().join(' | '))
  const rows = smoke.qa('.mcp-entry')
  const last = rows[rows.length - 1]
  smoke.check('新条目的来源是 base 的 mcp.json',
    smoke.text(last.querySelector('.mcp-entry-src')) === 'mcp.json',
    smoke.text(last.querySelector('.mcp-entry-src')))
  // 表单收起 = 这一条已经落盘（保存成功才关表单）；失败会留在原地并把原因写在下面。
  smoke.check('保存成功后表单收起', !smoke.q('.mcp-form'), '')

  smoke.finish()
})()
