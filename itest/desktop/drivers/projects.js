// projects — 项目组、只读工作区面板,以及项目写操作之后已经打开的界面必须跟着变。
//
// 这个场景的 fixture 里,会话记录自己的 WorkingDir 是沙箱 work 目录(一个「旧快照」),
// 而项目的主目录是另一个目录:凡是界面上出现的路径,只要不是那个旧快照,就证明它是从
// **项目**解析出来的 —— 这正是 §2 的机制,也是「快照还在、项目却搬走了」时唯一能区分两者的
// 判据。改名与删除都走组头右键菜单(不需要 native picker),分别覆盖 §7.4 的刷新契约和
// §6.2 的 detach;prompt 那一半由 Go 侧在 LLM 边界断言。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  // ── 1. 侧栏按项目分组 ────────────────────────────────────────────────────
  const head = await smoke.waitFor('.proj-head', '侧栏出现项目组头', 10000)
  if (!head) return smoke.finish()
  const heads = smoke.qa('.proj-head')
  const headText = smoke.text(head)
  smoke.check('组头显示项目名', headText.indexOf('smoke-proj') >= 0, headText)
  smoke.check('组头显示成员数', headText.indexOf('(1)') >= 0, headText)

  // 启动的默认折叠：只有「启动时恢复的那个会话」所在的项目是展开的，其余（包括那个还没有会话的）
  // 一律折叠。项目一多，侧栏先给一排组头；而「活动行必须看得见」这条规矩在启动这一刻仍然成立，
  // 因为读者还没做过任何选择 —— 这是它唯一的例外，别的入口各自 reveal（见 App.tsx 的 revealProject）。
  const expandedNames = () => heads
    .filter((h) => h.querySelector('.proj-toggle') && h.querySelector('.proj-toggle').getAttribute('aria-expanded') === 'true')
    .map((h) => smoke.text(h.querySelector('.proj-name')))
  const bootOpen = expandedNames()
  smoke.check('启动时只展开一个项目', bootOpen.length === 1, bootOpen.join(' | ') || '(全都折叠了)')
  smoke.check('展开的就是当前会话所在的那一组（active 行在组里可见）',
    smoke.qa('.proj-rows .session.active').length === 1,
    bootOpen.join(' | ') + ' · 组内 active 行 ' + smoke.qa('.proj-rows .session.active').length)
  smoke.check('还没有会话的项目启动时也是折叠的',
    !bootOpen.some((n) => n.indexOf('smoke-empty') >= 0), bootOpen.join(' | '))

  // 折叠是 DOM 层面的：收起的那一组把自己的行整个摘掉，所以下面凡是要读组内内容的断言，先自己
  // 展开 —— 这正是读者在界面上的那一步（点组头），不是测试的捷径。
  const openGroup = async (name) => {
    const h = smoke.qa('.proj-head').find((x) => smoke.text(x).indexOf(name) >= 0)
    if (!h) return null
    const t = h.querySelector('.proj-toggle')
    if (t.getAttribute('aria-expanded') !== 'true') t.click()
    await smoke.waitFor(() => (t.getAttribute('aria-expanded') === 'true' ? true : null), '展开 ' + name, 4000)
    return h
  }
  if (!(await openGroup('smoke-proj'))) return smoke.finish()

  const memberRows = smoke.qa('.proj-rows .session')
  smoke.check('成员会话挂在项目组下', memberRows.length === 1, memberRows.length + ' 行')
  const sections = smoke.allText('.session-section')
  smoke.check('没有项目的会话单独成组', sections.some((t) => t.indexOf('无项目') >= 0), sections.join(' | ') || '(没有分组标题)')

  // 空项目(刚建出来、还没有会话的那个)必须看得见:它是用户按下「保存」之后唯一能确认
  // 「项目建好了」的地方,也是它自己的 ＋ / 右键菜单的唯一入口。曾经被分组规则滤掉,
  // 症状就是「新建了项目,左边什么都没有」。
  smoke.check('两个项目都占组头（含还没有会话的那个）', smoke.qa('.proj-head').length === 2,
    smoke.allText('.proj-head').join(' | '))
  const emptyHead = smoke.qa('.proj-head').find((h) => smoke.text(h).indexOf('smoke-empty') >= 0)
  smoke.check('空项目显示成员数 0', !!emptyHead && smoke.text(emptyHead).indexOf('(0)') >= 0,
    emptyHead ? smoke.text(emptyHead) : '(没有空项目的组头)')
  if (!(await openGroup('smoke-empty'))) return smoke.finish()
  smoke.check('空项目说明还没有会话', smoke.text('.proj-empty').indexOf('还没有会话') >= 0,
    smoke.text('.proj-empty') || '(没有提示)')

  // 新建会话按钮的位置与分量：它造出来的行属于「无项目」这一组，所以它长在项目组之后、无项目
  // 标题之前 —— 也就是它自己那一组的头上，而不是像以前那样挂在侧栏最顶上。顶上那个位置是全宽的
  // 着色药丸，比会话本身还抢眼，读起来像整页的主操作。
  const newBtn = smoke.q('.new-chat')
  const looseTitle = smoke.q('.session-section')
  if (!newBtn || !looseTitle) {
    return smoke.fail('找到新建会话按钮与无项目标题',
      '按钮=' + (newBtn ? '有' : '无') + ' 无项目标题=' + (looseTitle ? '有' : '无'))
  }
  smoke.check('新建会话在项目组之后', !!(smoke.qa('.proj-head').slice(-1)[0].compareDocumentPosition(newBtn)
    & Node.DOCUMENT_POSITION_FOLLOWING),
    smoke.allText('.session-list > *').slice(0, 6).join(' | '))
  smoke.check('新建会话在无项目标题之上', !!(newBtn.compareDocumentPosition(looseTitle)
    & Node.DOCUMENT_POSITION_FOLLOWING), smoke.text(looseTitle))
  smoke.check('新建会话在会话列表里（不再占侧栏顶部）', newBtn.closest('.session-list') !== null,
    newBtn.parentElement ? newBtn.parentElement.className : '(没有父节点)')

  // 分量：不是着色按钮，字号也比它造出来的行小 —— 安静到不再和会话本身抢注意力。
  const btnStyle = getComputedStyle(newBtn)
  smoke.check('新建会话没有 accent 底色', btnStyle.backgroundColor === 'rgba(0, 0, 0, 0)',
    'background=' + btnStyle.backgroundColor)
  const sessionSize = parseFloat(getComputedStyle(smoke.q('.session-title')).fontSize)
  const projSize = parseFloat(getComputedStyle(heads[0].querySelector('.proj-name')).fontSize)
  const btnSize = parseFloat(btnStyle.fontSize)
  smoke.check('新建会话字号比会话标题小', btnSize < sessionSize,
    btnSize + 'px vs ' + sessionSize + 'px')
  smoke.check('新建会话字号比项目名小', btnSize < projSize, btnSize + 'px vs ' + projSize + 'px')

  // ── 2. 进成员会话，chip 说的是项目而不是目录 ────────────────────────────
  // 显式点进去：fixture 里有两个会话，谁被启动时选中是 fixture 的顺序细节，不该成为断言的前提。
  const member = smoke.q('.proj-rows .session')
  if (!member) return smoke.fail('找到项目组里的成员行', '没有 .proj-rows .session')
  member.click()
  const chipProject = await smoke.waitFor(
    () => (smoke.text('.work-dir').indexOf('smoke-proj') >= 0 ? true : null),
    'chip 显示项目名', 10000)
  smoke.check('chip 显示项目名', !!chipProject, smoke.text('.work-dir'))
  smoke.check('chip 不是会话记录里的旧快照', smoke.text('.work-dir').indexOf('work') < 0, smoke.text('.work-dir'))

  // ── 2b. ⌘N：当前会话属于项目，就在项目里新建 ─────────────────────────────
  // ⌘N 的语义是「再来一个跟眼前这个一样的」。项目供的是工作区、附加根和技能，掉到项目外等于
  // 静默换掉一整套环境，所以判据是「新行落在项目组里 + 新会话是当前会话」——只数行数看不出它
  // 落在哪一组，而无项目组里也有行。
  const memberRowsBefore = smoke.qa('.proj-rows .session').length
  smoke.key(document.body, 'n', { metaKey: true })
  const grew = await smoke.waitFor(
    () => (smoke.qa('.proj-rows .session').length === memberRowsBefore + 1 ? true : null),
    '⌘N 之后项目组里多一行', 10000)
  smoke.check('⌘N 在项目内新建会话', !!grew,
    smoke.qa('.proj-rows .session').length + ' 行（项目内）/' + memberRowsBefore + ' 行（之前）')
  smoke.check('新会话是当前会话且属于项目',
    smoke.qa('.proj-rows .session.active').length === 1,
    smoke.qa('.proj-rows .session.active').length + ' 行 active')
  const chipAfterCmdN = await smoke.waitFor(
    () => (smoke.text('.work-dir').indexOf('smoke-proj') >= 0 ? true : null),
    '⌘N 之后 chip 还是项目', 8000)
  smoke.check('⌘N 之后 chip 还是项目（工作区没掉）', !!chipAfterCmdN, smoke.text('.work-dir'))

  // ── 2c. 置顶在项目内部生效，不跨组 ──────────────────────────────────────
  // 置顶的语义是「排到本组最前」：项目里的会话在项目里排，不会越到别的组头上，也不重排项目本身。
  // 此刻项目里有两行（fixture 的老成员 + 刚 ⌘N 建的新会话），无项目组里也有行 —— 所以两个方向都
  // 能断言：老成员升到项目组第一行，而侧栏第一行仍在项目组内部。
  const memberTitles = () => smoke.qa('.proj-rows .session').map((r) => smoke.text(r.querySelector('.session-title')))
  const beforePin = memberTitles()
  const pinTarget = smoke.qa('.proj-rows .session').find((r) => smoke.text(r.querySelector('.session-title')) === '冒烟会话')
  if (!pinTarget) return smoke.fail('找到项目里的老成员行', beforePin.join(' | '))
  smoke.check('置顶前老成员不在项目组第一行', beforePin[0] !== '冒烟会话', beforePin.join(' | '))
  pinTarget.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 60, clientY: 200 }))
  if (!(await smoke.waitFor('.ctx-menu', '成员行右键菜单（置顶）', 3000))) return smoke.finish()
  const pinItem = smoke.qa('.ctx-item').find((b) => smoke.text(b) === '置顶')
  if (!pinItem) return smoke.fail('点得到「置顶」', smoke.allText('.ctx-item').join(' / '))
  pinItem.click()
  const memberFirst = await smoke.waitFor(
    () => (memberTitles()[0] === '冒烟会话' ? memberTitles() : null), '置顶的成员升到项目组第一行', 5000)
  smoke.check('置顶的成员升到项目组第一行', !!memberFirst, (memberFirst || memberTitles()).join(' | '))
  const firstRow = smoke.q('.session-list .session')
  smoke.check('置顶没有跨组：侧栏第一行仍在项目组内',
    !!firstRow && !!firstRow.closest('.proj-rows'),
    firstRow ? (firstRow.closest('.proj-rows') ? '在项目组内' : '跑到了项目组外') : '(没有行)')
  smoke.check('置顶不重排项目（第一个组头还是 smoke-proj）',
    smoke.text(smoke.qa('.proj-head')[0]).indexOf('smoke-proj') >= 0,
    smoke.allText('.proj-head').join(' | '))
  const pinnedMember = smoke.qa('.proj-rows .session')[0]
  smoke.check('置顶标记落在项目内的那一行', !!pinnedMember && !!pinnedMember.querySelector('.session-pin'),
    pinnedMember ? pinnedMember.innerHTML.slice(0, 80) : '(没有行)')
  // 留着这个置顶跑到结尾：detach（第 6 步删项目）必须原样保留会话记录上的 pinned，Go 侧也断言
  // 它挂在被置顶的那条会话上。

  // ── 2d. 删除会话之后选中谁：组内下一条 / 不是当前就不动 / 组里最后一条落到上一条 ──
  // 四行（冒烟会话恒被置顶在第一行，⌘N 现造的没有置顶标记），一次覆盖三条规则 —— 每条都要让「错误
  // 实现会选中的那一条」与正确答案**不同**，否则断言是空的：
  //   ① 删掉一个「下面还有行」的当前会话 → 落到下面那一行（错误实现会落到上一条）；
  //   ② 删掉一个不是当前的行 → 选中一步都不动（错误实现会落到它下面那一行）；
  //   ③ 删掉组里最后一行的当前会话 → 落到上面那一行（错误实现会跨到别的组/回到默认）。
  // 行的标题三行都一样，身份只能靠**下标 + 置顶标记**认。
  const rowsNow = () => smoke.qa('.proj-rows .session')
  const activeAt = () => rowsNow().findIndex((r) => r.classList.contains('active'))
  smoke.key(document.body, 'n', { metaKey: true })
  smoke.key(document.body, 'n', { metaKey: true })
  const fourRows = await smoke.waitFor(
    () => (rowsNow().length === 4 ? rowsNow() : null), '⌘N 两次，项目里 4 行', 10000)
  if (!fourRows) return smoke.finish()
  smoke.check('起手：置顶的冒烟会话在第一行，active 是第二行（下面还有两行）',
    !!fourRows[0].querySelector('.session-pin') && activeAt() === 1,
    activeAt() + ' / ' + fourRows.map((r) => (r.querySelector('.session-pin') ? '别' : '·')).join(''))

  // 右键一行 → 删除 → 确认。和模拟器里读者的两步完全一样。
  const deleteRow = async (row, label) => {
    if (!row) return smoke.fail(label, '没有可删的行')
    row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 60, clientY: 200 }))
    if (!(await smoke.waitFor('.ctx-menu', label + '：行菜单', 4000))) return false
    const del = smoke.qa('.ctx-item').find((b) => b.textContent.trim() === '删除')
    if (!del) return smoke.fail(label + '：菜单里有「删除」', smoke.allText('.ctx-item').join(' | '))
    del.click()
    if (!(await smoke.waitFor('.confirm-box', label + '：删除确认框', 4000))) return false
    const yes = smoke.qa('.confirm-box .btn').find((b) => b.textContent.trim() === '删除')
    if (!yes) return smoke.fail(label + '：确认框里有「删除」', smoke.allText('.confirm-box .btn').join(' | '))
    yes.click()
    return true
  }

  // ① 先把 active 挪到「下面还有一行」的那一行（下标 2），再删它。
  rowsNow()[2].click()
  if (!(await smoke.waitFor(() => (activeAt() === 2 ? true : null), 'active 挪到下标 2', 10000))) return smoke.finish()
  if (!(await deleteRow(rowsNow()[2], '删除下标 2 那一行（下面还有一行）'))) return smoke.finish()
  const handedDown = await smoke.waitFor(
    () => (rowsNow().length === 3 && activeAt() === 2 ? rowsNow() : null),
    '删掉之后选中落到它下面那一行', 10000)
  smoke.check('删掉中间那行：选中落到组内下一条', !!handedDown,
    rowsNow().length + ' 行，active 下标 ' + activeAt())
  smoke.check('落下来的是原来下面那一行，不是被置顶的冒烟会话（否则就是「上一条」实现）',
    !!handedDown && !rowsNow()[2].querySelector('.session-pin'),
    !!handedDown ? (rowsNow()[2].querySelector('.session-pin') ? '带置顶标记' : '没有置顶标记') : '(没有行)')

  // ② 把自己挪到第一行（冒烟会话），再删**不是当前**的下标 1 —— 错误实现会选中它下面那一行。
  rowsNow()[0].click()
  if (!(await smoke.waitFor(() => (activeAt() === 0 ? true : null), 'active 挪到冒烟会话（第一行）', 10000))) return smoke.finish()
  if (!(await deleteRow(rowsNow()[1], '删除下标 1 那一行（不是当前会话）'))) return smoke.finish()
  const stayed = await smoke.waitFor(
    () => (rowsNow().length === 2 && activeAt() === 0 ? true : null),
    '删掉别的行之后当前会话没有换', 10000)
  smoke.check('删的不是当前会话：选中一步都没有移动', !!stayed,
    rowsNow().length + ' 行，active 下标 ' + activeAt())

  // ③ 挪到最后一行（下面没有行了）再删它 —— 只能落到上面那一行，也就是被置顶的冒烟会话。
  rowsNow()[1].click()
  if (!(await smoke.waitFor(() => (activeAt() === 1 ? true : null), 'active 挪到最后一行', 10000))) return smoke.finish()
  if (!(await deleteRow(rowsNow()[1], '删除组里最后一行'))) return smoke.finish()
  const handedUp = await smoke.waitFor(
    () => (rowsNow().length === 1 && !!rowsNow()[0].querySelector('.session-pin') ? rowsNow() : null),
    '删掉最后一行之后选中落到上一条', 10000)
  smoke.check('删掉组里最后一行：选中落到组内上一条（被置顶的冒烟会话）', !!handedUp,
    rowsNow().length + ' 行，第一行' + (rowsNow()[0] && rowsNow()[0].querySelector('.session-pin') ? '带' : '不带') + '置顶标记')
  smoke.check('组里只剩一行，且它是当前会话', !!handedUp && activeAt() === 0,
    rowsNow().length + ' 行，active 下标 ' + activeAt())
  smoke.check('跨组没有被动过：无项目那一组还在原处', !!smoke.q('.session-section'), smoke.text('.session-section'))

  // ── 3. 工作区面板只读,并指向项目 ────────────────────────────────────────
  smoke.click('.work-dir')
  if (!(await smoke.waitFor('.roots-panel', '工作区面板打开', 8000))) return smoke.finish()
  const paneText = smoke.text('.roots-panel')
  smoke.check('面板说明由项目管理', paneText.indexOf('由项目') >= 0, paneText.slice(0, 160))
  smoke.check('只读：没有「更换主目录」', smoke.qa('.roots-panel .roots-btn').every((b) => b.textContent.indexOf('更换') < 0),
    smoke.allText('.roots-panel .roots-btn').join(' | ') || '(没有按钮)')
  smoke.check('只读：没有「添加目录」', smoke.qa('.roots-panel .roots-add').every((b) => b.textContent.indexOf('添加目录') < 0),
    smoke.allText('.roots-panel .roots-add').join(' | ') || '(没有按钮)')
  smoke.check('面板给出两个出口', paneText.indexOf('编辑项目') >= 0 && paneText.indexOf('新建项目会话') >= 0, paneText.slice(0, 200))
  // 后端拒绝是权威的（desktop/roots.go 的 projectGuard），前端还要让「为什么不能点」一眼可见：
  // 面板里既没有编辑按钮，也解释了原因。
  smoke.key(document.body, 'Escape')
  await smoke.waitGone('.roots-panel', '面板关闭', 5000)

  // ── 4. 一轮对话（prompt 用的是项目主目录，由 Go 侧断言）─────────────────
  smoke.type('.composer-input', '打个招呼')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant', '收到', 30000))) return smoke.finish()
  await smoke.waitFor(() => !smoke.q('.stop-btn'), '这一轮结算', 15000)

  // ── 5. 组头右键菜单 ──────────────────────────────────────────────────────
  const openProjectMenu = async (label) => {
    const h = smoke.q('.proj-head')
    if (!h) {
      smoke.fail(label, '没有 .proj-head')
      return null
    }
    h.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 60, clientY: 200 }))
    return smoke.waitFor('.ctx-menu', label, 4000)
  }

  let menu = await openProjectMenu('项目组头右键菜单出现')
  if (!menu) return smoke.finish()
  const items = smoke.allText('.ctx-menu .ctx-item')
  smoke.check('菜单里有重命名 / 编辑目录 / 删除项目',
    items.some((t) => t.indexOf('重命名') >= 0) && items.some((t) => t.indexOf('编辑目录') >= 0) && items.some((t) => t.indexOf('删除项目') >= 0),
    items.join(' | '))

  // 重命名：组头与 chip 都要跟着变，且没有切过会话 —— 这就是 §7.4 的刷新契约。
  const renameItem = smoke.qa('.ctx-menu .ctx-item').find((b) => b.textContent.indexOf('重命名') >= 0)
  if (!renameItem) return smoke.fail('点得到重命名', items.join(' | '))
  renameItem.click()
  if (!(await smoke.waitFor('.project-rename', '组头进入重命名', 4000))) return smoke.finish()
  smoke.type('.project-rename', 'smoke-proj-renamed')
  smoke.key('.project-rename', 'Enter')
  const renamed = await smoke.waitFor(
    () => (smoke.text('.proj-head').indexOf('smoke-proj-renamed') >= 0 ? true : null),
    '组头显示新名字', 8000)
  smoke.check('组头跟着改名', !!renamed, smoke.text('.proj-head'))
  const chipRenamed = await smoke.waitFor(
    () => (smoke.text('.work-dir').indexOf('smoke-proj-renamed') >= 0 ? true : null),
    'chip 跟着改名（不切会话）', 8000)
  smoke.check('chip 跟着改名，无需切会话', !!chipRenamed, smoke.text('.work-dir'))

  // 重名要拒绝，且原因留在组头自己的输入框下面（关掉编辑框会让用户以为程序没反应）。
  menu = await openProjectMenu('再次打开项目菜单')
  if (!menu) return smoke.finish()
  const renameAgain = smoke.qa('.ctx-menu .ctx-item').find((b) => b.textContent.indexOf('重命名') >= 0)
  renameAgain.click()
  if (!(await smoke.waitFor('.project-rename', '组头再次进入重命名', 4000))) return smoke.finish()
  smoke.type('.project-rename', 'smoke-proj-renamed')
  smoke.key('.project-rename', 'Enter')
  await smoke.sleep(400)
  smoke.check('改成同名不算冲突（同名即无操作）', !smoke.q('.proj-notice'), smoke.text('.proj-notice') || '(没有提示)')

  // ── 6. 删除项目 = detach（§6.2）─────────────────────────────────────────
  menu = await openProjectMenu('删除前再打开菜单')
  if (!menu) return smoke.finish()
  const delItem = smoke.qa('.ctx-menu .ctx-item').find((b) => b.textContent.indexOf('删除项目') >= 0)
  if (!delItem) return smoke.fail('点得到删除项目', smoke.allText('.ctx-menu .ctx-item').join(' | '))
  delItem.click()
  if (!(await smoke.waitFor('.confirm-box', '删除确认框出现', 4000))) return smoke.finish()
  const boxText = smoke.text('.confirm-box')
  smoke.check('确认框说明成员会话会保留', boxText.indexOf('保留') >= 0, boxText.slice(0, 200))
  const confirmDel = smoke.qa('.confirm-box .btn').find((b) => b.textContent.indexOf('删除项目') >= 0)
  if (!confirmDel) return smoke.fail('确认框里有「删除项目」', smoke.allText('.confirm-box .btn').join(' | '))
  confirmDel.click()
  const leftHeads = await smoke.waitFor(
    () => (smoke.qa('.proj-head').length === 1 ? smoke.qa('.proj-head') : null),
    '被删项目的组头消失（另一个空项目还在）', 10000)
  smoke.check('删除后只剩那个还没会话的项目',
    !!leftHeads && smoke.text(leftHeads[0]).indexOf('smoke-empty') >= 0,
    smoke.allText('.proj-head').join(' | ') || '(没有组头)')
  // detach 之后会话是普通会话：chip 回到自己的目录（快照已刷成项目主目录）、面板重新可编辑。
  const chipDetached = await smoke.waitFor(
    () => (smoke.text('.work-dir').indexOf('project-root') >= 0 ? true : null),
    'chip 回到会话自己的目录', 10000)
  smoke.check('detach 后 chip 显示自己的目录', !!chipDetached, smoke.text('.work-dir'))
  smoke.click('.work-dir')
  if (!(await smoke.waitFor('.roots-panel', '面板再次打开', 8000))) return smoke.finish()
  const editable = smoke.qa('.roots-panel .roots-btn').some((b) => b.textContent.indexOf('更换') >= 0)
  smoke.check('detach 后面板可编辑（后端也放行）', editable, smoke.text('.roots-panel').slice(0, 160))

  smoke.finish()
})()
