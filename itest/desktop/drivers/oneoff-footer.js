// oneoff-footer — the turn-level review entry, and the state machine it became.
//
// The chip used to start a review that streamed a whole turn into the conversation (and left
// that turn looking like one that had changed files). Now the review is a side-channel run:
// the chip goes 评审本轮改动 → 评审中… → 已评审 N 条 · 查看, where N is the backend's tally of
// ReportFinding calls, and 查看 opens the panel the run's process went to.
//
// The scope here is the file the turn wrote, so this scenario runs in a git repository with an
// uncommitted change — a scoped review refuses to start without a baseline.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '写一个 NOTES.md')
  smoke.click('.send-btn')

  // The chip only exists on a turn that changed something, so its arrival is also the proof
  // that the WriteFile round landed (a fragment diff on the card feeds the same aggregate).
  const before = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('评审本轮改动') >= 0) || null,
    'footer 出现「评审本轮改动」', 30000)
  if (!before) return smoke.finish()
  smoke.check('有改动的回合才有评审入口', true, smoke.qa('.diff-chip').map((b) => b.textContent).join(' | '))
  smoke.check('评审前没有已评审状态', !smoke.qa('.diff-chip').some((b) => b.textContent.indexOf('已评审') >= 0), '')

  // The chip's MIDDLE state — the one that was reported missing (「点击评审按钮后，立刻会变成
  // 已评审查看，但没法点击，应该是评审中」). Two things made it invisible: the pending state was
  // cleared as soon as the fork had been STARTED (while the record on disk already carried this
  // turn's id, so the chip jumped straight to 已评审 with the button disabled by the very run it
  // reported on), AND it is a short-lived state whose length is the run's — a few hundred ms under
  // the mock, seconds to minutes for a real review. So this does not wait for the state, it
  // SAMPLES the chip and asserts the sequence contains it: what is being pinned is that the chip
  // passes through 评审中…, and that it stays clickable there.
  const seen = []
  const tick = setInterval(() => {
    const chip = smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('评审') >= 0)
    if (!chip) return
    const text = smoke.text(chip)
    if (!seen.length || seen[seen.length - 1].text !== text) seen.push({ text, disabled: chip.disabled })
  }, 10)
  before.click()
  const after = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('已评审') >= 0) || null,
    'footer 变成「已评审 N 条 · 查看」', 30000)
  clearInterval(tick)
  const trail = seen.map((s) => s.text).join(' → ')

  const pending = seen.find((s) => s.text.indexOf('评审中') >= 0)
  smoke.check('评审期间 chip 经过「评审中…」（而不是直接跳到已评审）', !!pending, trail)
  smoke.check('「评审中…」可以点（去看过程）', !!pending && !pending.disabled,
    pending ? `disabled=${pending.disabled}` : '')
  smoke.check('评审结束后 chip 报出意见条数', !!after, after ? smoke.text(after) : trail)
  const done = seen.filter((s) => s.text.indexOf('已评审') >= 0).pop()
  smoke.check('「已评审 · 查看」可以点（会话忙也不禁用）', !!done && !done.disabled,
    done ? `disabled=${done.disabled}` : '')
  smoke.check('条数来自后端对 ReportFinding 的计数（mock 报了 1 条）',
    !!after && smoke.text(after).indexOf('1 条') >= 0, after ? smoke.text(after) : '')

  // The run's process is NOT in the conversation — that was the original complaint: a review
  // turn that looked like a turn which had changed files. The turn's OWN card stays (it is
  // the turn), so the assertion is about the review's: its ReportFinding call must not appear.
  const chat = smoke.text('.chat')
  smoke.check('对话里没有评审的回复', chat.indexOf('评审完成：1 条意见。') < 0, '')
  const names = smoke.qa('.chat .tool-name').map((e) => e.textContent)
  smoke.check('对话里没有评审的 ReportFinding 卡', !names.some((n) => n.indexOf('ReportFinding') >= 0), names.join('|'))
  smoke.check('对话里只有本轮自己的那张工具卡', names.length === 1, names.join('|'))

  // ...and the chip is now the way TO the run.
  if (!(await smoke.waitFor('.oneoff-panel', '评审开始即打开面板', 8000))) return smoke.finish()
  if (!smoke.click('.oneoff-toggle')) return smoke.fail('关掉面板', '按钮找不到')
  if (!(await smoke.waitFor(() => !smoke.q('.oneoff-panel'), '面板已收起', 5000))) return smoke.finish()
  after.click()
  const reopened = await smoke.waitFor('.oneoff-panel', '点「查看」重新打开面板', 5000)
  smoke.check('完成后 chip 变成回到面板的入口', !!reopened, '')

  // The panel opens on 过程 and is showing THIS review, with the reply the mock produced.
  const replay = await smoke.waitFor(
    () => smoke.text('.oneoff-body').indexOf('评审完成：1 条意见。') >= 0 ? smoke.text('.oneoff-body') : null,
    '面板回放这次评审', 8000)
  smoke.check('面板回放出这次评审的回复', !!replay, (replay || '').slice(0, 80))

  // P3: the 意见 pane pairs the run's OWN findings with the diff of the files it reviewed —
  // 「完整 diff」 lands there. This is the fix for "点完整 diff 什么也看不到".
  const diffChip = smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('完整 diff') >= 0)
  if (!diffChip) return smoke.fail('找到「完整 diff」入口', '按钮找不到')
  diffChip.click()
  if (!(await smoke.waitFor('.oneoff-tab.active', '面板切到意见区', 8000))) return smoke.finish()
  smoke.check('「完整 diff」切到意见 + diff 区', smoke.text('.oneoff-tab.active').indexOf('意见') >= 0, smoke.text('.oneoff-tab.active'))
  const pane = await smoke.waitFor(() => smoke.text('.oneoff-body').indexOf('NOTES.md') >= 0 ? smoke.text('.oneoff-body') : null,
    '意见区显示被评审的文件 diff', 10000)
  smoke.check('意见区里有被评审文件的 diff', !!pane && pane.indexOf('NOTES.md') >= 0, (pane || '').slice(0, 100))
  smoke.check('意见挂在 diff 行上（有 finding 行）', !!smoke.q('.oneoff-body .finding'), '')
  smoke.check('意见区报出条数', (pane || '').indexOf('评审意见 1 条') >= 0, '')

  // Re-running from the pane: the record carries the scope, so "review these files again" also
  // works from a historical entry. The mock's second review reports TWO findings, which is how
  // the chip proves it is showing the new run rather than the old one.
  const rerun = smoke.qa('.oneoff-body .diff-open').find((b) => b.textContent.indexOf('重新评审') >= 0)
  if (!rerun) return smoke.fail('意见区有「重新评审」按钮', '按钮找不到')
  rerun.click()
  const again = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('已评审 2 条') >= 0) || null,
    '重跑后 chip 报出新的条数', 30000)
  smoke.check('按同一范围重跑，chip 报出新条数（2 条）', !!again,
    again ? smoke.text(again) : smoke.qa('.diff-chip').map((b) => b.textContent).join(' | '))

  smoke.finish()
})()
