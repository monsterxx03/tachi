// bash-diff — 本轮改动**只有 Bash 动过文件**：footer 的数字、完整 diff、评审的 scope 三处都必须看到它。
//
// 这三处过去全部源自「工具调用的参数」：Edit/Write 的 old/new 片段。Bash 不在链上，所以这样一轮
// 在旧实现里连 chip 都不会出现（记录到的改动是 0），面板无从 diff，评审拿到的是空 scope。
// 现在它们都读这一轮自己的两棵树（`git diff <轮首> <轮末>`），于是「shell 写了什么」就是事实，
// 数字是真实 numstat，评审的 scope 里也有这两个文件名 —— Go 侧直接断言评审的 prompt。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '用 bash 写两个文件')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.msg-assistant', '这一轮回复出现', 30000))) return smoke.finish()
  await smoke.waitFor(() => !smoke.q('.stop-btn'), '这一轮结算', 10000)
  await smoke.sleep(300)

  // The chip EXISTS at all: a turn whose only changes came from a shell command declares
  // nothing in any tool call, and the old footer rendered no chip for it.
  const chip = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('files') >= 0) || null,
    'footer 出现改动 chip', 8000)
  smoke.check('只有 shell 改动的回合也有改动 chip', !!chip, chip ? smoke.text(chip) : '(没有 chip)')
  if (!chip) return smoke.finish()
  // The numbers are git's own (3 + 2 lines added), not a sum of fragments — there were none.
  smoke.check('数字是真实 numstat（2 个文件 +5）',
    smoke.text(chip).indexOf('2 files') >= 0 && smoke.text(chip).indexOf('+5') >= 0, smoke.text(chip))
  smoke.check('chip 标明来源，而不是笼统地报一个数',
    smoke.text(chip).indexOf('来自工具调用') < 0 && (chip.getAttribute('title') || '').indexOf('检查点') >= 0,
    chip.getAttribute('title') || '(没有 title)')

  // The chip's OWN click: this turn has no fragment diffs to unfold (no tool call declared a
  // change), so it opens the full diff — the only place these changes are readable. It used to
  // toggle the fold, which opened onto an empty list (a click that looked dead).
  chip.click()
  const viaChip = await smoke.waitFor('.viewer-overlay .viewer-doc.is-diff', '点数字 chip 打开完整 diff', 8000)
  smoke.check('只有 shell 改动时，chip 自己就是完整 diff 的入口', !!viaChip, '')
  smoke.key(document.body, 'Escape')
  if (!(await smoke.waitFor(() => !smoke.q('.viewer-overlay'), '浮层关掉（下面还要再开一次）', 3000))) return smoke.finish()

  // 「完整 diff」 reads the turn's own two trees, so both shell-written files are in it — with
  // real line numbers, which a fragment card never had.
  const diffChip = smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('完整 diff') >= 0)
  if (!diffChip) return smoke.fail('找到「完整 diff」入口', '按钮找不到')
  diffChip.click()
  if (!(await smoke.waitFor('.viewer-overlay .viewer-doc.is-diff', '「完整 diff」打开宽浮层', 8000))) return smoke.finish()
  const panelText = smoke.text('.viewer-doc.is-diff')
  smoke.check('面板里有 shell 写出的两个文件',
    panelText.indexOf('made.txt') >= 0 && panelText.indexOf('other.txt') >= 0, panelText.slice(0, 120))
  smoke.check('面板说明它对照的是这一轮的快照（不是 git HEAD）',
    smoke.text('.diff-panel-note').indexOf('快照对比') >= 0, smoke.text('.diff-panel-note'))
  smoke.check('带真实文件行号', smoke.qa('.viewer-doc.is-diff .diff-ln').length > 0,
    String(smoke.qa('.viewer-doc.is-diff .diff-ln').length))
  smoke.key(document.body, 'Escape')
  if (!(await smoke.waitFor(() => !smoke.q('.viewer-overlay'), '浮层关掉', 3000))) return smoke.finish()

  // 「评审本轮改动」: the scope is taken from the same trees, and the Go half asserts that the
  // reviewer's prompt names the two files and points at the frozen pair rather than HEAD.
  const reviewChip = smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('评审本轮改动') >= 0)
  if (!reviewChip) return smoke.fail('找到「评审本轮改动」入口', '按钮找不到')
  reviewChip.click()
  const reviewed = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('已评审') >= 0) || null,
    '评审跑完（chip 报出条数）', 30000)
  smoke.check('评审能对 shell 写出的文件跑起来', !!reviewed, reviewed ? smoke.text(reviewed) : '(chip 没变成已评审)')
  // The mock's finding is ON made.txt — a file only a shell command created, so a review that
  // could not see it could not have reported it.
  smoke.check('意见数来自对 shell 写出文件的评审（1 条）',
    !!reviewed && smoke.text(reviewed).indexOf('1 条') >= 0, reviewed ? smoke.text(reviewed) : '')

  smoke.finish()
})()
