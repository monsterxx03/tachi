// file-tree — 右列的文件树 + 内容列的文件视图。
//
// 这里断言的都是「一条绑定之外」的东西：
//   · 树是从会话的工作区根长出来的 —— 主目录带 git 分支；两个**同名**附加根的标签退化成全路径
//     （标签是后端 rootLabels 的答案，前端自己算不出来）；主目录那一行没有标签。
//   · 一层是惰性列的：展开一个目录才多出一层。
//   · 被 git 忽略的文件照旧**显示**，只是灰掉 + 一枚「忽略」——树回答的是"磁盘上有什么"。
//   · 每一行的 git 状态：新文件标 U，原地被改的**已提交**文件标 M —— 后者所在目录的 mtime 一动
//     没动，所以它只可能来自那个按时轮询的状态读取，不是重新列目录的副产物。
//   · 折叠着的目录（pkg/，全程没被展开过）用一个点说出里面有未提交的改动 —— 展开着的那一层不再
//     重复标一次，因为它的行自己会说。
//   · 文件在**会话那一列**打开（文件视图），而树还在旁边：这正是它不复用旁路面板的地方。
//   · 会话在文件视图盖着时保持挂载（只是 inert），所以草稿还在、滚动没丢。
//   · agent 写完文件后树自己更新（轮询 / 一轮结束触发）。
//   · 关掉面板再打开：展开态与正在看的文件都还在（这条走的是内存那一半；盘上那一半由 fixture
//     的开局断言和场景 Go 侧对 desktop_ui.json 的读回各钉一次）。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // fixture 的 desktop_ui.json 写着：面板开着、宽度 360、docs/agents 展开、正在看 docs/agents/desktop.md。
  if (!(await smoke.waitFor('.filetree-panel', '文件树面板（记忆里是开着的）', 10000))) return smoke.finish()

  // 行按**名字**找，绝不按下标：这个列表的顺序是排序规则的结果，不是驱动可以假定的东西。
  const rowsNamed = (name) => smoke.qa('.filetree-row').filter((r) => {
    const n = r.querySelector('.filetree-name')
    return !!n && smoke.text(n) === name
  })
  const rowEl = (name) => rowsNamed(name)[0] || null
  const waitRow = (name, label, timeout) => smoke.waitFor(() => rowEl(name), label || ('出现行 ' + name), timeout || 8000)
  const labelOf = (row) => {
    const el = row.querySelector('.filetree-label')
    return el ? smoke.text(el) : ''
  }
  // 点一行：找不到就记一条失败，绝不去点"大概位置"的那一行。
  const clickRow = async (name) => {
    const row = await waitRow(name, '点一行 ' + name)
    if (!row) return false
    row.click()
    return true
  }

  // ── 1. 根：一个主目录 + 两个同名附加目录 ────────────────────────────────────
  const primary = await waitRow('work', '主目录那一行（work 目录）')
  if (!primary) return smoke.finish()
  smoke.check('主目录那一行没有根标签（标签为空就是它自己的名字）', labelOf(primary) === '', labelOf(primary) || '(没有标签)')
  const branch = primary.querySelector('.filetree-branch')
  smoke.check('主目录那一行带 git 分支', !!branch && smoke.text(branch).length > 0,
    branch ? smoke.text(branch) : '(没有分支芯片)')

  const libs = await smoke.waitFor(() => (rowsNamed('shared-lib').length === 2 ? rowsNamed('shared-lib') : null),
    '两个同名的附加根都出现', 8000)
  if (!libs) {
    smoke.log('诊断：此刻的行', smoke.allText('.filetree-row').join(' | ').slice(0, 300))
    return smoke.finish()
  }
  const libLabels = libs.map(labelOf)
  smoke.check('同名根的标签退化成全路径', libLabels.every((l) => l.indexOf('/') >= 0), libLabels.join(' | ') || '(没有标签)')

  // ── 2. 从盘上恢复的展开态与选中的文件 ──────────────────────────────────────
  const docs = await waitRow('docs', 'docs 行')
  const agents = await waitRow('agents', 'docs 的子目录（记忆里展开着）')
  smoke.check('展开态从 desktop_ui.json 恢复', !!docs && docs.getAttribute('aria-expanded') === 'true' && !!agents,
    docs ? 'aria-expanded=' + docs.getAttribute('aria-expanded') : '(docs 行不在)')
  const desktopMd = await waitRow('desktop.md', '记忆里正在看的文件')
  smoke.check('正在看的文件被选中（不是只把它列出来）',
    !!desktopMd && desktopMd.className.indexOf('is-selected') >= 0,
    desktopMd ? desktopMd.className : '(行不在)')
  smoke.check('宽度偏好从盘上恢复（360）', Math.abs(smoke.q('.filetree-panel').getBoundingClientRect().width - 360) < 2,
    String(smoke.q('.filetree-panel').getBoundingClientRect().width))
  // 注意：文件视图此刻**不**该出现 —— view 不进 uiState，启动一律落在会话上（设计 §5.3）。
  smoke.check('启动不看文件：view 从不落盘', !smoke.q('.file-view'), '面板开着，内容列还是会话')

  // ── 3. 忽略的文件照样显示 ─────────────────────────────────────────────────
  const dist = await waitRow('dist', '被忽略的 dist 行')
  smoke.check('被 git 忽略的行仍然在树里，只是灰掉',
    !!dist && dist.className.indexOf('is-ignored') >= 0, dist ? dist.className : '(行不在)')
  smoke.check('忽略的行说明被谁忽略', !!dist && smoke.text(dist).indexOf('忽略') >= 0,
    dist ? smoke.text(dist) : '(行不在)')
  if (dist) dist.click()
  smoke.check('展开一个目录才多出一层（惰性）', !!(await waitRow('app.js', 'dist 的子项', 6000)),
    'dist 展开后没有 app.js')

  // 草稿：文件视图盖住会话之前先打上，返回后它必须还在（会话是挂载着的，不是被卸载的）。
  smoke.type('.composer-input', '草稿还在吗')

  // ── 4. 文件在内容列打开，而树还在 ──────────────────────────────────────────
  if (!(await clickRow('main.go'))) return smoke.finish()
  if (!(await smoke.waitFor('.file-view', '内容列换成文件视图', 8000))) return smoke.finish()
  const codeText = await smoke.waitText('.file-view', 'package main', 8000)
  smoke.check('代码文件的内容显示出来了', !!codeText, smoke.text('.file-view').slice(0, 120))
  smoke.check('文件视图立着时树仍然在（这次改形态的原因）', !!smoke.q('.filetree-panel'), '')
  smoke.check('会话被 inert 而不是被卸载（草稿与滚动位置靠这个活着）',
    !!smoke.q('.chat') && smoke.q('.chat-covered')?.hasAttribute('inert') === true,
    'chat 在 DOM 里=' + !!smoke.q('.chat') + ' inert=' + smoke.q('.chat-covered')?.hasAttribute('inert'))
  // inert 子树不参与命中测试：套在文件视图外面的那一层要是也 inert，这个视图就变成"看得见、
  // 点不动、滚不动"——按钮全哑、文档也翻不了页。驱动点得动按钮是上一轮就证过的，这条把**机制**
  // 钉住（合成点击绕过命中测试，所以没有这条断言的话，那个 bug 在下一次运行里照样是绿的）。
  const inertAncestor = smoke.q('.file-view')?.closest('[inert]')
  smoke.check('文件视图自己不在 inert 子树里（在里面的东西点不了也滚不动）', !inertAncestor,
    inertAncestor ? 'inert 祖先是 ' + inertAncestor.className : '')
  // 主目录下的文件只显示相对路径：没有根标签，因为"这不是第二棵树"（附加根的文件才要带标签）。
  smoke.check('主目录下的文件只写相对路径', smoke.text('.fileview-path') === 'main.go',
    smoke.text('.fileview-path'))

  // ── 5. 一份比阅读区高的文档：正文那个盒子必须是滚动器 ─────────────────────
  if (!(await clickRow('long.md'))) return smoke.finish()
  // 先等**正文**渲染出来再量：空盒子（读取中）的 scrollHeight 就等于 clientHeight，量早了会得到
  // 一个看起来像"不能滚"的盒子，而那不是这条断言要问的问题。
  if (!(await smoke.waitText('.file-view', '第 60 节', 8000))) return smoke.finish()
  const body = smoke.q('.file-view .fileview-body')
  const taller = body.scrollHeight > body.clientHeight + 20
  smoke.check('内容比阅读区高时这个盒子会滚（overflow 与高度都成立）', taller,
    'scrollHeight=' + body.scrollHeight + ' clientHeight=' + body.clientHeight)
  body.scrollTop = 240
  // 等它真的滚过去，而不是 sleep 一下再读（读到的值要能说明这次赋值生效了）。
  smoke.check('它真的能滚到位（scrollTop 动了）',
    !!(await smoke.waitFor(() => (body.scrollTop > 0 ? body.scrollTop : null), '滚动位置生效', 3000)),
    'scrollTop=' + body.scrollTop)

  // ── 6. markdown、图片、二进制三种来源 ─────────────────────────────────────
  const mdRow = rowsNamed('desktop.md')[0]
  if (mdRow) mdRow.click()
  if (!(await smoke.waitText('.file-view', '桌面手册', 8000))) {
    smoke.log('诊断：切换后的文件视图', smoke.text('.file-view').slice(0, 120))
  } else {
    smoke.check('markdown 渲染成标题（不是原始文本）', !!smoke.q('.file-view h1'), smoke.text('.file-view h1'))
  }

  const pngRow = rowEl('pic.png')
  if (pngRow) pngRow.click()
  const img = await smoke.waitFor(() => smoke.q('.file-view .fileview-img'), '图片在文件视图里', 8000)
  smoke.check('图片走 /local（webview 直接取盘，不经过 IPC）',
    !!img && (img.getAttribute('src') || '').indexOf('/local') >= 0, img ? img.getAttribute('src') : '(没有 img)')

  const binRow = rowEl('blob.bin')
  if (binRow) binRow.click()
  const binNote = await smoke.waitText('.file-view', '不支持预览', 8000)
  smoke.check('二进制文件说清它不预览、并给出「打开」', !!binNote && !!smoke.q('.fileview-head .file-btn'),
    binNote ? smoke.text(binNote) : '(没有说明)')

  // ── 7. 返回会话（按钮与 Esc 各一次），草稿还在 ─────────────────────────────
  smoke.click('.fileview-back')
  if (!(await smoke.waitGone('.file-view', '文件视图收起', 6000))) return smoke.finish()
  smoke.check('返回后草稿还在（会话没有被卸载过）', smoke.q('.composer-input').value === '草稿还在吗',
    JSON.stringify(smoke.q('.composer-input').value))
  smoke.check('返回后会话不再是 inert', smoke.q('.chat-covered')?.hasAttribute('inert') !== true, '')

  const pngRow2 = rowEl('pic.png')
  if (pngRow2) pngRow2.click()
  if (!(await smoke.waitFor('.file-view', '再次打开文件视图', 6000))) return smoke.finish()
  smoke.key(document.body, 'Escape')
  const gone = await smoke.waitFor(() => !smoke.q('.file-view') || null, 'Esc 回到会话', 6000)
  smoke.check('Esc 也回会话（这个视图自带两条出路）', !!gone, gone ? '' : 'Esc 之后文件视图还在')

  // ── 8. agent 写完文件，树自己更新 ──────────────────────────────────────────
  smoke.type('.composer-input', '写个文件到 docs')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant', '写好了', 30000))) return smoke.finish()
  await smoke.waitFor(() => !smoke.q('.stop-btn'), '这一轮结算', 15000)
  const fresh = await waitRow('agent-notes.md', 'agent 写完的文件出现在树里（自动刷新）', 15000)
  smoke.check('agent 改完文件，树自己长出了新的一行', !!fresh, fresh ? '' : '等不到 docs/agent-notes.md')

  // ── 8b. 每一行的 git 状态 ──────────────────────────────────────────────────
  // 新文件是未跟踪的（U）。文件新增确实会动 docs 的 mtime，所以它证明的是「状态真的被画出来了」，
  // 而不是它自己的时机；不过行和它的 mark 走的是两条时钟（listing 与状态各 2s 一轮），所以这里
  // 等的是**mark 出现**这件事，不是「行刚出现时它就在」。第一条（M）才是时机的断言。
  const gitBadge = (name) => {
    const row = rowEl(name)
    const el = row && row.querySelector('.filetree-git')
    return el ? { code: smoke.text(el), title: el.getAttribute('title') || '' } : null
  }
  const freshGit = await smoke.waitFor(() => {
    const g = gitBadge('agent-notes.md')
    return g && g.code === 'U' ? g : null
  }, 'agent 写的新文件标着 U（未跟踪）', 15000)
  smoke.check('agent 写的新文件标着 U（未跟踪）',
    !!freshGit && freshGit.title.indexOf('未跟踪') >= 0,
    freshGit ? freshGit.code + ' / ' + freshGit.title : '(没有状态标记)')
  // 原地修改一个**已提交**的文件（main.go）：目录的 mtime 一点没动，所以这条只能由按时轮询的
  // 状态读取带来 —— 行上的 mark 会变，而那一层从来没被重新列过。这正是状态不挂在 listing 上的
  // 理由，也是这条断言唯一能看见的东西。
  const mainGit = await smoke.waitFor(() => {
    const g = gitBadge('main.go')
    return g && g.code === 'M' ? g : null
  }, 'main.go 原地修改后带上 M（不需要重新列目录）', 15000)
  smoke.check('原地修改的已提交文件带上 M，且说明是已修改',
    !!mainGit && mainGit.title.indexOf('已修改') >= 0,
    mainGit ? mainGit.code + ' / ' + mainGit.title : '(没有状态标记)')

  // 折叠着的目录自己说它藏着什么：pkg/ 全程没被展开过，所以那条 mark 是它唯一能说的话 ——
  // 而「一个目录折着就什么都看不见」正是这个功能最开始缺的那一半。
  const pkgDot = await smoke.waitFor(() => {
    const row = rowEl('pkg')
    const dot = row && row.querySelector('.filetree-git.is-dir')
    const title = dot ? dot.getAttribute('title') || '' : ''
    return dot && title.indexOf('已修改') >= 0 ? { title } : null
  }, '折叠的 pkg 目录标出里面有未提交的改动', 15000)
  smoke.check('折叠的目录标出里面有未提交的改动，并说明是什么',
    !!pkgDot, pkgDot ? pkgDot.title : '(没有聚合标记)')
  // 展开着的那一层不重复标一次：下面的行自己会说（与侧栏折叠分组同一条规则）。
  const docsRow = rowEl('docs')
  smoke.check('展开着的目录不重复标（行自己会说）',
    !(docsRow && docsRow.querySelector('.filetree-git')), docsRow ? docsRow.className : '(docs 行不在)')

  // ── 8c. 只看改动：这一列变成工作树的改动清单，点开的文件是 diff ──────────────
  // 清单不是"把树过滤一下"：它的行是从**状态表的路径**长出来的（后端答的是整个工作区），
  // 所以一个从没被展开过的目录（pkg/）也在里面，而且它的目录行是展开的。
  const changesToggle = smoke.q('.filetree-changes-toggle')
  smoke.check('文件树头部有「只看改动」开关', !!changesToggle, changesToggle ? '在' : '(没有按钮)')
  if (!changesToggle) return smoke.finish()
  changesToggle.click()
  const namesNow = () => smoke.qa('.filetree-row .filetree-name').map((n) => smoke.text(n))
  const changeList = await smoke.waitFor(() => {
    const names = namesNow()
    return names.indexOf('agent-notes.md') >= 0 && names.indexOf('README.md') < 0 ? names : null
  }, '只看改动：只剩工作树有改动的文件', 10000)
  smoke.check('开关把状态画出来（aria-pressed + 高亮）',
    smoke.q('.filetree-changes-toggle').getAttribute('aria-pressed') === 'true' &&
    smoke.q('.filetree-changes-toggle').className.indexOf('is-on') >= 0,
    smoke.q('.filetree-changes-toggle').className)
  smoke.check('只看改动时未改动的文件从树里消失了', !!changeList,
    changeList ? changeList.join(' | ') : '此刻的行：' + namesNow().join(' | '))
  smoke.check('未跟踪的新文件也在清单里（不只是已跟踪的改动）',
    !!changeList && changeList.indexOf('agent-notes.md') >= 0, changeList ? changeList.join(' | ') : '')
  smoke.check('折叠着的目录里的改动也在清单里（pkg/util.go）',
    !!changeList && changeList.indexOf('util.go') >= 0, changeList ? changeList.join(' | ') : '')

  if (!(await clickRow('main.go'))) return smoke.finish()
  if (!(await smoke.waitFor('.file-view', '内容列打开', 8000))) return smoke.finish()
  const diffShown = await smoke.waitFor(() => {
    const added = smoke.qa('.file-view .diff-line.is-add')
    if (!smoke.q('.file-view .fileview-diff') || !added.length) return null
    // 一行是"行号 + 符号 + 文本"三块，所以拼起来整份去找那一句，别只看第一行的开头。
    return { text: smoke.allText('.file-view .diff-line.is-add').join(' ') }
  }, '改动清单里点开一个文件，内容列显示的是 diff', 10000)
  smoke.check('点开有改动的文件直接显示 diff，不显示预览', !!diffShown && !smoke.q('.file-view .fileview-doc'),
    diffShown ? diffShown.text : smoke.text('.file-view').slice(0, 120))
  smoke.check('diff 里是这次真正改的那一行', !!diffShown && diffShown.text.indexOf('from the agent') >= 0,
    diffShown ? diffShown.text : '(没有新增行)')
  smoke.check('它不是浮层：diff 就在内容列里，树还在旁边',
    !!smoke.q('.file-view .fileview-diff') && !!smoke.q('.filetree-panel'), '')
  smoke.check('diff 说清它和谁比', smoke.text('.fileview-diff-head').indexOf('git HEAD') >= 0,
    smoke.text('.fileview-diff-head'))

  // 同一条改动两种读法：上下（逐行对照）与左右（旧的一列、新的一列）。开关与评审面板是同一份偏好
  // （setDiffSplit），所以这里断言的是"这个界面也能切"，不是"它自己有一套"。
  const diffBody = () => smoke.q('.file-view .fileview-diff .diff-body')
  smoke.check('diff 默认是上下（逐行对照）', !!diffBody() && diffBody().className.indexOf('is-split') < 0,
    diffBody() ? diffBody().className : '(没有 diff body)')
  const splitBtn = smoke.qa('.fileview-diff .diff-layout-btn').filter((b) => smoke.text(b) === '左右')[0]
  smoke.check('文件视图的 diff 头上有布局开关', !!splitBtn, splitBtn ? '' : '(没有布局按钮)')
  if (splitBtn) splitBtn.click()
  const splitOn = await smoke.waitFor(() => {
    const body = diffBody()
    return body && body.className.indexOf('is-split') >= 0 && smoke.q('.fileview-diff .diff-split-row') ? true : null
  }, '切成左右（两列对齐）', 4000)
  smoke.check('能切到左右：两列并排，一行一行对齐', !!splitOn,
    diffBody() ? diffBody().className : '(没有 diff body)')
  const unifiedBtn = smoke.qa('.fileview-diff .diff-layout-btn').filter((b) => smoke.text(b) === '上下')[0]
  if (unifiedBtn) unifiedBtn.click()
  const backUnified = await smoke.waitFor(() => {
    const body = diffBody()
    return body && body.className.indexOf('is-split') < 0 ? true : null
  }, '切回上下', 4000)
  smoke.check('能切回上下', !!backUnified, diffBody() ? diffBody().className : '(没有 diff body)')

  // 反过来也要有出路：diff 是一条关于"此刻"的话，文件本身仍然是可读的。
  const previewBtn = smoke.qa('.fileview-head .file-btn').filter((b) => smoke.text(b) === '预览')[0]
  if (previewBtn) previewBtn.click()
  const previewDoc = await smoke.waitFor(() => (smoke.q('.file-view .fileview-doc') ? true : null),
    'diff 头部切回文件本身', 8000)
  smoke.check('diff 头部能切回文件本身（预览）', !!previewDoc && !smoke.q('.file-view .fileview-diff'),
    previewDoc ? '' : smoke.text('.file-view').slice(0, 120))

  smoke.click('.fileview-back')
  await smoke.waitGone('.file-view', '回到会话', 6000)
  smoke.q('.filetree-changes-toggle').click()
  const backToListing = await smoke.waitFor(() => (namesNow().indexOf('README.md') >= 0 ? true : null),
    '关掉开关后完整目录回来', 8000)
  smoke.check('关掉开关后完整目录回来（未改动的文件重新出现）', !!backToListing, namesNow().join(' | '))

  // ── 9. 关掉面板再打开：展开态与选中的文件都还在 ────────────────────────────
  smoke.key(document.body, 'e', { metaKey: true })
  if (!(await smoke.waitGone('.filetree-panel', '面板收起', 6000))) return smoke.finish()
  smoke.key(document.body, 'e', { metaKey: true })
  if (!(await smoke.waitFor('.filetree-panel', '面板重新打开', 6000))) return smoke.finish()
  const docsAgain = await waitRow('docs', '重开后 docs 还在', 8000)
  smoke.check('重开面板后展开态还在', !!docsAgain && docsAgain.getAttribute('aria-expanded') === 'true',
    docsAgain ? 'aria-expanded=' + docsAgain.getAttribute('aria-expanded') : '(docs 行不在)')
  smoke.check('重开面板后 dist 的展开态也在（不止记忆里的那一层）', !!(await waitRow('app.js', '重开后 app.js 还在', 6000)), '')

  // 最后一步有副作用要落地：memo 的写入是去抖的（500ms），驱动一上报 runner 就杀掉应用，
  // 所以这里必须等那次写入发生 —— 这是 driver 里唯一正当的固定等待（没有状态可等）。
  await smoke.sleep(900)
  smoke.finish()
})()
