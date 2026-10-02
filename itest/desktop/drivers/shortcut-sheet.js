// shortcut-sheet —— ⌘? 那张列表：标题、一张一张的键位、关闭键。
//
// 表越长越容易说谎：键位一路加进来（新建两个、删除一个……），到了一屏放不下的那天，"行能滚、而标题
// 和「关闭」不跟着滚走"就成了这张表唯一还要紧的布局事实 —— 滚没了的关闭键，是读者离开这里唯一要按
// 的东西。所以这里量的是几何：整个框在窗口里、行区自己有滚动条、滚到底之后最后一行看得见而关闭键
// 还在原处。顺带钉住新加的 ⌘⇧N / ⌘⇧⌫ 真的被写进了表里（键位与说明两处分开加，漏一处没人看得出来）。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  const viewport = { top: 0, bottom: window.innerHeight }
  const inside = (el) => {
    const r = el.getBoundingClientRect()
    return r.top >= viewport.top - 1 && r.bottom <= viewport.bottom + 1
  }
  const kbdLabels = () => smoke.qa('.shortcut-row kbd').map((k) => smoke.text(k))

  // ⌘? 打开（key 就是 '?'，见 App.tsx 的那一条分支）。
  smoke.key(null, '?', { metaKey: true, shiftKey: true })
  const box = await smoke.waitFor('.shortcuts-box', '快捷键表出现', 5000)
  if (!box) return smoke.finish()
  smoke.check('⌘? 打开快捷键表', smoke.text('.shortcuts-box .confirm-msg') === '快捷键',
    smoke.text('.shortcuts-box .confirm-msg'))

  const body = smoke.q('.shortcut-body')
  if (!body) return smoke.fail('表里的行有自己的滚动区', smoke.text('.shortcuts-box').slice(0, 120))
  const overflow = body.scrollHeight - body.clientHeight
  smoke.check('行区自己滚动（表比一屏长）', overflow > 40,
    '行区 ' + body.clientHeight + 'px / 内容 ' + body.scrollHeight + 'px')
  smoke.check('整个框仍在窗口里', inside(box),
    'top=' + Math.round(box.getBoundingClientRect().top) + ' bottom=' + Math.round(box.getBoundingClientRect().bottom) +
    ' 窗口高=' + window.innerHeight)

  const closeBtn = smoke.qa('.shortcuts-box .confirm-actions .btn')[0]
  if (!closeBtn) return smoke.fail('表底部有「关闭」按钮', smoke.allText('.shortcuts-box .btn').join('/'))
  smoke.check('关闭键在框里可见（不用滚动）', inside(closeBtn), smoke.text(closeBtn))

  // 新键位真的写进了表：⌘N / ⌘⇧N / ⌘⇧⌫ 各一行。
  const labels = kbdLabels()
  for (const want of ['⌘ N', '⌘ ⇧ N', '⌘ ⇧ ⌫']) {
    smoke.check('表里列着 ' + want, labels.indexOf(want) >= 0, labels.join(' | '))
  }

  // 滚到底：最后一行看得见，标题和关闭键一动不动（它们不在滚动区里）。
  const closeBefore = Math.round(closeBtn.getBoundingClientRect().top)
  body.scrollTop = body.scrollHeight
  await smoke.sleep(120)
  const lastRow = smoke.qa('.shortcut-row').slice(-1)[0]
  const visible = lastRow.getBoundingClientRect().bottom <= body.getBoundingClientRect().bottom + 1
  smoke.check('滚到底看得到最后一行', visible,
    smoke.text(lastRow.querySelector('span')) + ' · 底=' + Math.round(lastRow.getBoundingClientRect().bottom) +
    ' 行区底=' + Math.round(body.getBoundingClientRect().bottom))
  smoke.check('行区滚动不影响标题与关闭键',
    smoke.text('.shortcuts-box .confirm-msg') === '快捷键' &&
      Math.round(closeBtn.getBoundingClientRect().top) === closeBefore && inside(closeBtn),
    '关闭键 top ' + closeBefore + ' → ' + Math.round(closeBtn.getBoundingClientRect().top))

  smoke.key(null, 'Escape')
  const gone = await smoke.waitFor(() => (!smoke.q('.shortcuts-box') ? true : null), 'Esc 关掉快捷键表', 3000)
  smoke.check('Esc 关掉快捷键表', !!gone, '')

  smoke.finish()
})()
