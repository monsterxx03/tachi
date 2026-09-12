// ctx-ring — 上下文占用环（ContextMeter）必须跟着 API 调用走，而不是等这一轮结束。
//
// 报的现象：新建会话后 agent 跑了很多，环一直是空的（0.0%），点开明细却有数字，切到别的
// 会话再回来就正常。两条路本来不同：环读 App 的 ctxEstimate/ctxWindow（只在
// refreshProvider() 时更新：挂载 / 新建 / 切换 / 每轮 turn_complete），明细读
// GetContextInfo(sessionId)（打开时实时 fetch）。于是**一个还在跑的长 turn**（多轮工具
// 调用）里，环停在 turn 开始前的值，明细是新的 —— 切换会话之所以「修好」它，是因为切换
// 顺手刷新了一次。
//
// `sleep 4` 的工具把那个窗口撑开，断言就是「进行中环也有数」，并与同一时刻的明细对比。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  const ringLabel = () => {
    const el = smoke.q('.ctx-btn[aria-label^="上下文"]')
    return el ? (el.getAttribute('aria-label') || '(空 label)') : '(找不到 .ctx-btn)'
  }
  // The ring's own label: `上下文 X%（A / B），点击查看明细`. `null` when the window is
  // unknown — the label then reads 上下文 —, which is NOT "zero" but "nothing to measure".
  const ringPct = () => {
    const el = smoke.q('.ctx-btn[aria-label^="上下文"]')
    if (!el) return null
    const m = /上下文\s*([\d.]+)%/.exec(el.getAttribute('aria-label') || '')
    return m ? parseFloat(m[1]) : null
  }
  // The popover says `4.4% 的上下文窗口` (only when both numbers are known).
  const detailPct = () => {
    const m = /([\d.]+)%\s*的上下文窗口/.exec(smoke.text('.ctx-panel'))
    return m ? parseFloat(m[1]) : null
  }

  smoke.q('.chat').focus()
  if (!smoke.click('.new-chat')) return smoke.fail('点击新建会话', '按钮找不到')
  if (!(await smoke.waitFor('.welcome', '新会话是空的（welcome 出现）', 8000))) return smoke.finish()
  smoke.log('新建会话后（还没跑 turn）', ringLabel())

  smoke.type('.composer-input', '跑一个慢工具')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.stop-btn', '这一轮跑起来了', 20000))) return smoke.finish()
  // The first API call has come back (the mock does not stall) and the tool is sleeping —
  // this is the window 「执行了很多」 describes.
  await smoke.sleep(1500)
  const midRing = ringPct()
  const midLabel = ringLabel()

  smoke.click('.ctx-btn')
  if (!(await smoke.waitFor('.ctx-panel', '明细面板打开', 5000))) return smoke.finish()
  const midDetail = detailPct()
  const midDetailText = smoke.text('.ctx-panel')
  smoke.click('.ctx-btn')

  // The two numbers describe the same moment, so they have to agree: the ring is allowed to
  // lag a little behind a call in flight, but not to still read 0.0%.
  smoke.check('长 turn 进行中上下文环就有数（不等这一轮结束）', midRing !== null && midRing > 0,
    `环=${midLabel}${midRing === null ? '（无法解析：窗口未知）' : ''}`)
  smoke.check('进行中环与明细说的是同一件事（差值 < 0.5 个百分点）',
    midRing !== null && midDetail !== null && Math.abs(midRing - midDetail) < 0.5,
    `环=${midRing}% · 明细=${midDetail}% · 明细文本="${midDetailText}"`)

  await smoke.waitFor(() => !smoke.q('.stop-btn'), '这一轮结束', 30000)
  await smoke.sleep(400)
  const endRing = ringPct()
  smoke.check('本轮结束后环仍然有数', endRing !== null && endRing > 0, ringLabel())

  smoke.finish()
})()
