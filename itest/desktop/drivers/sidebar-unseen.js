// sidebar-unseen — 一个回合在你看别处的时候跑完了，那一行要保持高亮，直到你看它为止；它所在的分组
// 还折着时，那句话落在表头上。
//
// 四件事，按它们发生的顺序：
//   * 它在跑，你在看它：行上有高亮 + 转圈；
//   * 切到一个新会话（无项目桶）——切到哪一组就只留那一组，所以项目那一组**自己**收起来，它从
//     「行上的标记」变成「表头上的标记」，这就是折叠那半的入口，不用驱动去折；
//   * 它跑完了，而你没在看它：**转圈没了，高亮还在**（这是这条场景存在的理由 —— 高亮比回合活得久，
//     因为「有事等你读」不会自己消失），表头上多出一枚「还没看」的计数；
//   * 点它（看它）：高亮与表头计数一起消失 —— 清零点只有一个，就是「它成为屏幕上那个会话」。
//
// 断言读的是类与计算样式（能不能看出「一眼」是观感问题，测不了；能让它成立的机制可以测）。
;(async () => {
  const PROJECT = 'smoke-unseen'
  if (!(await smoke.waitForSession())) return smoke.finish()

  const sessionId = () => { const el = smoke.q('.app'); return (el && el.getAttribute('data-session-id')) || '' }
  const headOf = (name) => smoke.qa('.proj-head').find((h) => smoke.text(h).indexOf(name) >= 0)
  const rowByTitle = (t) => smoke.qa('.session').find((s) => smoke.text(s.querySelector('.session-title')) === t)
  const titleOf = (el) => (el && el.getAttribute('title')) || ''
  const painted = (c) => !!c && c !== 'transparent' && c !== 'rgba(0, 0, 0, 0)'
  const isUnseen = (row) => !!row && row.classList.contains('is-unseen')

  smoke.type('.composer-input', '跑一轮长一点的')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.session.active .spin-dot', '在跑的会话那一行出现转圈', 30000))) return smoke.finish()
  const fixtureId = sessionId()
  const runningRow = rowByTitle('冒烟会话')
  smoke.check('它在跑：那一行有高亮 + 转圈',
    isUnseen(runningRow) && !!runningRow.querySelector('.spin-dot'),
    runningRow ? runningRow.className : '（找不到那一行）')

  // ── 切到一个新会话 ────────────────────────────────────────────────────────
  if (!smoke.click('.proj-head.is-loose .proj-add')) return smoke.fail('新建会话', '无项目桶的 ＋ 找不到')
  if (!(await smoke.waitFor(() => sessionId() !== fixtureId, '切到新会话', 10000))) return smoke.finish()
  if (!(await smoke.waitFor(() => !rowByTitle('冒烟会话'), '它所在的项目组自己收起来了'))) return smoke.finish()
  smoke.check('折叠之后表头说「在运行」（行不在了，话落到表头上）',
    !!smoke.q('.proj-head .proj-run'), smoke.text(headOf(PROJECT) || document.body).slice(0, 40))

  // ── 它跑完了，而你在别处 ─────────────────────────────────────────────────
  if (!(await smoke.waitGone('.proj-head .proj-run', '回合跑完，表头上的「在运行」消失', 60000))) return smoke.finish()
  const badge = await smoke.waitFor('.proj-head .proj-unseen', '表头出现「还没看」的计数', 5000)
  if (!badge) return smoke.finish()
  smoke.check('计数是 1', smoke.text(badge) === '1', smoke.text(badge))
  smoke.check('计数点出是哪个会话', titleOf(badge).indexOf('冒烟会话') >= 0, titleOf(badge))
  smoke.check('「还没看」不是「在等你」那枚徽章（两件事不能混成一个标记）',
    !smoke.q('.proj-head .proj-wait'), '')
  const others = smoke.qa('.proj-head').filter((h) => smoke.text(h).indexOf(PROJECT) < 0)
  smoke.check('别的分组没有这枚计数',
    others.length > 0 && others.every((h) => !h.querySelector('.proj-unseen')),
    others.map((h) => smoke.text(h).slice(0, 20)).join(' | '))

  // ── 展开：高亮还在，转圈不在 ─────────────────────────────────────────────
  const head = headOf(PROJECT)
  if (!head) return smoke.fail('项目的表头还在', '找不到 ' + PROJECT)
  smoke.click(head)
  const row = await smoke.waitFor(() => rowByTitle('冒烟会话'), '展开后那一行回到 DOM')
  if (!row) return smoke.finish()
  smoke.check('跑完之后那一行仍旧带高亮（高亮比回合活得久）', isUnseen(row), row.className)
  smoke.check('但转圈没了（在跑这件事已经结束了）', !row.querySelector('.spin-dot'), row.innerHTML.slice(0, 80))
  const rowStyle = getComputedStyle(row)
  smoke.check('高亮是真的画出来了（水洗 + 细导轨）',
    painted(rowStyle.backgroundColor) && painted(getComputedStyle(row, '::before').backgroundColor),
    rowStyle.backgroundColor + ' / ' + getComputedStyle(row, '::before').backgroundColor)

  // ── 看它：高亮与表头计数一起消失 ─────────────────────────────────────────
  if (!smoke.click(row)) return smoke.fail('点开那个会话', '行不可点')
  if (!(await smoke.waitFor(() => !isUnseen(rowByTitle('冒烟会话')), '看过之后高亮消失', 10000))) return smoke.finish()
  smoke.check('看过之后那一行不再说「还没看」', true)
  smoke.check('表头上的计数也跟着没了（同一件事的第二个说法）',
    !smoke.q('.proj-unseen'), '')
  smoke.check('那一轮的回复就在屏幕上（看过了，所以它不再是「还没看」）',
    smoke.text('.chat-content').indexOf('跑完了') >= 0, smoke.text('.chat-content').slice(-80))

  smoke.finish()
})()
