// multi-root — 会话带一个 additional root 时，「本轮改动」必须说清每个文件属于哪棵树。
//
// 检查点对每个 root 各存一份，回退卡片一直就是按 root 分块的；但「本轮改动」这条链过去把
// 各 root 的 diff 文本拼成一份、文件只留相对路径，面板再用**主目录**把它解析成绝对路径 ——
// 于是第二个目录里的文件被解析到主目录的同名位置：预览/打开/发送都会是错的那个文件，而且
// 一声不响。两个 root 各有一个同名文件时连分组都分不开。
//
// 这里让主目录和 additional root 各有一个同名文件、同一轮各改一行：面板必须出现两个分组、
// 其中一个带 root 标签，Go 侧另断言评审 prompt 里两棵树都被点名（分组形式）。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '两个目录各改一行')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant', '两边都改好了', 30000))) return smoke.finish()
  await smoke.waitFor(() => !smoke.q('.stop-btn'), '这一轮结算', 15000)
  await smoke.sleep(300)

  const chip = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('files') >= 0) || null,
    'footer 出现改动 chip', 8000)
  smoke.check('两个 root 的改动都算进了这一轮', !!chip, chip ? smoke.text(chip) : '(没有 chip)')
  if (!chip) return smoke.finish()

  const diffChip = smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('完整 diff') >= 0)
  if (!diffChip) return smoke.fail('找到「完整 diff」入口', '按钮找不到')
  diffChip.click()
  if (!(await smoke.waitFor('.viewer-overlay .viewer-doc.is-diff', '「完整 diff」打开宽浮层', 8000))) return smoke.finish()

  // 两个同名文件 → 两个分组。这正是「只按 path 记身份」会塌掉的地方。
  const groups = await smoke.waitFor(() => (smoke.qa('.viewer-doc.is-diff .diff-file').length >= 2 ? smoke.qa('.viewer-doc.is-diff .diff-file') : null),
    '浮层里出现两个文件分组', 10000)
  if (!groups) {
    smoke.log('诊断：浮层此刻的内容', smoke.text('.viewer-doc.is-diff').slice(0, 200))
    return smoke.finish()
  }
  const paths = smoke.allText('.viewer-doc.is-diff .diff-path')
  smoke.check('同名文件各成一组（不是一组、也不是丢掉一个）',
    paths.length === 2 && paths.every((p) => p.indexOf('notes.md') >= 0), paths.join(' | '))

  // 关键的一条：其中一个分组必须标出它来自 additional root。没有它，读者无法知道哪一份是
  // 哪个目录的，而「预览/打开」用的正是这个归属。
  const pane = smoke.text('.viewer-doc.is-diff')
  smoke.check('additional root 的文件带上了 root 标签', pane.indexOf('shared-lib') >= 0,
    smoke.allText('.viewer-doc.is-diff .diff-badge').join(' | '))
  // 两边的行都在（内容不会被合并成一份）。
  smoke.check('两个 root 的改动都显示出来了',
    pane.indexOf('main-change') >= 0 && pane.indexOf('lib-change') >= 0, pane.slice(0, 160))

  // 折叠状态按 (root, path) 记：点一个分组的折叠按钮，不能把另一个也折了。
  const toggles = smoke.qa('.viewer-doc.is-diff .diff-file-toggle')
  if (toggles.length === 2) {
    toggles[0].click()
    await smoke.sleep(150)
    const folded = smoke.qa('.viewer-doc.is-diff .diff-file').filter((g) => !g.querySelector('.diff-line')).length
    smoke.check('折一个分组不会连带折掉另一个（身份含 root）', folded === 1, folded + ' 个被折叠')
  } else {
    smoke.check('两个分组各有一个折叠按钮', false, toggles.length + ' 个按钮')
  }
  smoke.key(document.body, 'Escape')
  if (!(await smoke.waitFor(() => !smoke.q('.viewer-overlay'), '浮层关掉', 5000))) return smoke.finish()

  // 评审：scope 必须按 root 分组，否则「notes.md」在两个目录里各指一个文件。
  const reviewChip = smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('评审本轮改动') >= 0)
  if (!reviewChip) return smoke.fail('找到「评审本轮改动」入口', '按钮找不到')
  reviewChip.click()
  const reviewed = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('已评审') >= 0) || null,
    '评审跑完（chip 报出条数）', 30000)
  smoke.check('多 root 的一轮能评审', !!reviewed, reviewed ? smoke.text(reviewed) : '(chip 没变成已评审)')

  smoke.finish()
})()
