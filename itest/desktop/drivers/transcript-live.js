// transcript-live — 运行中的样子：工具还在跑时，过程条是实时条，正在跑的那张卡外露；
// 回合结束后回到摘要，且（内容超出一屏时）展开/收起都不让视图跳。
//
// 这一步故意是 `sleep 4`：太快的工具"跑完即归档"，driver 看不到运行态——所以运行态要有一个
// 真的在跑的窗口来观察。观察点两处：过程条的实时行，以及外露的运行卡。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '睡一会儿')
  smoke.click('.send-btn')

  // ① 工具在跑
  if (!(await smoke.waitFor('.process-head.live', '实时条出现（工具正在跑）', 20000))) return smoke.finish()
  const live = smoke.text('.process-head')
  smoke.check('实时条说清正在做什么', live.indexOf('正在') >= 0, live)
  smoke.check('实时条报出第几步', live.indexOf('第 1 步') >= 0, live)
  smoke.check('实时条有脉冲点（"还在动"的信号）', !!smoke.q('.process-head .process-dot'))
  smoke.check('正在跑的那张卡外露在过程条之外（没被折叠）',
    smoke.qa('.tool-card').length === 1, smoke.qa('.tool-card').length + ' 张卡')

  // ② 回合结束：实时态收掉，摘要回来，活动行消失
  if (!(await smoke.waitText('.msg-assistant .msg-content', '好验证跟随底部', 40000))) return smoke.finish()
  if (!(await smoke.waitFor(() => !smoke.q('.process-head.live'), '结束后实时条变回摘要'))) return smoke.finish()
  const settled = smoke.text('.process-head')
  smoke.check('结束后过程条改报步数', settled.indexOf('1 步') >= 0, settled)
  smoke.check('结束后成功那步已归档（卡回到过程条里）', smoke.qa('.tool-card').length === 0,
    smoke.qa('.tool-card').length + ' 张卡')

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
