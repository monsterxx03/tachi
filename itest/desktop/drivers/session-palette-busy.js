// session-palette-busy — ⌘K 把「在跑 / 在等你」的会话排在最前面。
//
// 这条规则要满足的是读者打开这个浮层最常见的那个理由：「刚才停下来等我的那次对话叫什么来着」。
// 他打不出那个标题（他根本不知道它叫什么），而「最近更新」也不会把它顶上来 —— 被 park 的那一轮
// 自 park 之后就没再写过，中途跑完的那些反而更新。
//
// 所以这条 fixture 让规则**可判**：停在授权上的会话（冒烟会话，也是启动时打开的那个）**不是**最近
// 更新的那个 —— 读者随后在无项目会话里真的发过一条、那条跑完了。于是「第一行是冒烟会话」只可能
// 来自新规则；Go 那一半读磁盘上的 updated_at 把这句话钉死（被 park 的确实更旧），否则这条断言在
// 旧实现下也照样绿。
//
// 同时钉住「为什么它在最前面」必须写在行上：排到最前的那一行带着侧栏那一对同样的标记（琥珀色的
// 在等你、在跑的转圈），另一行两个都没有 —— 一个读者解释不了的重排，读起来就是列表坏了。
;(async () => {
  if (!(await smoke.waitForSession())) return smoke.finish()

  const boxOpen = () => {
    const b = smoke.q('.palette-box')
    return !!b && b.getAttribute('aria-label') === '会话搜索'
  }
  const names = () => smoke.allText('.palette-name')
  const paletteRow = (i) => smoke.qa('.palette-row')[i]
  const rowOf = (title) => smoke.qa('.session').find((s) => smoke.text(s.querySelector('.session-title')) === title)
  const clickRow = (title) => { const r = rowOf(title); return r ? smoke.click(r) : false }
  const headOf = (name) => smoke.qa('.proj-head').find((h) => smoke.text(h).indexOf(name) >= 0)
  const expandedOf = (name) => {
    const h = headOf(name)
    const t = h && h.querySelector('.proj-toggle')
    return !!t && t.getAttribute('aria-expanded') === 'true'
  }
  const titleOf = (el) => (el && el.getAttribute('title')) || ''
  const rowHeights = () => smoke.qa('.palette-row').map((r) => Math.round(r.getBoundingClientRect().height))

  // ── 起手：冒烟会话跑一轮，停在权限授权上（在等你）────────────────────────────
  smoke.type('.composer-input', '读一下那个文件')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.perm-form', '权限确认卡出现', 30000))) return smoke.finish()
  const dot = await smoke.waitFor('.session.active .wait-dot', '在等你的那一行出现标记')
  if (!dot) return smoke.finish()
  smoke.check('停在授权上的会话，行上的标记说出在等什么',
    titleOf(dot).indexOf('等待你的授权') >= 0, titleOf(dot))
  smoke.check('它同时在跑（转圈与琥珀点是两件事）', !!smoke.q('.session.active .spin-dot'),
    'spin=' + !!smoke.q('.session.active .spin-dot'))

  // ── 切到无项目会话，在那里真的发一条 ──────────────────────────────────────
  // 折叠的组里那些行根本不在 DOM 里，所以先展开它：这是「先断言前提、再动作」的规矩（一个折着的
  // 组会让后面的行查找得到一个空数组，而那读起来跟「这个组驱动不了它的成员」一模一样）。
  if (expandedOf('无项目')) return smoke.fail('起手：读者不在桶那一组里（它该是折着的）', 'aria-expanded=true')
  const bucket = headOf('无项目')
  if (!bucket) return smoke.fail('「无项目」那一组的表头存在', smoke.allText('.proj-name').join(' | '))
  smoke.click(bucket)
  if (!(await smoke.waitFor(() => (rowOf('无项目会话') ? true : null), '展开后桶里的行出现', 8000))) return smoke.finish()
  if (!clickRow('无项目会话')) return smoke.fail('切到无项目会话', smoke.allText('.session-title').join(' | '))
  if (!(await smoke.waitFor(() => (smoke.text('.session.active .session-title') === '无项目会话' ? true : null),
    '切到无项目会话', 10000))) return smoke.finish()

  smoke.type('.composer-input', '在桶里说一句')
  smoke.click('.send-btn')
  // 等这一轮**结束**：那一行的转圈消失是一个稳态（回合跑完就不再回来），所以直接等它不在。
  // 它结束之后，这个会话就是磁盘上最近更新的那一个 —— 下面那条断言的分母。
  if (!(await smoke.waitGone('.session.active .spin-dot', '桶里那一轮跑完', 30000))) return smoke.finish()
  // 切走之后那个会话的行当然离开了屏幕（它那一组折起来了 —— 同时只开一组），但「它还在等你」
  // 换了地方说：折叠的组头带着那枚计数标记。停在授权上的那一轮没有被这次切换动过。
  const badge = await smoke.waitFor('.proj-head .proj-wait', '折起的那一组表头上出现等待标记')
  smoke.check('停着的那一轮不受影响（折叠的组头仍说它在等你）',
    !!badge && titleOf(badge).indexOf('冒烟会话') >= 0 && titleOf(badge).indexOf('等待你的授权') >= 0,
    titleOf(badge) || '(没有表头标记)')

  // ── ⌘K：第一行是那个在等你的会话，而不是最近更新的那个 ──────────────────────
  smoke.key(document.body, 'k', { metaKey: true })
  if (!(await smoke.waitFor(() => (boxOpen() ? true : null), '⌘K 打开会话搜索', 8000))) return smoke.finish()
  smoke.check('在等你的那个会话排第一（它并不是最近更新的那个）', names()[0] === '冒烟会话', names().join(' | '))
  smoke.check('最近更新的那个排第二', names()[1] === '无项目会话', names().join(' | '))

  const first = paletteRow(0)
  if (!first) return smoke.fail('第一行在 DOM 里', names().join(' | '))
  smoke.check('排到最前的那一行自己说出为什么：在等你的点 + 在跑的转圈',
    !!first.querySelector('.wait-dot') && !!first.querySelector('.spin-dot'),
    'wait=' + !!first.querySelector('.wait-dot') + ' spin=' + !!first.querySelector('.spin-dot'))
  smoke.check('在等什么写在那一枚标记上（与侧栏同一句话）',
    titleOf(first.querySelector('.wait-dot')).indexOf('等待你的授权') >= 0,
    titleOf(first.querySelector('.wait-dot')))
  const second = paletteRow(1)
  smoke.check('没有在等、也没有在跑的那一行两个标记都没有',
    !!second && !second.querySelector('.wait-dot') && !second.querySelector('.spin-dot'),
    second ? 'markers=' + (!!second.querySelector('.wait-dot')) + '/' + (!!second.querySelector('.spin-dot')) : '(缺第二行)')
  const heights = rowHeights()
  smoke.check('标记不改变行高（接下键盘走位时列表不会跳）', new Set(heights).size === 1, heights.join(' | '))

  // 过滤掉那个会话之后，剩下的仍然是列表自己的顺序 —— 「抬到最前面」是一次分区，不是把列表整个重排。
  smoke.type('.palette-input', '无项目')
  const filtered = await smoke.waitFor(() => (names().length === 1 ? true : null), '按标题过滤掉那个会话', 4000)
  smoke.check('过滤之后只剩另一个会话（顺序回到列表自己的顺序）',
    !!filtered && names()[0] === '无项目会话', names().join(' | '))

  smoke.finish()
})()
