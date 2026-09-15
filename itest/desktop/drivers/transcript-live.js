// transcript-live — 运行中的样子：工具还在跑时，过程条是实时条，正在跑的那张卡外露；
// 回合结束后回到摘要，且（内容超出一屏时）展开/收起都不让视图跳。
//
// 这一步故意是 `sleep 4`：太快的工具"跑完即归档"，driver 看不到运行态——所以运行态要有一个
// 真的在跑的窗口来观察。观察点两处：过程条的实时行，以及外露的运行卡。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  // 过程条的高度序列：10ms 采样、只记变化。这是"整轮不跳"的判据 —— 状态是瞬时的（工具一跑完
  // 就切回摘要），读一次不算数，所以采一串。每一格是 `<高度>:<状态类名>`。
  const trail = []
  const sampler = setInterval(() => {
    const h = smoke.q('.process-head')
    const r = h ? h.getBoundingClientRect() : null
    const entry = (r ? r.height.toFixed(1) : 'gone') + ':' + (h ? h.className.replace('process-head', '').trim() : '-')
    if (trail[trail.length - 1] !== entry) trail.push(entry)
  }, 10)

  smoke.type('.composer-input', '睡一会儿')
  smoke.click('.send-btn')

  // ① 工具在跑
  if (!(await smoke.waitFor('.process-head.live', '实时条出现（工具正在跑）', 20000))) return smoke.finish()
  const live = smoke.text('.process-head')
  smoke.check('实时条说清正在做什么', live.indexOf('正在') >= 0, live)
  smoke.check('实时条报出第几步', live.indexOf('第 1 步') >= 0, live)
  smoke.check('实时条有脉冲点（"还在动"的信号）', !!smoke.q('.process-head .process-dot'))
  // 运行中的卡不外露：它每完成一步就消失一次，会让下面的正文一上一下地跳。这个事实由实时行
  // 承担；想看卡本身，点开过程条即可（它就在时序里，且会原地更新）。
  smoke.check('运行中的卡不外露（页面不再逐步一跳）',
    smoke.qa('.tool-card').length === 0, smoke.qa('.tool-card').length + ' 张卡')
  smoke.click(smoke.q('.process-head'))
  if (!(await smoke.waitFor(() => smoke.qa('.tool-card').length === 1, '展开后能看到运行中的卡'))) return smoke.finish()
  smoke.check('展开时序能看到正在跑的那张卡',
    smoke.text('.tool-card').indexOf('sleep 4') >= 0, smoke.text('.tool-card').slice(0, 40))
  smoke.click(smoke.q('.process-head'))
  if (!(await smoke.waitFor(() => smoke.qa('.tool-card').length === 0, '收起后卡片又隐去'))) return smoke.finish()

  // ② 回合结束：实时态收掉，摘要回来，活动行消失
  if (!(await smoke.waitText('.msg-assistant .msg-content', '好验证跟随底部', 40000))) return smoke.finish()
  if (!(await smoke.waitFor(() => !smoke.q('.process-head.live'), '结束后实时条变回摘要'))) return smoke.finish()
  const settled = smoke.text('.process-head')
  smoke.check('结束后过程条改报步数', settled.indexOf('2 步') >= 0, settled)
  smoke.check('结束后成功那步已归档（卡回到过程条里）', smoke.qa('.tool-card').length === 0,
    smoke.qa('.tool-card').length + ' 张卡')

  // 整个过程条的高度必须是一个定值：实时态与摘要态之间来回切换（每一步都切一次）不能改变
  // 它的高度，否则转录钉在底部，整片可见内容会跟着一上一下地跳。曾经就差在那条 1px 边框上
  // （实时态有、摘要态没有 → 每次切换 2px）。
  clearInterval(sampler)
  const heights = [...new Set(trail.map((e) => e.split(':')[0]).filter((h) => h !== 'gone'))]
  smoke.check('过程条高度整轮恒定（"工具在跑"与"两次调用之间"不改变高度）',
    heights.length === 1, trail.join(' | '))
  smoke.check('过程条出现后不再消失（整轮都占着那一行）',
    trail[0].indexOf('gone') === 0 && trail.slice(1).every((e) => e.indexOf('gone') !== 0),
    trail.join(' | '))

  // ③ 折叠会收缩高度，所以"跟随底部"必须在收缩方向也成立。要断言这一点，就得真的制造一次
  // 收缩：先展开（长高），再收起（变矮），两次都读底部距离。只读"回合结束后贴底"是空的——
  // 那时内容只增长过。
  const c = smoke.q('.chat')
  const bottomGap = () => c.scrollHeight - c.scrollTop - c.clientHeight
  smoke.check('回复够长（转写超过一屏）', c.scrollHeight > c.clientHeight + 40,
    `${c.scrollHeight} vs ${c.clientHeight}`)

  const before = bottomGap()
  smoke.click(smoke.q('.process-head'))
  const justAfterClick = bottomGap()
  if (!(await smoke.waitFor(() => smoke.q('.process-timeline'), '展开时序'))) return smoke.finish()
  // 长高会让视图先向上滑一滑（点开就变高的折叠体都是这个行为，见 App.tsx 里那段注释），
  // 之后由内容盒子的 ResizeObserver 拉回底部 —— 所以要等它稳定，别在读之前就断言。
  smoke.sleep(400)
  const expanded = c.scrollHeight
  smoke.check('展开之后贴底', bottomGap() <= 4,
    `点击前 ${before}px · 点击后 ${justAfterClick}px · 静置后 ${bottomGap()}px（scrollTop=${c.scrollTop}/${c.scrollHeight - c.clientHeight}）`)

  smoke.click(smoke.q('.process-head'))
  if (!(await smoke.waitFor(() => !smoke.q('.process-timeline'), '收起时序'))) return smoke.finish()
  smoke.check('收起确实让内容变矮（真的发生了收缩）', c.scrollHeight < expanded,
    `${expanded} → ${c.scrollHeight}`)
  smoke.check('收缩之后仍贴在底部', bottomGap() <= 4, `底部距离 ${bottomGap()}px`)
  smoke.finish()
})()
