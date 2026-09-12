// compact — /compact moves the conversation into a child session, and the UI has to say so in
// three places at once: the meter that reports the context, the sidebar (one row per conversation,
// the pre-compaction session folded under it), and the disk (the chain linked both ways).
//
// The meter is the interesting half. Compaction's whole point is that the context got smaller, so
// the ring's own number is the reader's evidence — captured BEFORE and AFTER rather than asserted
// from memory, because a stale meter is exactly the bug this scenario was written for.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  // A first turn, so there is something to summarise and a measured context to compare against.
  smoke.type('.composer-input', '看一下工作目录')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.chat', '这是一段很长的历史内容', 30000))) return smoke.finish()

  // The ring's own label: `上下文 X%（A / B），点击查看明细`. It is on the BUTTON — the SVG is
  // aria-hidden — and read as a number, so the assertion is about the quantity, not the string.
  const ringPct = () => {
    const el = smoke.q('.ctx-btn[aria-label^="上下文"]')
    if (!el) return null
    const m = /上下文\s*([\d.]+)%/.exec(el.getAttribute('aria-label') || '')
    return m ? parseFloat(m[1]) : null
  }
  const usageRow = () => smoke.text('.usage-meta')
  const rows = () => smoke.qa('.session')
  // A second turn: the estimate describes the PROMPT of a call, so the big reply only enters the
  // measurement when the next turn is sent with it in the history.
  smoke.type('.composer-input', '继续')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.chat', '第二轮回复', 30000))) return smoke.finish()

  // The first turn is a big reply on purpose (see the scenario), so the estimate it leaves behind
  // is clearly above the system-prompt floor — waiting for the reply TEXT alone would race the
  // estimate, and reading the floor as "before" would make the comparison meaningless.
  const before = await smoke.waitFor(() => {
    const p = ringPct()
    return p !== null && p > 10 ? p : null
  }, '第二轮后上下文环报出被撑起的占用', 20000)
  if (before === null) return smoke.finish()
  const usageBefore = usageRow()
  smoke.check('压缩前上下文环有占用', before > 0, `${before}% · 用量行="${usageBefore}"`)
  smoke.check('压缩前侧栏只有一个会话', rows().length === 1, rows().length + ' 行')

  // /compact: the command runs the summarising turn, then the conversation moves to the child.
  smoke.type('.composer-input', '/compact')
  smoke.click('.send-btn')

  // The signal to wait for is the FOLD, not the 「对话已压缩」 notice: the notice is written into
  // the PARENT's transcript (the conversation the compacted one replaces), so the session switch
  // that follows it takes the notice off screen with it. Sample the transcript while waiting so
  // the notice can still be reported as having been seen, without making the run depend on
  // catching a state whose whole life is the switch.
  const seen = []
  const tick = setInterval(() => {
    const t = smoke.text('.chat')
    if (t.indexOf('对话已压缩') >= 0) seen.push(t)
  }, 10)
  const childRow = await smoke.waitFor(
    () => (smoke.qa('.session-chain').length === 1 ? smoke.qa('.session-chain')[0] : null),
    '侧栏出现「压缩前」折叠入口', 30000)
  clearInterval(tick)
  smoke.log('压缩时对话里出现过的提示', seen.length ? '对话已压缩' : '(没抓到，压缩很快)')
  if (!childRow) {
    smoke.log('诊断：侧栏行', rows().map((r) => smoke.text(r)).join(' | '))
    return smoke.finish()
  }
  smoke.check('压缩后侧栏仍是一行对话（父会话折起来了）', rows().length === 1,
    rows().map((r) => smoke.text(r)).join(' | '))
  smoke.check('折叠入口报出压缩前的节数', smoke.text(childRow).indexOf('压缩前 1 节') >= 0, smoke.text(childRow))
  smoke.check('折叠入口的标题说明了它是什么', /会话/.test(childRow.getAttribute('title') || ''), childRow.getAttribute('title'))

  // The folded half is reachable: opening it shows the pre-compaction session, marked as such.
  childRow.click()
  const shown = await smoke.waitFor(() => (smoke.qa('.session.is-compacted').length === 1 ? smoke.qa('.session.is-compacted')[0] : null),
    '展开后出现「压缩前」的会话行', 5000)
  smoke.check('展开后能看到压缩前的会话', !!shown, shown ? smoke.text(shown) : '')
  smoke.check('压缩前的行标注了自己的身份', !!shown && smoke.text(shown).indexOf('压缩前') >= 0, shown ? smoke.text(shown) : '')
  smoke.check('展开不改变对话行数（只是多了一条子行）', rows().length === 2, rows().length + ' 行')

  // The meter: this is the assertion the scenario exists for.
  const after = ringPct()
  smoke.check('压缩后上下文环回落（不是压缩前的数字）', after !== null && before - after >= 5,
    `before=${before}% after=${after}%`)
  // …and the other per-session numbers: they belong to the NEW session, so they start empty
  // rather than carrying the parent's totals into a conversation that has just begun.
  smoke.log('压缩前后的用量行', `before="${usageBefore}" after="${usageRow()}"`)

  smoke.finish()
})()
