// file-palette — ⌘O 文件搜索：在键盘上找到工作区里的一个文件，回车就在内容列里打开它。
//
// 它钉的是三件单元测试看不见的事（都是「这个手势在真窗口里到底做了什么」）：
//   1. 一个键把人送进一个空框，敲字就窄化列表 —— 行来自后端的模糊索引（SearchFiles），不是前端
//      再过滤一遍；
//   2. 回车真的把那个文件读进文件视图（内容出现、头部的名字对、浮层自己关掉），Esc 从视图退回
//      会话，而不是把文件留在屏幕上；
//   3. **两个根里同名的文件各开各的**：主根有 notes.md、附加根 shared-lib 也有一份。行上给的是
//      相对路径 + 根标签，打开时用的是后端带来的绝对路径（AbsPath/RootPath）—— 前端若把相对路径
//      再对着主根解析一次，选中第二行读到的会是第一个文件的内容。这条只有真窗口里选得出来。
//
// 附带钉住「同一个框」的规矩：⌘O 与 ⌘K 互斥（打开一个关掉另一个），空结果时回车什么也不做。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const boxLabel = () => {
    const b = smoke.q('.palette-box')
    return b ? b.getAttribute('aria-label') : ''
  }
  const fileBoxOpen = () => boxLabel() === '文件搜索'
  const names = () => smoke.allText('.palette-name')
  const paths = () => smoke.allText('.palette-path')
  const selectedName = () => smoke.text('.palette-row[aria-selected="true"] .palette-name')
  const viewName = () => smoke.text('.fileview-name')
  const viewPath = () => smoke.text('.fileview-path')
  const viewText = () => smoke.text('.file-view')
  const openPalette = () => smoke.key(document.body, 'o', { metaKey: true })
  // 搜索是去抖的（120ms）加一次往返：等「要找的那一行出现」，绝不数行数 —— 上一轮的行在新答案
  // 落地之前还在屏幕上，一眼看去的行数既不是旧的也不是新的。
  const waitRow = (name) => smoke.waitFor(
    () => smoke.qa('.palette-row').some((r) => {
      const n = r.querySelector('.palette-name')
      return !!n && n.textContent.trim() === name
    }),
    `列表里出现 ${name}`, 8000,
  )

  // ── 1. ⌘O 打开空框，敲字窄化，回车打开 ───────────────────────────────────
  openPalette()
  if (!(await smoke.waitFor(() => (fileBoxOpen() ? true : null), '⌘O 打开文件搜索', 6000))) return smoke.finish()
  smoke.check('起手就有行（空查询列的是工作区顶层的文件）', true, paths().join(' | ').slice(0, 120))

  smoke.type('.palette-input', 'desktop')
  if (!(await waitRow('desktop.md'))) return smoke.finish()
  smoke.check('输入窄化了列表：只剩命中项', names().includes('desktop.md'), names().join(', '))
  smoke.check('选中的是第一行', selectedName() === 'desktop.md', selectedName())
  // 行上写的是相对路径（根标签为空时不带前缀），不是绝对路径 —— 那是打开时才需要的东西。
  smoke.check('行上给的是相对路径', paths()[0] === 'docs/agents/desktop.md', paths()[0])

  smoke.key('.palette-input', 'Enter')
  if (!(await smoke.waitFor('.file-view', '回车打开文件视图', 8000))) return smoke.finish()
  if (!(await smoke.waitText('.file-view', '文件模糊搜索', 8000))) return smoke.finish()
  smoke.check('文件内容真的读出来了', viewText().includes('文件模糊搜索'), viewText().slice(0, 100))
  smoke.check('头部是这个文件的名字', viewName() === 'desktop.md', viewName())
  smoke.check('打开之后浮层自己关掉', !smoke.q('.palette-box'), boxLabel())

  // Esc 从文件视图回会话：和从文件树点开的那条路一模一样（FileView 自己的 Esc）。
  smoke.key(document.body, 'Escape')
  if (!(await smoke.waitGone('.file-view', 'Esc 回到会话', 6000))) return smoke.finish()

  // ── 2. 附加根里的文件也搜得到、也打得开 ──────────────────────────────────
  openPalette()
  if (!(await smoke.waitFor(() => (fileBoxOpen() ? true : null), '再次打开文件搜索', 6000))) return smoke.finish()
  smoke.type('.palette-input', 'util')
  if (!(await waitRow('util.txt'))) return smoke.finish()
  smoke.key('.palette-input', 'Enter')
  if (!(await smoke.waitText('.file-view', 'EXTRA-ROOT-UTIL', 8000))) return smoke.finish()
  smoke.check('附加根里的文件打开了（内容来自那个根）', viewText().includes('EXTRA-ROOT-UTIL'), viewText().slice(0, 100))
  smoke.check('路径行带着根标签', viewPath().includes('shared-lib'), viewPath())
  smoke.key(document.body, 'Escape')
  if (!(await smoke.waitGone('.file-view', 'Esc 回到会话（第二次）', 6000))) return smoke.finish()

  // ── 3. 两个根里同名的文件：第二行打开的是第二个文件 ──────────────────────
  openPalette()
  if (!(await smoke.waitFor(() => (fileBoxOpen() ? true : null), '第三次打开文件搜索', 6000))) return smoke.finish()
  smoke.type('.palette-input', 'notes')
  if (!(await waitRow('notes.md'))) return smoke.finish()
  // 两行同名：一行在主根（相对路径就是文件名），一行在附加根（带标签）。两行都在，才谈得上「选
  // 哪一行」—— 一条「只有一行」的断言在坏掉的世界里也会通过。
  const twoNotes = await smoke.waitFor(() => (names().filter((n) => n === 'notes.md').length === 2 ? true : null),
    '两份 notes.md 都在列表里', 8000)
  if (!twoNotes) return smoke.finish()
  smoke.check('第一个是同名文件的主根那一份', paths().find((p) => p === 'notes.md') === 'notes.md', paths().join(' | '))
  smoke.check('第二个带着根标签', paths().some((p) => p === 'shared-lib/notes.md'), paths().join(' | '))

  // 走到第二行（Ctrl+N 是列表自己那套键），回车：读到的是附加根那份的内容 —— 前端若把第一行的
  // 相对路径再对着主根解析，这里出现的会是 PRIMARY-NOTES。
  //
  // 高亮**不是**从「第二行」开始的：回车那一刻列表按后端的分数排过，同名两份之间还夹着别的命中。
  // 所以按键的次数由行自己的位置算出来（列表的选中项从第一行起，见 FilePalette 的 re-seat），
  // 而不是写死「按一次」—— 那样只要分数换个顺序，这个场景量到的就是另一个文件。
  const selPath = () => smoke.text('.palette-row[aria-selected="true"] .palette-path')
  const target = paths().indexOf('shared-lib/notes.md')
  smoke.check('目标行在列表里', target > 0, paths().join(' | '))
  for (let i = 0; i < target; i++) smoke.key('.palette-input', 'n', { ctrlKey: true })
  if (!(await smoke.waitFor(() => (selPath() === 'shared-lib/notes.md' ? true : null), 'Ctrl+N 走到附加根那一行', 4000))) return smoke.finish()
  smoke.check('高亮落在附加根那一行（同名文件靠根标签分开）', selPath() === 'shared-lib/notes.md', selPath())
  smoke.key('.palette-input', 'Enter')
  if (!(await smoke.waitText('.file-view', 'EXTRA-ROOT-NOTES', 8000))) return smoke.finish()
  smoke.check('打开的是附加根里的那一份（不是主根的同名文件）', !viewText().includes('PRIMARY-NOTES'),
    viewText().slice(0, 120))
  smoke.key(document.body, 'Escape')
  if (!(await smoke.waitGone('.file-view', 'Esc 回到会话（第三次）', 6000))) return smoke.finish()

  // ── 4. 同一个框：⌘O 与 ⌘K 互斥，两个方向都成立 ──────────────────────────
  openPalette()
  if (!(await smoke.waitFor(() => (fileBoxOpen() ? true : null), '第四次打开文件搜索', 6000))) return smoke.finish()
  smoke.key('.palette-input', 'k', { metaKey: true })
  if (!(await smoke.waitFor(() => (boxLabel() === '会话搜索' ? true : null), '⌘K 把文件搜索换成会话搜索', 6000))) return smoke.finish()
  smoke.check('同一个框只站一个：文件搜索被换掉', !smoke.q('.palette-row .palette-path'), boxLabel())
  smoke.key('.palette-input', 'o', { metaKey: true })
  if (!(await smoke.waitFor(() => (fileBoxOpen() ? true : null), '⌘O 又把它换回来', 6000))) return smoke.finish()
  smoke.check('再按 ⌘O 换回文件搜索', fileBoxOpen(), boxLabel())

  // ── 5. 没有命中时回车什么也不做，Esc 关掉 ────────────────────────────────
  smoke.type('.palette-input', 'zzz-没有这个文件')
  if (!(await smoke.waitText('.palette-empty', '没有匹配的文件', 8000))) return smoke.finish()
  smoke.key('.palette-input', 'Enter')
  await smoke.sleep(400)
  smoke.check('空列表上回车什么也不开', !smoke.q('.file-view'), smoke.text('.file-view'))
  smoke.key('.palette-input', 'Escape')
  if (!(await smoke.waitGone('.palette-box', 'Esc 关掉文件搜索', 6000))) return smoke.finish()
  smoke.check('关掉之后没有留下任何东西', !smoke.q('.file-view') && !smoke.q('.palette-box'), '')

  smoke.finish()
})()
