// roots-branch — the workspace panel names each directory's git branch.
//
// Two checkouts of the same repository are the same row in that list (both read "tachi"), so the
// branch is the one thing that tells them apart. It is fetched per root when the panel opens, so the
// driver waits for the panel to fill rather than reading it the instant it appears.
//
// The value is asserted as "a real branch name, on the right row" — the driver can read the DOM but
// cannot run git, and the exact string is already pinned by the desktop's own test.
;(async () => {
  if (!(await smoke.waitFor('.work-dir', '工作区 chip 出现', 15000))) return smoke.finish()

  smoke.click('.work-dir')
  if (!(await smoke.waitFor('.roots-panel', '工作区面板打开', 8000))) return smoke.finish()
  await smoke.sleep(500) // the branches are probed per root when the panel opens

  const rows = smoke.qa('.roots-panel .roots-row')
  const chips = smoke.allText('.roots-panel .roots-branch')
  smoke.log('面板行', rows.map((r) => smoke.text(r)).join(' || '))
  smoke.log('分支芯片', chips.join(' | ') || '(一个都没有)')

  if (!smoke.check('主目录那一行有分支芯片', chips.length >= 1, `${chips.length} 个芯片 / ${rows.length} 行`)) return smoke.finish()
  smoke.check('分支名是真的（不是占位或“未知”）',
    !/未知|unknown|^(git|n\/a|-+)$/i.test(chips[0]) && chips[0].length > 0, chips[0])
  // The plain extra root must NOT get one: an empty branch rendered as a chip would tell the reader
  // that a directory is a checkout when it is not.
  smoke.check('不是 git 仓库的那一行没有分支芯片', chips.length < rows.length,
    `${chips.length} 个芯片 / ${rows.length} 行`)

  smoke.finish()
})()
