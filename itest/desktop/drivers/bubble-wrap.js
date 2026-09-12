// bubble-wrap — 用户气泡里的长链接必须折行，不能把气泡撑破。
//
// 用户粘贴一条长 URL（没有空格可断），`.msg-user .msg-content` 上既没有 overflow-wrap
// 也没有 word-break，于是默认的 `normal` 让整条链接直着冲出气泡右边缘（报的现象：
// 「贴了个长链接，没有自动折行，突破了消息气泡」）。这里读到的是气泡自己的滚动尺寸：
// 溢出时 scrollWidth 会大于 clientWidth。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  const long = 'https://example.com/' + 'a'.repeat(200) + '?q=' + 'b'.repeat(60)
  smoke.type('.composer-input', '看看这个链接：' + long)
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.msg-user .msg-content', '用户气泡出现', 15000))) return smoke.finish()
  // 回复落地后再读：气泡的布局在那之后才最终确定。
  await smoke.waitFor(() => smoke.qa('.msg-assistant').length > 0, '回复落地', 30000)

  const bubble = smoke.q('.msg-user .msg-content')
  const over = bubble.scrollWidth - bubble.clientWidth
  const width = Math.round(bubble.getBoundingClientRect().width)
  smoke.check('长链接在气泡内折行（不横向溢出）', over <= 1,
    `scrollW/clientW = ${bubble.scrollWidth}/${bubble.clientWidth}`)
  smoke.check('气泡宽度没有被长链接撑开（≤ 520px 上限内）', width <= 520,
    `width=${width}px`)
  smoke.check('链接内容完整（折行不是截断）', smoke.text(bubble).indexOf('bbb') >= 0,
    `气泡文本 ${smoke.text(bubble).length} 字`)

  smoke.finish()
})()
