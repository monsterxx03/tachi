// agents-panel — 状态栏的 Agents 按钮与节点面板。
//
// 现场：沙箱配了一个 peer（`smoke-nas` → https://127.0.0.1:1，那个端口上没人听，所以探测是**立刻
// 被拒**而不是等超时）。这给面板三样东西可看，而且每一样都不能靠猜：
//
//   - 行里三件事实各就各位：本机配的别名、**本机 pin 的**身份（这里从来没有连过，所以是「身份未记
//     录」）、以及上次探测的结论；
//   - 展开后的详情里有本机用来够它的地址（配置的，不是 card 自称的）、它自己的说法（这里没有，
//     因为它没回答）、以及它在这条消息里该怎么被引用（`#a:smoke-nas`）；
//   - 「刷新」是一次真的往返：它把缓存丢掉重新问，所以那个连不上的节点最终显示的是「联系不上」，
//     而不是永远停在「检查中…」。
//
// 「联系不上」是这一场景的关键断言：一个点完刷新仍然停在原地的面板会让读者以为是自己的问题。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  smoke.click('.agents-btn')
  if (!(await smoke.waitFor('.agents-panel', 'Agents 面板打开'))) return smoke.finish()
  smoke.check('面板标题是 Agents', smoke.text('.mcp-title').indexOf('Agents') >= 0, smoke.text('.mcp-head'))

  if (!(await smoke.waitText('.agents-panel', 'smoke-nas', 10000))) return smoke.finish()
  const row = smoke.text('.mcp-server-row')
  smoke.check('行里有本机配的别名', row.indexOf('smoke-nas') >= 0, row)
  // 从来没有出站连过它，所以本机没有可展示的身份 —— 而那个"未知"是如实回答，不是缺数据。
  smoke.check('身份未记录（本机还没跟它连过）', row.indexOf('身份未记录') >= 0, row)

  // ── 展开：地址是配置的，引用是选择器会插的那种拼写 ───────────────────────
  smoke.click('.mcp-server-row')
  if (!(await smoke.waitText('.agents-detail', '127.0.0.1:1', 10000))) return smoke.finish()
  const detail = smoke.text('.agents-detail')
  smoke.check('展开后给出本机够它的地址', detail.indexOf('https://127.0.0.1:1') >= 0, detail)
  smoke.check('展开后给出该用的引用拼写', detail.indexOf('#a:smoke-nas') >= 0, detail)
  smoke.check('没有 card 的时候说的是「没回答」而不是编一个名字',
    detail.indexOf('它没有回答') >= 0 || detail.indexOf('还没检查') >= 0, detail)

  // ── 刷新是一次真的往返 ────────────────────────────────────────────────────
  smoke.click('.agents-refresh')
  if (!(await smoke.waitText('.agents-panel', '联系不上', 30000))) return smoke.finish()
  smoke.check('刷新之后那个连不上的节点说得出「联系不上」', true, smoke.text('.mcp-server-row'))
  smoke.check('面板说得出这批结论是什么时候取的',
    smoke.text('.mcp-head').indexOf('上次检查') >= 0, smoke.text('.mcp-head'))

  // 点面板外面关掉它（自然行为，且必须真的关）。这里派发的是 mousedown 而不是 click：面板听的
  // 是 mousedown（一次按下才算「点进去」），而 smoke.click 只发 click 事件。
  smoke.q('.chat-content').dispatchEvent(new MouseEvent('mousedown', { bubbles: true }))
  await smoke.sleep(150)
  smoke.check('点外面关掉面板', !smoke.q('.agents-panel'), '')
  smoke.finish()
})()
