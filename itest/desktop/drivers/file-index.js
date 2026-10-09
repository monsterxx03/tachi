// file-index — 左侧文件索引里「当前是哪个文件」。
//
// 这个场景存在的唯一理由：点**中间**那一条。跳转把目标文件的顶部停在 `scroll-margin-top` 那条线上
// （头部下方），而上一条的底边正好落在那条线的下面几像素 —— 用「谁的底边还在头栏下方」这种自己另立
// 一条线的判法，亮起来的就是上一条（读者报的原话就是「有时候会是上一条」）。两个文件的 fixture 抓不到
// 它：第二条就是最后一条，而「滚到底时最后一个胜出」的兜底会把答案修正回来，看着是对的。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  smoke.type('.composer-input', '写三个文件')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant', '三个文件都写好了', 30000))) return smoke.finish()
  await smoke.waitFor(() => !smoke.q('.stop-btn'), '这一轮结算', 15000)
  await smoke.sleep(300)

  const chip = await smoke.waitFor(
    () => smoke.menuItem(smoke.q('.chat .msg-assistant'), '完整 diff'),
    'footer 出现「完整 diff」入口', 8000)
  if (!chip) return smoke.finish()
  chip.click()
  if (!(await smoke.waitFor('.viewer-overlay .viewer-doc.is-diff', '「完整 diff」打开宽浮层', 8000))) return smoke.finish()

  const rows = await smoke.waitFor(
    () => (smoke.qa('.diff-filelist-row').length === 3 ? smoke.qa('.diff-filelist-row') : null),
    '索引里出现三行', 10000)
  if (!rows) return smoke.finish()
  smoke.check('索引三行，顺序与正文一致',
    smoke.text(rows[0]).indexOf('NOTES.md') >= 0
    && smoke.text(rows[1]).indexOf('OTHER.md') >= 0
    && smoke.text(rows[2]).indexOf('ZED.md') >= 0,
    smoke.allText('.diff-filelist-row').join(' | '))

  // 拖拽条要**能抓**整列的高度，而不是只有列表那么高（issue #31）：索引只有三行时，读者仍然要能在
  // 行下方的空白里抓住它。判据是 elementFromPoint —— 那正是"鼠标能不能落上去"的定义。
  //
  // 注意量的是**可抓区域**，不是条的盒子：条本身仍然只有列表那么高（它跟着列表走、药丸居中在列表
  // 上），延伸到整列的是它那个绝对定位的 ::before（见 chat.css）。所以这里不比较高度，而是在最后一行
  // 下面 40px / 120px 处各探一次。
  const resizer = smoke.q('.diff-filelist-resizer')
  const list = smoke.q('.diff-filelist')
  if (!resizer || !list) return smoke.fail('列表与拖拽条都在', '')
  const lBox = list.getBoundingClientRect()
  const rBox = resizer.getBoundingClientRect()
  smoke.log('索引与拖拽条', `列表 ${Math.round(lBox.height)}px / 条 ${Math.round(rBox.height)}px` +
    ` / 可抓高度 ${getComputedStyle(resizer).getPropertyValue('--diff-room-h').trim() || '(未测?)'}`)
  const probes = [40, 120].map((dy) => {
    const y = lBox.bottom + dy
    const hit = document.elementFromPoint(rBox.left + rBox.width / 2, y)
    const ok = !!hit && (hit === resizer || resizer.contains(hit))
    return { dy, ok, got: hit ? `${hit.tagName.toLowerCase()}.${hit.className}` : 'null' }
  })
  smoke.check('最后一行下面的空白处也抓得到拖拽条（40px / 120px 两处）',
    probes.every((p) => p.ok), probes.map((p) => `y+${p.dy}: ${p.got}`).join(' / '))

  const current = () => {
    const sel = smoke.qa('.diff-filelist-row.is-current')
    return sel.length === 1 ? smoke.text(sel[0]) : `(${sel.length} 行高亮)`
  }
  // 三条都点一遍，其中最要紧的是中间那条：它前面有文件（所以上一条的底边会落在落点线下方）、
  // 后面也有文件（所以「滚到底时最后一个胜出」的兜底救不了它）。
  for (const [i, name] of [[0, 'NOTES.md'], [1, 'OTHER.md'], [2, 'ZED.md']]) {
    smoke.click(smoke.qa('.diff-filelist-row')[i])
    const marked = await smoke.waitFor(
      () => { const c = current(); return c.indexOf(name) >= 0 ? c : null },
      `点第 ${i + 1} 行后它自己是当前文件`, 4000)
    smoke.check(`点「${name}」亮的就是它（不是上一条）`, !!marked, current())
  }

  // 滚动仍然要高亮说了算：滚回顶部，当前文件要回到第一个 —— 点击之外的那一半。
  const stage = smoke.q('.viewer-overlay .viewer-stage')
  if (stage) stage.scrollTop = 0
  const top = await smoke.waitFor(() => (current().indexOf('NOTES.md') >= 0 ? true : null), '滚回顶部后第一个文件是当前', 4000)
  smoke.check('滚动时高亮跟着走', !!top, current())

  smoke.finish()
})()
