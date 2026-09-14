// frozen-panel — 「本轮改动」在**工作树里已经没有这些改动**之后，面板与评审仍然读同一份 diff。
//
// 评审现在读的是这一轮自己的两棵树（`git diff <轮首> <轮末>`），因为它要能评审已经被提交/已经被
// 删掉的改动 —— 而侧栏面板（意见 + diff）过去仍按「工作区 vs HEAD」取数：改动一旦不在工作树里，
// 面板就变成空态，所有意见都掉进「其它文件 / 不在本轮差异里」，评审刚读过的文件看起来像是它自己
// 认错了。这里让第 2 轮用 bash 把两个文件删掉，然后开第 1 轮的「完整 diff」与那一次评审的面板：
// 两边都必须还是第 1 轮的两棵树。Go 侧另断言评审 prompt 里点名了两个文件、且指向冻结的命令。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  const settle = async (label) => {
    await smoke.waitFor(() => !smoke.q('.stop-btn'), label, 15000)
    await smoke.sleep(300)
  }
  // The Nth assistant card in the CONVERSATION. The side panel replays messages of its own
  // (same classes), so every lookup here is scoped to `.chat` — and every chip lookup to the
  // card it belongs to: both turns have chips, and only turn 1's is the subject.
  const card = (idx) => smoke.qa('.chat .msg-assistant')[idx] || null
  const chipIn = (el, text) =>
    el ? Array.prototype.slice.call(el.querySelectorAll('.diff-chip')).find((b) => b.textContent.indexOf(text) >= 0) || null : null
  const chipText = (el) => (el ? el.textContent.replace(/\s+/g, ' ').trim() : '(没有卡片)')

  // Turn 1: only a shell command writes — no tool call declares a path, so this turn used to
  // render no chip at all.
  smoke.type('.composer-input', '用 bash 写两个文件')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.chat .msg-assistant', '两个文件写好了', 30000))) return smoke.finish()
  await settle('第 1 轮结算')

  // Turn 2: delete them. From here on the working tree has neither file, and nothing uncommitted.
  smoke.type('.composer-input', '把那两个文件删掉')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.chat .msg-assistant', '已经删掉了', 30000))) return smoke.finish()
  await settle('第 2 轮结算')

  smoke.check('两轮各有一张卡', smoke.qa('.chat .msg-assistant').length === 2,
    String(smoke.qa('.chat .msg-assistant').length))

  // ── Turn 1's 完整 diff: the files are gone from disk, the turn's change is not ────────
  const diffChip = chipIn(card(0), '完整 diff')
  if (!diffChip) return smoke.fail('第 1 轮卡片上有「完整 diff」入口', chipText(card(0)))
  diffChip.click()
  if (!(await smoke.waitFor('.viewer-overlay .viewer-doc.is-diff', '「完整 diff」打开宽浮层', 8000))) return smoke.finish()
  const frozen = await smoke.waitFor(
    () => (smoke.text('.viewer-doc.is-diff').indexOf('made.txt') >= 0 ? smoke.text('.viewer-doc.is-diff') : null),
    '浮层里是第 1 轮的两棵树（文件已经不在磁盘上）', 10000)
  smoke.check('被删掉的文件仍在这一轮的 diff 里', !!frozen && frozen.indexOf('other.txt') >= 0,
    (frozen || '(浮层里没有 made.txt)').slice(0, 140))
  smoke.check('对照的是这一轮的快照，不是 git HEAD',
    smoke.text('.diff-panel-note').indexOf('快照对比') >= 0, smoke.text('.diff-panel-note'))
  smoke.check('带真实文件行号', smoke.qa('.viewer-doc.is-diff .diff-ln').length > 0,
    String(smoke.qa('.viewer-doc.is-diff .diff-ln').length))
  // These two files are not on disk any more, so the frozen diff has to say it is not the disk —
  // otherwise 打开文件 hands the reader text that does not match the hunks above it.
  smoke.check('标记出「之后又改过」', smoke.text('.viewer-doc.is-diff').indexOf('之后又改过') >= 0, '')
  smoke.key(document.body, 'Escape')
  if (!(await smoke.waitFor(() => !smoke.q('.viewer-overlay'), '浮层关掉', 5000))) return smoke.finish()

  // ── The review: it starts, and the panel shows the diff the reviewer actually read ─────
  const reviewChip = chipIn(card(0), '评审本轮改动')
  if (!reviewChip) return smoke.fail('第 1 轮卡片上有「评审本轮改动」入口', chipText(card(0)))
  reviewChip.click()
  const reviewed = await smoke.waitFor(() => chipIn(card(0), '已评审'), '评审跑完（chip 报出条数）', 30000)
  smoke.check('改动不在工作树里也能评审（scope 来自检查点）', !!reviewed, reviewed ? chipText(reviewed) : chipText(card(0)))
  smoke.check('意见数来自对已删文件的评审（1 条）',
    !!reviewed && chipText(reviewed).indexOf('1 条') >= 0, reviewed ? chipText(reviewed) : '')
  if (!(await smoke.waitFor('.oneoff-panel', '评审开始即打开面板', 8000))) return smoke.finish()

  const findingsTab = smoke.qa('.oneoff-tab').find((b) => b.textContent.indexOf('意见') >= 0)
  if (!findingsTab) return smoke.fail('面板有「意见」标签', smoke.allText('.oneoff-tab').join('|'))
  findingsTab.click()
  if (!(await smoke.waitFor('.oneoff-tab.active', '面板切到意见区', 8000))) return smoke.finish()

  // The pane's diff is a round trip of its own, keyed by the run's recorded TURN. Without it
  // there is nothing here but an empty state.
  const groups = await smoke.waitFor(() => (smoke.qa('.diff-file').length >= 2 ? smoke.qa('.diff-file') : null),
    '意见区渲染出被评审文件的 diff 组', 15000)
  if (!groups) {
    smoke.log('诊断：意见区此刻的样子', smoke.text('.oneoff-body').slice(0, 200))
    return smoke.finish()
  }
  const made = groups.find((g) => smoke.text(g.querySelector('.diff-path')).indexOf('made.txt') >= 0)
  const pane = smoke.text('.oneoff-body')
  smoke.check('意见区有被删掉的 made.txt 的 diff', !!made, smoke.allText('.diff-file .diff-path').join(' | '))
  smoke.check('diff 带真实行号（不是只剩一条路径）', !!made && !!made.querySelector('.diff-ln'),
    made ? made.querySelectorAll('.diff-ln').length + ' 行' : '(没有这个文件组)')
  smoke.check('意见挂在这份 diff 的行上', !!made && !!made.querySelector('.finding'), '')
  // These two are the symptom this scenario exists for: reading the working tree put the
  // reviewer's own files in 「不在本轮差异里」 and left the pane empty.
  smoke.check('意见不再是「不在本轮差异里」', pane.indexOf('不在本轮差异里') < 0, pane.slice(0, 140))
  smoke.check('面板也没有回落到空态', pane.indexOf('没有未提交的改动') < 0, '')

  // 「重新评审」 has to carry the turn too: it used to hardcode turn 0, so after a commit or a
  // deletion every re-run was refused with 「工作树里已经没有未提交的差异」 — the one thing the
  // frozen pair makes possible was unreachable from the pane. The mock's second review reports
  // TWO findings, so the chip's number is the proof that a new run really happened.
  const rerun = smoke.qa('.oneoff-body .diff-open').find((b) => b.textContent.indexOf('重新评审') >= 0)
  if (!rerun) return smoke.fail('意见区有「重新评审」按钮', '按钮找不到')
  rerun.click()
  const again = await smoke.waitFor(() => chipIn(card(0), '已评审 2 条'), '重跑后 chip 报出新的条数', 30000)
  smoke.check('按同一轮重跑没有被拒绝（chip 报出新条数）', !!again, again ? chipText(again) : chipText(card(0)))

  smoke.finish()
})()
