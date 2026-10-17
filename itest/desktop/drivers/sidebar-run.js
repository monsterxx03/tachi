// sidebar-run — 一个会话还在跑，而它所在的分组被折起来时，表头要说出这件事。
//
// 折叠把行从 DOM 里拿走了，行上那只转圈的（`.spin-dot`）也随之消失 —— 于是表头得自己说
// （`.proj-run`：同一只转圈 + 多于一个时的数量）。这条 driver 钉三件事：
//   * 折叠后表头出现它，并指名是哪个会话在跑；
//   * 它与「在等你」是两件事：这一轮没人在等你，所以那枚琥珀色徽章不该出现（两个标记混成一个，
//     读者就没法分辨「要我做什么」和「它还在忙」）；
//   * 回合真的跑完之后标记自己消失 —— 那是稳态，可以直接等它不在。
//
// 回合由 fixture 里的 mock pause 撑着：太短的话，折叠与断言还没做完它就已经结束了。
;(async () => {
  const PROJECT = 'smoke-run'
  if (!(await smoke.waitForSession())) return smoke.finish()

  const headOf = (name) => smoke.qa('.proj-head').find((h) => smoke.text(h).indexOf(name) >= 0)
  const titleOf = (el) => (el && el.getAttribute('title')) || ''

  smoke.type('.composer-input', '跑一轮长一点的')
  smoke.click('.send-btn')

  // 前提：这一轮确实在跑（行上的转圈出来了），否则下面折起来之后既没有标记可言、也证明不了什么。
  if (!(await smoke.waitFor('.session.active .spin-dot', '在跑的会话那一行出现转圈', 30000))) return smoke.finish()

  // 这只转圈要「一眼看得出来」，那是机制问题不是观感问题，所以两件事在这一侧钉住：
  //   * 动的那部分是整整 3/4 圈（被 mask 成环的 conic 渐变），而不是一格细薄的弧 —— 弧长和对比度
  //     才是它在视野边缘还能被看见的原因；实心点或者 1/4 圈都是「凑近了才看得见」；
  //   * 它待在 meta 行自己的盒子里：一次运行开始/结束不能让这一行的高度变化，否则整列会话会跟着
  //     挪一下（下面在回合结束后复量同一个高度）。
  const dot = smoke.q('.session.active .spin-dot')
  const dotStyle = getComputedStyle(dot)
  const maskImage = dotStyle.maskImage || dotStyle.webkitMaskImage || ''
  smoke.check('转圈画成一个环（conic 渐变 + mask），不是一个实心点',
    /conic-gradient/.test(dotStyle.backgroundImage) && maskImage !== '',
    dotStyle.backgroundImage.slice(0, 52) + ' / mask=' + maskImage.slice(0, 34))
  smoke.check('它确实在转（动画与时长都对）',
    dotStyle.animationName === 'spin-dot' && parseFloat(dotStyle.animationDuration) > 0,
    dotStyle.animationName + ' ' + dotStyle.animationDuration)
  smoke.check('直径与环的粗细都是可读的那个量级',
    parseFloat(dotStyle.width) >= 14 && /calc\(100% - 3px\)/.test(maskImage),
    dotStyle.width + ' / ' + maskImage.slice(0, 40))
  // 当前会话那一行不叠「还没看过」那层水洗：它在跑，所以它自己也带着 is-unseen（还在跑就是
  // 「还没看」的一种），但屏幕上这一行不需要第二层水洗 —— 只会把「你在哪」弄糊。那层是给后台
  // 会话的，由 session-running 断言，那里屏幕上的会话是另一个。
  //
  // 断言的是「这层没画到它身上」，用 CSS 自己那个选择器来说：.session.is-unseen:not(.active)。
  // 拿 matches() 而不是复述底色像素值 —— 底色是设计的事，会变；而「未读的水洗排除当前行」是
  // 必须保持为真的那条。有人把 :not(.active) 去掉，这里就红。
  const activeRow = smoke.q('.session.active')
  const paintedUnseen = (row) => !!row && row.matches('.session.is-unseen:not(.active)')
  const otherUnseen = smoke.qa('.session.is-unseen:not(.active)')[0]
  smoke.check('当前会话那一行只留转圈，不叠「还没看过」那层水洗',
    !!activeRow && !paintedUnseen(activeRow)
      && (!otherUnseen || getComputedStyle(activeRow).backgroundColor !== getComputedStyle(otherUnseen).backgroundColor),
    'is-unseen=' + (activeRow && activeRow.classList.contains('is-unseen'))
      + ' · 命中未读水洗=' + paintedUnseen(activeRow)
      + ' · 底色=' + (activeRow && getComputedStyle(activeRow).backgroundColor))
  const runningRowHeight = smoke.q('.session.active').getBoundingClientRect().height

  // ── 折叠它所在的那一组：行离开 DOM，标记录到表头上 ────────────────────────
  const head = headOf(PROJECT)
  if (!head) return smoke.fail('项目的表头存在', '找不到 ' + PROJECT)
  smoke.click(head)
  if (!(await smoke.waitFor(() => !smoke.q('.session.active'), '折叠后当前会话的行离开 DOM'))) return smoke.finish()

  const badge = await smoke.waitFor('.proj-head .proj-run', '折叠的表头上出现「在跑」标记')
  if (!badge) return smoke.finish()
  smoke.check('表头标记用的就是行上那只转圈', !!badge.querySelector('.spin-dot'), badge.innerHTML.slice(0, 80))
  smoke.check('表头标记指名是哪个会话在跑', titleOf(badge).indexOf('冒烟会话') >= 0, titleOf(badge))
  smoke.check('一个会话在跑时不写数量（转圈已经说了「这里有」）', smoke.text(badge) === '', smoke.text(badge))
  smoke.check('在跑 ≠ 在等你：这一轮没有等待，所以没有那枚徽章',
    !smoke.q('.proj-head .proj-wait'), '')
  const others = smoke.qa('.proj-head').filter((h) => smoke.text(h).indexOf(PROJECT) < 0)
  smoke.check('别的分组没有这枚标记（它属于有成员在跑的那一组）',
    others.length > 0 && others.every((h) => !h.querySelector('.proj-run')),
    others.map((h) => smoke.text(h).slice(0, 20)).join(' | '))

  // ── 回合跑完：标记自己消失（表头是此刻唯一看得见它的地方，组还折着）────────
  if (!(await smoke.waitGone('.proj-head .proj-run', '回合跑完后表头标记消失', 60000))) return smoke.finish()
  smoke.check('回合结束后表头不再说有东西在跑', true)

  // 展开回去：行上也没有转圈了 —— 消失的是事实，不只是被折叠藏起来的那一份。
  const head2 = headOf(PROJECT)
  if (!head2) return smoke.fail('项目的表头还在', '找不到 ' + PROJECT)
  smoke.click(head2)
  if (!(await smoke.waitFor(() => smoke.q('.session.active'), '展开后那一行回到 DOM'))) return smoke.finish()
  smoke.check('展开之后行上也没有转圈', !smoke.q('.session.active .spin-dot'),
    (smoke.q('.session.active') || {}).textContent)
  smoke.check('这一轮的回复还在屏幕上（消失的是标记，不是那一轮）',
    smoke.text('.chat-content').indexOf('跑完了') >= 0, smoke.text('.chat-content').slice(-80))

  // 几何：转圈出现和消失都不许改变这一行的高度 —— 它只在 meta 行自己的盒子里，所以回合开始/结束
  // 不会让整列会话挪一下（这一条在转圈变大之后尤其容易破）。
  const idleRowHeight = smoke.q('.session.active').getBoundingClientRect().height
  smoke.check('转圈出现与消失都没有改变行高（列表不会跟着挪）',
    Math.abs(idleRowHeight - runningRowHeight) < 0.5,
    '运行中 ' + runningRowHeight.toFixed(1) + 'px / 结束后 ' + idleRowHeight.toFixed(1) + 'px')

  smoke.finish()
})()
