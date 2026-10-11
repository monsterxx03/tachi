// at-session — `#s:` 引用另一个会话。
//
// 现场：app 打开的是最新的「冒烟会话」，另一个 fixture 会话（另一个会话）在列表里但没被显示。
// 驱动侧断三件事：打 "#" 先给出**命名空间候选**，敲成 "#s:" 后打开的是**会话**命名空间（不是
// 文件），Tab 插进来的是**完整 id 的引用**（不是标题，也不是那个会话的内容）。
//
// 「模型到底收到什么」由 Go 侧断（scenarios.go 的 after）：那一行指针 + transcript 路径，
// 绝不是另一段对话的正文 —— 内联它会让本会话的上下文为一份没人要读的转写买单。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // 只打一个 "#"：弹窗给的是两个命名空间本身，读者不必记住拼写。
  smoke.type('.composer-input', '#')
  if (!(await smoke.waitFor('.at-picker', '# 打开补全弹窗'))) return smoke.finish()
  smoke.check('弹窗是引用命名空间', smoke.text('.at-picker-title').indexOf('# 引用') >= 0,
    smoke.text('.at-picker-head'))
  if (!(await smoke.waitText('.at-picker-list', '#a:', 10000))) return smoke.finish()
  const candidates = smoke.text('.at-picker-list')
  smoke.check('给出 #a: 与 #s: 两种候选',
    candidates.indexOf('#a:') >= 0 && candidates.indexOf('#s:') >= 0, candidates)

  smoke.type('.composer-input', '#s:')
  if (!(await smoke.waitFor('.at-picker', '#s: 打开会话列表'))) return smoke.finish()
  smoke.check('弹窗是会话命名空间', smoke.text('.at-picker-title').indexOf('# 会话') >= 0,
    smoke.text('.at-picker-head'))

  if (!(await smoke.waitText('.at-picker-list', '另一个会话', 10000))) return smoke.finish()
  smoke.check('列出另一个会话', true, smoke.text('.at-picker-list'))
  smoke.check('当前会话不在列表里（引用自己没有意义）',
    smoke.text('.at-picker-list').indexOf('冒烟会话') < 0, smoke.text('.at-picker-list'))

  // Tab 接受高亮行。插进来的必须是后端能展开的那个引用：完整 id，无空格。
  // React 的重渲染是异步的 —— 等这个值出现，而不是在同一个 tick 里读 DOM。
  smoke.key('.composer-input', 'Tab')
  const value = await smoke.waitFor(() => {
    const v = smoke.q('.composer-input').value
    return /^#s:\d{4}-\d{2}-\d{2}-\d{6}-[0-9a-f]{8} $/.test(v) ? v : null
  }, 'Tab 把完整 id 的 #s: 引用插进输入框')
  if (!value) return smoke.finish()
  smoke.check('插入完整 id 的 #s: 引用', true, value)
  smoke.check('接受后弹窗关闭', !smoke.q('.at-picker'), '')

  smoke.type('.composer-input', value + '这个会话里问过什么？')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant .msg-content', '读过那个会话的转写', 30000))) return smoke.finish()

  // 气泡显示用户打的原文：展开只发生在发给模型的那一份（session.Message.DisplayContent）。
  const bubble = smoke.text('.msg-user')
  smoke.check('用户气泡显示的是 #s: 原文', bubble.indexOf('#s:') >= 0, bubble.slice(0, 80))
  smoke.check('气泡里没有展开后的指针', bubble.indexOf('[#s:') < 0, bubble.slice(0, 120))
  smoke.finish()
})()
