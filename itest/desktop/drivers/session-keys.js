// session-keys —— 作用于「眼前那个会话」的键位：⌘N / ⌘⇧N（新建）与 ⌘⇧⌫（删除）。
//
// ⌘N 与 ⌘⇧N 只差一个 shift，差别却藏在**新会话落在哪个组**里：⌘N 把当前会话的 projectId 传下去，
// ⌘⇧N 传空。组是前端算出来的（sessionGroups 的纯 join），而"绑没绑项目"是磁盘上的事实 —— 所以
// 两边各验一半：driver 看新行落在哪个组、是不是 active、是不是滚到了眼前；Go 侧读 meta.json 数
// project_id（只数得出一个：fixture 那个），并且 ⌘⇧⌫ 删掉的正是 ⌘N 建的那个绑着项目的会话。
//
// 第二件事是「定位到新建的会话」：新建的行必须在视野里。这一条要有**前提**才有意义 —— 列表得先长到
// 要滚动，否则"行在视野里"是白给的。所以 driver 先用无项目那一组的 ＋ 把列表撑过一屏（那些会话全堆
// 在桶里），滚到底部（读者自己的滚动位置），然后 ⌘⇧N：新行落在桶的**头部**，远远在视野上方。
//
// 第三件事是**分组的聚焦规则**：屏幕上那个会话在哪个组，就只有那个组开着。这个场景从三个方向夹住它：
// 一个 ＋ 或 ⌘⇧N 建无项目会话会把项目组收起来、⌘N 把桶收起来，而 ⌘K / ⌘P 把读者送进一个**折着的**
// 组时，那个组必须自己打开 —— 而这正是「跨组行走」这件事在新规则下的样子：走过去 = 上一组收起来。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // 显示中的会话 id：标题栏不再写了之后，它是页面上唯一读得到的地方（挂在 app 根上）。
  const curID = () => (smoke.q('.app') ? smoke.q('.app').getAttribute('data-session-id') : '') || ''
  const nav = () => smoke.q('.session-list')
  // 两个组的组头与行。组头按 is-loose 分：项目的那一个和「无项目」桶的那一个。
  const projHead = () => smoke.q('.proj-head:not(.is-loose)')
  const looseHead = () => smoke.q('.proj-head.is-loose')
  const projRows = () => smoke.qa('.proj-rows:not(.is-loose) .session')
  const looseRows = () => smoke.qa('.proj-rows.is-loose .session')
  const rowOf = (id) => smoke.qa('.session').find((r) => r.getAttribute('data-sid') === id) || null
  const hasRow = (rows, id) => rows.some((r) => r.getAttribute('data-sid') === id)
  // 一组开着没有，只认 aria-expanded —— 折叠是 DOM 级的，行数在「折着的组」和「空组」上是同一个数。
  const isOpen = (head) => !!head && head.querySelector('.proj-toggle').getAttribute('aria-expanded') === 'true'
  // 组头自己报的成员数。它是**折叠**也读得到的事实，所以「新行有没有落进这一组」不必先把组打开。
  const groupCount = (head) => {
    const el = head && head.querySelector('.proj-count')
    return el ? parseInt(smoke.text(el).replace(/[^0-9]/g, ''), 10) : -1
  }
  // 展开一个组：读者在界面上就是点组头。这里只负责**打开** —— 别的组由聚焦规则自己收。
  const openGroup = async (head, label) => {
    if (!head) return false
    if (!isOpen(head)) smoke.click(head.querySelector('.proj-toggle'))
    return !!(await smoke.waitFor(() => (isOpen(head) ? true : null), label, 5000))
  }
  const focusedComposer = () => document.activeElement === smoke.q('.composer-input')
  const keyBoth = (k) => smoke.key(document.body, k, { metaKey: true, shiftKey: true })

  // ── 起手：当前会话是 fixture 的项目成员（app 打开列表里最新的那个） ──────────
  // 分组的聚焦规则从启动这一刻就成立：只有当前会话所在的那一组是开的。桶折着不是因为它是空的，
  // 而是因为读者不在里面。
  const head = await smoke.waitFor('.proj-head:not(.is-loose)', '侧栏出现项目组头', 10000)
  if (!head) return smoke.finish()
  if (!(await smoke.waitFor(() => (isOpen(projHead()) ? true : null), '起手项目组是展开的', 5000))) return smoke.finish()
  smoke.check('起手只开着当前会话所在的那一组（「无项目」折着）',
    isOpen(projHead()) && !isOpen(looseHead()),
    '项目=' + isOpen(projHead()) + ' · 桶=' + isOpen(looseHead()))
  smoke.check('起手：当前会话是项目里的那一行',
    hasRow(projRows(), curID()) && projRows().some((r) => r.classList.contains('active')), 'id=' + curID())
  const projCountAtBoot = groupCount(projHead())

  // 把列表撑过一屏。无项目那一组的 ＋ 建的就是无项目会话，它们全堆在那个组里 —— 组头在列表最
  // 下方，所以行越多，新建的那一行（组的头部）离视野越远。每一次 ＋ 也是一次「落到某个组」：
  // 桶自己打开，项目组收起来。
  //
  // 门槛是**新行的落点真的在视野之外**，不是"能滚一点点"：只滚得动 40px 的话，滚到底它照样在视野里
  // —— 那样这条场景对"没把行滚到眼前"是瞎的。
  const OVERFLOW_NEEDED = 300
  let clicked = 0
  while (clicked < 25 && nav().scrollHeight <= nav().clientHeight + OVERFLOW_NEEDED) {
    if (!smoke.click('.proj-head.is-loose .proj-add'))
      return smoke.fail('「无项目」组头上有 ＋', smoke.allText('.proj-head').join(' | '))
    const grown = await smoke.waitFor(
      () => (looseRows().length === clicked + 2 ? true : null), '第 ' + (clicked + 1) + ' 个会话出现', 8000)
    if (!grown) return smoke.finish()
    clicked++
  }
  smoke.check('建一个无项目会话：桶打开、项目组收起来（聚焦规则）',
    isOpen(looseHead()) && !isOpen(projHead()),
    '桶=' + isOpen(looseHead()) + ' · 项目=' + isOpen(projHead()))
  smoke.check('项目组的计数没被这些会话动过（它们不属于这个项目）',
    groupCount(projHead()) === projCountAtBoot,
    groupCount(projHead()) + ' vs ' + projCountAtBoot)
  const overflow = nav().scrollHeight - nav().clientHeight
  if (!smoke.check('列表已被撑到要滚动', overflow > 40, clicked + ' 次 ＋ · 可滚 ' + overflow + 'px'))
    return smoke.finish()

  // 读者的滚动位置：滚到底。
  nav().scrollTop = nav().scrollHeight

  // 前提：新行的落点此刻在视野之外。新行会插在「无项目」组的最前面，也就是组里第一行**现在**的
  // 位置（按下去之前就量得到）。这一条不成立（窗口太高、列表撑不够），下面就证明不了任何东西，
  // 所以它自己也是一项断言。
  const box = nav().getBoundingClientRect()
  const slot = looseRows()[0].getBoundingClientRect()
  const offScreen = (r, b) => r.bottom <= b.top + 1 || r.top >= b.bottom - 1
  if (!smoke.check('前提：⌘⇧N 新行的落点在视野之外',
    offScreen(slot, box), 'scrollTop=' + nav().scrollTop + ' · 落点距视野顶 ' + Math.round(slot.top - box.top) + 'px'))
    return smoke.finish()

  // ── ⌘⇧N：一定是无项目的新会话 ──────────────────────────────────────────────
  const before = curID()
  keyBoth('N')
  const fresh = await smoke.waitFor(() => (curID() && curID() !== before ? curID() : null), '⌘⇧N 建出新会话', 10000)
  if (!fresh) {
    smoke.check('⌘⇧N 建出了新会话', false, '当前会话还是 ' + before)
    return smoke.finish()
  }
  smoke.check('⌘⇧N 建出了新会话', true, fresh)
  smoke.check('新行落在「无项目」组里（不在项目组）', hasRow(looseRows(), fresh),
    '无项目 ' + looseRows().length + ' 行 · 项目 ' + projRows().length + ' 行')
  smoke.check('项目组的计数没有跟着多一', groupCount(projHead()) === projCountAtBoot,
    groupCount(projHead()) + ' vs ' + projCountAtBoot)
  const row = rowOf(fresh)
  smoke.check('新行是 active（不是底下多了一行、选中还停在原地）',
    !!row && row.classList.contains('active'),
    smoke.qa('.session.active').length + ' 行 active')

  // 「定位」：这一行本来在视野上方，新建之后必须在视野里（整行都在）。
  const at = rowOf(fresh)
  const rect = at ? at.getBoundingClientRect() : null
  smoke.check('新建的行被滚到了眼前', !!rect && rect.top >= box.top - 1 && rect.bottom <= box.bottom + 1,
    rect ? 'scrollTop=' + nav().scrollTop + ' · 行顶距视野 ' + Math.round(rect.top - box.top) +
      'px，行高 ' + Math.round(rect.height) : '行没了')
  const focused = await smoke.waitFor(() => (focusedComposer() ? true : null), '光标落在输入框', 5000)
  smoke.check('新会话建好之后可以直接开始输入', !!focused,
    document.activeElement ? String(document.activeElement.className) : '(没有焦点元素)')

  // ── ⌘N：还是「跟眼前这个一样」，所以先回到项目成员会话 ──────────────────────
  // 走回去 = 打开项目组 + 点那一行；点行本身就是一次切换，所以桶会跟着收起来 —— 下面先断言这一点。
  if (!(await openGroup(projHead(), '打开项目组'))) return smoke.finish()
  const backRow = projRows()[0]
  if (!backRow) return smoke.fail('项目组里有可点的行', smoke.allText('.proj-rows:not(.is-loose)').join(' | '))
  smoke.click(backRow)
  if (!(await smoke.waitFor(() => (hasRow(projRows(), curID()) ? true : null), '切回项目成员会话', 10000)))
    return smoke.finish()
  smoke.check('点项目里的行：「无项目」那一组自己收起来（聚焦规则）',
    isOpen(projHead()) && !isOpen(looseHead()),
    '项目=' + isOpen(projHead()) + ' · 桶=' + isOpen(looseHead()))

  const before2 = curID()
  smoke.key(document.body, 'N', { metaKey: true })
  const fresh2 = await smoke.waitFor(() => (curID() && curID() !== before2 ? curID() : null), '⌘N 建出新会话', 10000)
  if (!fresh2) {
    smoke.check('⌘N 建出了新会话', false, '当前会话还是 ' + before2)
    return smoke.finish()
  }
  smoke.check('⌘N 建的行落在项目组里（一个 shift 的差别）', hasRow(projRows(), fresh2),
    '项目 ' + projRows().length + ' 行 · 无项目 ' + looseRows().length + ' 行')
  smoke.check('项目组的计数多了一（新行真的进了这一组）',
    groupCount(projHead()) === projCountAtBoot + 1,
    groupCount(projHead()) + ' vs ' + (projCountAtBoot + 1))
  smoke.check('⌘N 之后也是只有项目组开着（桶收着）',
    isOpen(projHead()) && !isOpen(looseHead()),
    '项目=' + isOpen(projHead()) + ' · 桶=' + isOpen(looseHead()))
  const freshRow = rowOf(fresh2)
  smoke.check('⌘N 建的行是 active', !!freshRow && freshRow.classList.contains('active'),
    smoke.qa('.session.active').length + ' 行 active')

  // ── ⌘⇧⌫：删掉眼前这个会话，过确认框 ──────────────────────────────────────
  // 眼前这个是 ⌘N 刚建的（绑着项目）。先走一次取消：框开了、Esc 关了、什么都没少。
  const victim = curID()
  keyBoth('Backspace')
  const opened = await smoke.waitFor('.confirm-box', '⌘⇧⌫ 开出删除确认框', 5000)
  smoke.check('⌘⇧⌫ 开出的框问的是眼前这个会话',
    !!opened && smoke.text('.confirm-msg').indexOf('删除会话') >= 0,
    opened ? smoke.text('.confirm-msg') : '(没有框)')
  // 框必须自己拿着焦点：光标本来在输入框里，Enter 若留在那儿就是「发一条消息」。
  smoke.check('确认框自己拿着焦点（Enter 不会被输入框吃掉）',
    document.activeElement === opened,
    document.activeElement ? String(document.activeElement.className || document.activeElement.tagName) : '(没有焦点元素)')
  const rowsBefore = smoke.qa('.session').length
  smoke.key(opened || document.body, 'Escape')
  const closed = await smoke.waitFor(() => (!smoke.q('.confirm-box') ? true : null), 'Esc 关掉确认框', 4000)
  smoke.check('Esc 关闭确认框，会话没被删', !!closed && curID() === victim &&
    smoke.qa('.session').length === rowsBefore && !!rowOf(victim),
    '闭=' + !!closed + ' · 当前=' + curID() + ' · 行数=' + smoke.qa('.session').length + '/' + rowsBefore)

  // 再来一次，这次按 Enter。
  keyBoth('Backspace')
  const again2 = await smoke.waitFor('.confirm-box', '再一次开出删除确认框', 5000)
  if (!again2) return smoke.finish()
  smoke.key(again2, 'Enter')
  const gone = await smoke.waitFor(
    () => (!smoke.q('.confirm-box') && curID() && curID() !== victim ? true : null), 'Enter 确认后会话被删', 8000)
  smoke.check('Enter 确认后确认框关掉、当前会话换成别的', !!gone,
    '闭=' + !smoke.q('.confirm-box') + ' · 当前=' + curID())
  smoke.check('被删的那一行从侧栏消失', !rowOf(victim), rowOf(victim) ? '行还在' : '')
  smoke.check('删完只剩项目组开着（读者被交给的是组内的邻居）',
    isOpen(projHead()) && !isOpen(looseHead()),
    '项目=' + isOpen(projHead()) + ' · 桶=' + isOpen(looseHead()))

  // ── 换会话就要定位：⌘K / ⌘P 这两条键盘路 ──────────────────────────────────
  // 目标在**折着的那一组**里（这里就是：读者此刻在桶里翻，而目标在项目组）—— 判据是「选完之后目标
  // 那一组自己打开、行在视野里、而且是 active」。用 fixture 里唯一有标题的那个会话当目标：列表里其余
  // 的行都是这场跑出来的「未命名会话」，而 ⌘K 是按标题过滤的，选不中它们。
  const titled = smoke.qa('.session').find((r) => smoke.text(r.querySelector('.session-title')) === '冒烟会话')
  if (!titled) return smoke.fail('fixture 的项目成员行还在', smoke.allText('.session-title').join(' | '))
  const targetID = titled.getAttribute('data-sid')

  // 走到列表另一头：打开「无项目」组，点桶里最后一行 —— 那是一次切换，所以项目组会跟着收起来，
  // 目标那一行也就不在 DOM 里了。这既是前提，也是聚焦规则的第三个方向。
  const walkAway = async (label) => {
    if (!(await openGroup(looseHead(), '打开「无项目」组'))) return false
    const rows = looseRows()
    const far = rows[rows.length - 1]
    if (!far) return smoke.fail('无项目组里有一行可以点走', smoke.allText('.session-title').join(' | '))
    smoke.click(far)
    if (!(await smoke.waitFor(() => (curID() === far.getAttribute('data-sid') ? true : null), label, 8000)))
      return false
    nav().scrollTop = nav().scrollHeight
    return smoke.check('前提：目标那一组收起来了，它的行不在 DOM 里',
      !isOpen(projHead()) && !rowOf(targetID),
      '项目=' + isOpen(projHead()) + ' · 目标行=' + (rowOf(targetID) ? '还在' : '不在'))
  }
  if (!(await walkAway('先走到列表另一头（无项目组最后一行）'))) return smoke.finish()

  // ⌘K：按标题过滤，回车。
  smoke.key(document.body, 'k', { metaKey: true })
  if (!(await smoke.waitFor('.palette-box', '⌘K 浮层打开', 8000))) return smoke.finish()
  smoke.type('.palette-input', '冒烟')
  if (!(await smoke.waitFor(() => (smoke.qa('.palette-row').length === 1 ? true : null), '过滤到那一个会话', 4000)))
    return smoke.finish()
  smoke.key('.palette-input', 'Enter')
  const picked = await smoke.waitFor(() => (curID() === targetID ? true : null), '⌘K 回车切过去', 10000)
  smoke.check('⌘K 选的会话成为当前会话', !!picked, curID())
  const back = await smoke.waitFor(() => (smoke.q('.palette-box') ? null : true), '浮层关掉再量', 4000)
  smoke.check('⌘K 送进折着的那一组，那一组自己打开',
    !!back && isOpen(projHead()) && !isOpen(looseHead()),
    '项目=' + isOpen(projHead()) + ' · 桶=' + isOpen(looseHead()))
  const nowBox = nav().getBoundingClientRect()
  const nowRow = rowOf(targetID)
  const nowRect = nowRow ? nowRow.getBoundingClientRect() : null
  smoke.check('⌘K 切过去之后那一行在视野里、并且是 active',
    !!nowRect && nowRect.top >= nowBox.top - 1 && nowRect.bottom <= nowBox.bottom + 1 &&
      nowRow.classList.contains('active'),
    nowRect ? 'scrollTop=' + nav().scrollTop + ' · 目标距视野顶 ' + Math.round(nowRect.top - nowBox.top) + 'px' : '行没了')

  // ⌘P：同一个位置上的另一个浮层，选的是项目 → 项目里最近的会话，落点还是那一行。
  if (!(await walkAway('⌘P 之前再走到列表另一头'))) return smoke.finish()
  smoke.key(document.body, 'p', { metaKey: true })
  if (!(await smoke.waitFor('.palette-box', '⌘P 浮层打开', 8000))) return smoke.finish()
  smoke.key('.palette-input', 'Enter')
  const picked2 = await smoke.waitFor(() => (curID() === targetID ? true : null), '⌘P 回车进项目', 10000)
  smoke.check('⌘P 选的项目把人送回它最近的会话', !!picked2, curID())
  await smoke.waitFor(() => (smoke.q('.palette-box') ? null : true), '浮层关掉再量（⌘P）', 4000)
  smoke.check('⌘P 也把那一组打开、别的组收起来',
    isOpen(projHead()) && !isOpen(looseHead()),
    '项目=' + isOpen(projHead()) + ' · 桶=' + isOpen(looseHead()))
  const box3 = nav().getBoundingClientRect()
  const row3 = rowOf(targetID)
  const rect3 = row3 ? row3.getBoundingClientRect() : null
  smoke.check('⌘P 切过去之后那一行也在视野里', !!rect3 &&
    rect3.top >= box3.top - 1 && rect3.bottom <= box3.bottom + 1,
    rect3 ? 'scrollTop=' + nav().scrollTop + ' · 目标距视野顶 ' + Math.round(rect3.top - box3.top) + 'px' : '行没了')

  smoke.finish()
})()
