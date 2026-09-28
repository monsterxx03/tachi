// bubble-lines — 用户气泡必须保留自己敲的换行。
//
// composer 的 Shift+Enter、粘贴进来的多行文本，都以 `\n` 存进会话（模型收到的就是带换行的
// 原文），气泡把消息当纯文本渲染（App.tsx 的 UserBubble），所以换行能不能活下来只取决于气泡
// 自己的 white-space——默认的 `normal` 把每个换行折成一个空格（报的现象：「发送后气泡里的换行
// 都没了」）。
//
// 断言的是渲染结果本身：文本里还留着换行，那段文字真的占两行（Range 量的是文字自己的行盒，
// 气泡里那行时间戳、内边距都影响不到它）。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  // 会话的首次加载在启动后那一帧才落定，输入框在那次渲染里会被重挂一次——冷启动时敲进去的
  // 字就这样没了，点击落在 disabled 的按钮上，症状却是「气泡一直没出现」。等它落定再敲。
  await smoke.waitFor(() => !smoke.q('.chat-loading'), '会话加载完成', 10000)

  smoke.type('.composer-input', '第一行\n第二行')
  // 等按钮反映这次输入（它就是 send 读的东西）：没反映出来就报这里，而不是报后面没有气泡。
  const send = await smoke.waitFor(
    () => { const b = smoke.q('.send-btn'); return b && !b.disabled ? b : null },
    '输入后发送按钮可用', 5000)
  if (!send) return smoke.finish()
  smoke.click(send)
  if (!(await smoke.waitFor('.msg-user .msg-content', '用户气泡出现', 15000))) return smoke.finish()
  // 回复落地后再读：气泡的布局在那之后才最终确定。
  await smoke.waitFor(() => smoke.qa('.msg-assistant').length > 0, '回复落地', 30000)

  const bubble = smoke.q('.msg-user .msg-content')
  const node = Array.prototype.slice.call(bubble.childNodes)
    .filter((n) => n.nodeType === 3 && n.data.trim())[0]
  if (!node) return smoke.fail('气泡里有一段消息文本', '找不到文本节点')
  smoke.check('气泡文本原样保留了换行', node.data.indexOf('\n') >= 0, JSON.stringify(node.data))

  const style = getComputedStyle(bubble)
  smoke.check('气泡的空白模式不折叠换行（pre-wrap）', style.whiteSpace === 'pre-wrap',
    'white-space=' + style.whiteSpace)

  const range = document.createRange()
  range.selectNodeContents(node)
  const box = range.getBoundingClientRect()
  const line = parseFloat(style.lineHeight)
  const rows = Math.round(box.height / line)
  smoke.check('那段文字排成了两行（不是折成一行）', rows === 2,
    `文字高 ${Math.round(box.height)}px / 行高 ${Math.round(line)}px = ${rows} 行`)

  smoke.finish()
})()
