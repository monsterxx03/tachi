// oneoff-report — the 报告 pane over a report that did not exist when the pane was opened.
//
// A run's report PATH is recorded when the run STARTS (it is what the round's prompt tells the
// model to write), so the tab is there from the first second while the file itself only arrives
// at the END — the round's last act. A read taken in between used to be cached under the run's
// own key, which is the same key the code read as "already have it": the pane said
// 「报告是空的」 for a report that landed a moment later, and never looked again
// (「review 过后，点击报告页，是空的」). The mock holds its write back for reportWritePause so
// this window is TESTED rather than raced — with the pause removed the read happens after the
// write and nothing here is under test.
//
// Two facts are pinned, in this order: the pane says WHY it has nothing (a missing file is not
// an empty one), and it fills in by itself once the report lands — no second click, no ⟳.
// MARKER is what the mock writes into the report (scenarios.go's smokeReportMarker). It is
// spelled out on both sides on purpose: the two halves are separate programs, and the whole
// point of the marker is that nothing else in this run can produce it.
const MARKER = 'SMOKE-REPORT-MARKER'

;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  // Timing: the window this scenario needs is [run start, report written], and the driver has to
  // click inside it. These lines say WHEN each step happened, so a run that misses the window
  // reports a number instead of an impression.
  const t0 = Date.now()
  const el = () => (Date.now() - t0) + 'ms'

  // A typed /review: no diff chip, no turn — the command is the whole trigger.
  smoke.type('.composer-input', '/review')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.oneoff-panel', '面板随运行自动打开', 15000))) return smoke.finish()
  smoke.log('时序：面板出现', el())

  // The panel must be ON this run while it runs. It is opened by the run's start event, and the
  // record file lands a moment after that event — so a panel that only reads the list at that
  // instant shows 「这个会话还没有旁路运行（评审、提交）」 for the whole run (measured: the 报告 tab
  // used to arrive at the run's END, i.e. after the report was already written, which is also how
  // a read taken in the middle could be answered from a cache).
  const listedLive = await smoke.waitFor(
    () => smoke.qa('.oneoff-select option').find((o) => o.textContent.indexOf('评审') >= 0) || null,
    '运行期间切换器里就有这次评审', 8000)
  smoke.check('运行期间面板就跟着这次评审（不是等它跑完才列出来）', !!listedLive,
    smoke.allText('.oneoff-select option').join(' | ') || '(没有选项)')
  smoke.check('运行期间面板不说「还没有旁路运行」',
    smoke.text('.oneoff-panel').indexOf('还没有旁路运行') < 0, '')

  // The tab is there because the RECORD names the report path, not because the file exists.
  const reportTab = await smoke.waitFor(
    () => smoke.qa('.oneoff-tab').find((b) => b.textContent.indexOf('报告') >= 0) || null,
    '「报告」标签已出现（记录开跑时就写下了报告路径）', 20000)
  if (!reportTab) return smoke.fail('面板有「报告」标签', smoke.allText('.oneoff-tab').join('|'))
  smoke.log('时序：报告标签出现', el())
  reportTab.click()

  // While the round is still writing, the pane must say so. 「报告是空的」 over a file that has
  // not been written yet is the lie this scenario exists for; the note also has to carry the
  // path, because "which file were we pointed at" is the fact a reader needs here.
  const note = await smoke.waitFor(
    () => (smoke.text('.oneoff-body').indexOf('还没写出来') >= 0 ? smoke.text('.oneoff-body') : null),
    '报告页说明文件还没写出来', 6000)
  smoke.check('报告还没落盘时，面板给出的是原因，不是「报告是空的」',
    !!note && note.indexOf('报告是空的') < 0, (note || '(没出现)').slice(0, 160))
  smoke.check('原因里带着它去找的那个路径', !!note && note.indexOf('/reviews/') >= 0, '')
  smoke.check('此刻面板上没有报告正文（下面那条要靠它才说明问题）',
    smoke.text('.oneoff-body').indexOf(MARKER) < 0, '')

  // …and it fills in ON ITS OWN: the pane is showing while the run finishes, and the report is
  // read again when it lands. No click lands here — that is the difference between "the reader
  // can retry" and "the pane stops lying". Isolated control: with the list half above in place and
  // the OLD read (which stored the empty answer under the run's key) put back, this assertion is
  // red forever — the report is written and the pane never shows it.
  const filled = await smoke.waitFor(
    () => (smoke.text('.oneoff-body').indexOf(MARKER) >= 0 ? smoke.text('.oneoff-body') : null),
    '报告落盘后，打开着的报告页自己填上正文', 40000)
  smoke.check('报告一落盘，打开着的报告页就自己填上（空结果不该被当成读过了）',
    !!filled, (filled || '(始终没出现)').slice(0, 160))
  smoke.check('正文就是报告文件里的那份（SMOKE-REPORT-MARKER）',
    !!filled && filled.indexOf(MARKER) >= 0, '')
  smoke.check('填上的是正文，说明行被换掉了', !!filled && filled.indexOf('还没写出来') < 0, '')

  // The pane also reports where it read from, and that path is the file the Go half checks.
  smoke.check('报告页标出文件名', !!smoke.q('.oneoff-open-report'),
    smoke.text('.oneoff-meta'))

  smoke.finish()
})()
