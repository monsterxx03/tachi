// switch-scroll — switching away from a RUNNING session and back must land at the newest
// message and stay there.
//
// The report that started this: with a turn still streaming, going to another session and
// coming back showed the transcript drift UP first and only then snap down. An impression is
// not a measurement, so this driver samples the distance from the bottom across the switch —
// from a ResizeObserver callback, which is where the browser is done laying out and has not yet
// painted, i.e. the state that actually reaches the screen. A clean switch keeps every sample at
// ~0; the failure mode shows up as one large number (the excursion up) before the final 0.
//
// The turn is kept alive with mock pauses on purpose — without a running turn there is nothing
// streaming in while the switch happens, which is the whole point.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  // Turn 1 puts a mermaid diagram in the history: it renders asynchronously, so on every
  // switch back it grows from its placeholder into a figure — content that changes height
  // AFTER the view is pinned to the bottom.
  smoke.type('.composer-input', '先画一张图')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.chat .mermaid', '图表渲染完成', 30000))) return smoke.finish()

  // Turn 2 stays running while the driver goes away and comes back.
  smoke.type('.composer-input', '写一段长回复')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.stop-btn', '回合开始（停止按钮出现）', 15000))) return smoke.finish()

  const runningTitle = smoke.text('.session.active .session-title')

  // Two probes, and one rule each — because where the read happens decides whether the number
  // means anything.
  //
  //   - drawn (this driver's own ResizeObserver): the state that gets PAINTED, and the one that
  //     decides. The app pins to the bottom from a ResizeObserver callback, which the browser
  //     delivers after layout and before paint; an observer registered HERE runs later in the
  //     same delivery (observers are called in registration order), so a gap read in it is the
  //     gap on screen.
  //   - raw (setInterval): every intermediate state, from an independent task. It cannot tell a
  //     real drift from a mid-frame transient on its own, so it only decides when `drawn` has no
  //     samples at all (an occluded window may run no observer deliveries) — and then it
  //     requires the excursion to LAST (two consecutive samples, i.e. about a frame).
  //
  // A requestAnimationFrame probe used to stand where `drawn` does, reading scrollHeight in the
  // frame callback. It has been REMOVED, because reading scrollHeight FORCES LAYOUT, layout runs
  // BEFORE the app's pin, and so that series reported the intermediate state "content grew, pin
  // not run yet" — measured: a lone 298px frame with 0 on both sides, i.e. a gap the pin closed
  // before anything was drawn. No read that happens before the pin can answer "what was drawn".
  const drawn = []
  const raw = []
  const content = smoke.q('.chat-content')
  const observer = new ResizeObserver(() => {
    const el = smoke.q('.chat')
    if (el) drawn.push({ t: Date.now(), gap: Math.round(el.scrollHeight - el.scrollTop - el.clientHeight) })
  })
  if (content) observer.observe(content)
  const probeTimer = setInterval(() => {
    const el = smoke.q('.chat')
    if (el) raw.push({ t: Date.now(), gap: Math.round(el.scrollHeight - el.scrollTop - el.clientHeight) })
  }, 16)

  // Away: a brand-new session (its own empty transcript).
  if (!smoke.click('.new-chat')) return smoke.fail('点击新建会话', '按钮找不到')
  if (!(await smoke.waitFor(() => smoke.text('.session.active .session-title') !== runningTitle, '切到新会话', 10000))) return smoke.finish()

  // Back: the running session's row.
  const row = smoke.qa('.session').find((s) => smoke.text(s.querySelector('.session-title')) === runningTitle)
  if (!row) return smoke.fail('在侧边栏找到运行中的会话', runningTitle)
  if (!smoke.click(row)) return smoke.fail('点回运行中的会话', '行找不到')
  const backAt = Date.now()
  if (!(await smoke.waitFor(() => smoke.text('.session.active .session-title') === runningTitle, '切回运行中的会话', 10000))) return smoke.finish()

  // Sample until the streaming turn is over, so a late correction is seen too.
  await smoke.waitGone('.stop-btn', '这一轮跑完', 30000)
  observer.disconnect()
  clearInterval(probeTimer)

  // Both series from the moment of the switch back — by TIME, so an empty drawn series cannot
  // shift the raw window.
  const back = drawn.filter((s) => s.t >= backAt)
  const tail = raw.filter((s) => s.t >= backAt)
  const drawnWorst = back.length ? Math.max(...back.map((s) => s.gap)) : -1
  const drawnLast = back.length ? back[back.length - 1].gap : -1
  // The timer series over the same window: the longest run of consecutive samples off the
  // bottom (one sample ≈ one frame at this rate).
  let rawRun = 0
  let worstRun = 0
  let rawWorst = 0
  for (const s of tail) {
    rawWorst = Math.max(rawWorst, s.gap)
    rawRun = s.gap > 24 ? rawRun + 1 : 0
    worstRun = Math.max(worstRun, rawRun)
  }
  // Three delivery batches are enough to see a drift that survives a frame; a window that runs
  // no deliveries at all leaves the series empty, and the raw one decides instead.
  const drawnJudge = back.length >= 3
  smoke.log('切回后的距底采样', `绘制帧=${back.length} 最大=${drawnWorst}px 末帧=${drawnLast}px；` +
    `原始采样=${tail.length} 最大=${rawWorst}px 最长连续偏离=${worstRun}；判据=${drawnJudge ? '绘制帧' : '连续偏离（没有观测回调）'}`)

  if (drawnJudge) {
    smoke.check('切回后光标落在最新消息（末帧贴底）', drawnLast >= 0 && drawnLast <= 4, `末帧距底 ${drawnLast}px`)
    smoke.check('切回过程没有先向上飘（每一绘制帧都贴底）', drawnWorst >= 0 && drawnWorst <= 24,
      `最大距底 ${drawnWorst}px`)
    smoke.finish()
    return
  }
  // No observer deliveries: the window is not painting (something else is in front), so judge
  // the drift by whether it lasted. Skipping the check would silently pass on an empty series.
  if (!smoke.check('探针采到了样本', tail.length >= 3, `${tail.length} 个采样`)) return smoke.finish()
  smoke.check('切回过程没有持续地向上飘（没出帧时的保守判据）', worstRun <= 1,
    `最长连续偏离 ${worstRun} 个采样，最大 ${rawWorst}px`)
  smoke.finish()
})()
