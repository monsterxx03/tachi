// external-link — 消息区里的链接点下去要交给**系统浏览器**，而不是让 webview 自己导航走。
//
// 现场：mock 的回复里带一个链接，driver 真的点它。webview 没有标签页也没有后退键（窗口就是 app 本身），
// 所以旧行为是「点一下，整个界面被那个网页换掉，且回不来」（issue #48）。
//
// 断言分两半：driver 侧证明链接是个**带地址的控制**、点完应用还在原地（真导航会把页面整个换掉，这个
// driver 连下一行都跑不到）；Go 侧证明那次点击真的要系统去开那个地址 —— app 在脚本运行里把 launcher
// 换成了记录器（desktop/demo.go 的 TACHI_DEMO_OPEN_LOG），所以 smoke 永远不会在别人的机器上弹出浏览器，
// 而落下来的 argv 就是「本来会被打开的东西」。
const LINK = 'https://example.com/tachi-link-smoke'

;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  smoke.type('.composer-input', '给个链接')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.chat', '看这个', 30000))) return smoke.finish()

  // 按**名字**找（链接上写的字），不按位置：一段话里几个链接只有标签分得开。
  const link = await smoke.waitFor(
    () => smoke.qa('.assistant-text a.md-link').filter((a) => smoke.text(a) === '示例')[0] || null,
    '回答里渲染出一个链接', 8000)
  if (!link) return smoke.finish()

  // 它得是个真链接：地址挂在 href / title 上，悬停看得到、读屏念得出。
  smoke.check('链接带着它要去的地址', link.getAttribute('href') === LINK, link.getAttribute('href') || '(没有 href)')
  smoke.check('悬停也能看到地址', (link.getAttribute('title') || '') === LINK, link.getAttribute('title') || '(没有 title)')

  smoke.click(link)
  // 副作用要落地：这一点是 fire-and-forget（前端不 await 那个绑定），而 runner 一收到 driver 的结果
  // 就杀 app —— 这是 driver 里唯一合法的固定等待（界面对了，欠的是一次写入）。
  await smoke.sleep(500)
  smoke.check('点完还在对话里（webview 没有被导航带走）',
    !!smoke.q('.composer-input') && smoke.text('.chat').indexOf('看这个') >= 0, '')
  smoke.finish()
})()
