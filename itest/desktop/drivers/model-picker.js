// model-picker — 工具条上那个「模型」选择器，是一个**带过滤框的菜单**（App.tsx 的 ProviderPicker）。
//
// 它存在的理由是数量：这台机器的配置里有几十个模型，而原生下拉是系统菜单、塞不进一个输入框。
// 所以这一条要的不是「列表里有几项」（settings 场景已经在读那个列表了，它关心的是保存之后新
// provider 有没有立刻出现），而是**打字真的会缩窄列表**、键盘真的能选、Esc 和点外部真的能关。
// 「菜单长得像有过滤框、但输入不生效」「过滤把该留的也滤掉了」「过滤框记着上次的查询」都是只读
// 一次列表看不出来的错。
//
// 沙箱默认只种一个 provider（mock），一条也证明不了过滤——输入之后它要么全留要么全空。所以这个
// 场景种了一批额外条目（scenario.extraProviders），每个一个 provider 文件。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const trigger = () => smoke.q('.provider-picker .provider-select')
  const names = () => smoke.allText('.provider-menu-name')
  const typeFilter = (v) => smoke.type('.provider-menu-input', v)
  const openMenu = async () => {
    if (!trigger()) { smoke.fail('工具条上有模型选择器', ''); return false }
    smoke.click(trigger())
    return !!(await smoke.waitFor('.provider-menu-input', '模型菜单打开（过滤框出现）'))
  }

  // ── 打开 ──────────────────────────────────────────────────────────────────
  if (!(await openMenu())) return smoke.finish()
  const all = names()
  smoke.check('点开的是一个菜单（有过滤框），不是一个系统下拉', !!smoke.q('.provider-menu-input'), '')
  // 它得**看得见**：菜单在 DOM 里、行为也对，但被祖先的 overflow 裁掉、或者塌成一条线，只在
  // 断言行为时是看不出来的。位置也一起看：工具条在窗口底部，所以菜单要向上展开（top 为正、
  // 底边在窗口内），而它比触发按钮宽——一个从右边缘往左伸的浮层出不了窗口左边。
  const menuBox = smoke.q('.provider-menu').getBoundingClientRect()
  const inWindow = menuBox.top >= 0 && menuBox.left >= 0 &&
    menuBox.bottom <= window.innerHeight && menuBox.right <= window.innerWidth
  smoke.check('菜单有实际尺寸、且整个落在窗口内（没被裁掉）',
    menuBox.width > 200 && menuBox.height > 100 && inWindow,
    `${Math.round(menuBox.width)}×${Math.round(menuBox.height)} @ ${Math.round(menuBox.left)},${Math.round(menuBox.top)}`)
  smoke.check('菜单列出配置里的全部 provider（共享的 mock + 场景种的 8 个）', all.length === 9,
    all.join(' / ') || '(空)')
  // 当前在用的那个被标出来。默认是共享的 mock（sandbox 的 config.yaml 写着 provider: smoke），
  // 而它在列表里不是唯一一条——标记必须落在它身上。
  smoke.check('当前在用的那个有「当前」标记',
    smoke.text('.provider-menu-row.is-current .provider-menu-name') === 'smoke',
    smoke.text('.provider-menu-row.is-current .provider-menu-name') || '(没有标记)')

  // ── 过滤 ──────────────────────────────────────────────────────────────────
  typeFilter('pick')
  const narrowed = await smoke.waitFor(() => {
    const n = names()
    return n.length === 1 && n[0] === 'fixture-pick' ? n : null
  }, '输入 "pick" 之后只剩 fixture-pick')
  smoke.check('打字会缩窄列表（只剩匹配的那条）', !!narrowed, (narrowed || names()).join(' / '))

  // 过滤也看 MODEL 字符串，不只是名字：名字是工具条上显示的那个（`fixture-pick`），model 是
  // 真正送去 API 的那个（`fixture/fixture-pick`）——按厂商找模型（"anthropic"、"deepseek/"）
  // 只有后者里有。种下的条目 model 都带斜杠，而共享 mock 的 model 是 `smoke-model`：所以一个
  // 斜杠只可能匹配到 model。
  typeFilter('fixture/')
  const byModel = await smoke.waitFor(() => {
    const n = names()
    return n.length === 8 ? n : null
  }, '按 model 过滤："fixture/" 命中 8 个，mock 被滤掉')
  smoke.check('过滤也匹配 model 字符串（名字里没有斜杠，斜杠只可能来自 model）',
    !!byModel, (byModel || names()).join(' / '))

  // ↑↓ 移动高亮。上面那条过滤之后列表是 8 条（按名字排），所以"往下一次"落在第二条上——
  // 钉的是「高亮真的动了、而且动的是列表自己的一条」，不是"按了键没报错"。
  smoke.key('.provider-menu-input', 'ArrowDown')
  const movedDown = await smoke.waitFor(
    () => (smoke.text('.provider-menu-row.is-selected .provider-menu-name') === 'fixture-beta'
      ? true : null), '↓ 把高亮移到第二条')
  smoke.check('↓ 把高亮往下移一条', !!movedDown,
    smoke.text('.provider-menu-row.is-selected .provider-menu-name') || '(没有高亮)')
  smoke.key('.provider-menu-input', 'ArrowUp')
  const movedBack = await smoke.waitFor(
    () => (smoke.text('.provider-menu-row.is-selected .provider-menu-name') === 'fixture-alpha'
      ? true : null), '↑ 把高亮移回第一条')
  smoke.check('↑ 把高亮移回来（可逆）', !!movedBack,
    smoke.text('.provider-menu-row.is-selected .provider-menu-name') || '(没有高亮)')

  // 一条都不剩时要有话说：一个空框和"没有匹配"是两回事，前者让读者以为自己操作错了。
  typeFilter('zzz-没有这个模型')
  const empty = await smoke.waitFor(() => (smoke.q('.provider-menu-empty') ? true : null), '空结果的提示')
  smoke.check('没有匹配时给一句「没有匹配的模型」，而不是一片空白',
    !!empty, smoke.text('.provider-menu-empty') || '(空)')

  // ── 键盘：Enter 切过去 ────────────────────────────────────────────────────
  typeFilter('fixture-pick')
  if (!(await smoke.waitFor(() => (names().length === 1 ? true : null), '再筛到唯一一条'))) return smoke.finish()
  // 高亮必须落在这一条上，否则 Enter 是一个静默的空操作（shown[idx] 为 undefined）—— 这正是
  // 过滤之后最容易错的地方：记住的是"第 8 行"，而查询只剩下了一行。
  smoke.check('过滤到一条时高亮就落在它身上（否则 Enter 是空操作）',
    smoke.text('.provider-menu-row.is-selected .provider-menu-name') === 'fixture-pick',
    smoke.text('.provider-menu-row.is-selected .provider-menu-name') || '(没有高亮)')
  smoke.key('.provider-menu-input', 'Enter')
  const switched = await smoke.waitFor(
    () => (smoke.text('.provider-select') === 'fixture-pick' ? true : null), 'Enter 切换到这个模型')
  smoke.check('Enter 把高亮的那条切过去（工具条上跟着变）', !!switched, smoke.text('.provider-select'))
  smoke.check('切换之后菜单自己关掉', !smoke.q('.provider-menu'), '')

  // ── 关掉它的两条路，以及"重新打开是干净的" ────────────────────────────────
  if (!(await openMenu())) return smoke.finish()
  smoke.check('重新打开时过滤框是空的（上一次的查询不该留着）',
    smoke.q('.provider-menu-input').value === '', `"${smoke.q('.provider-menu-input').value}"`)
  // 高亮该落在当前在用的那个身上，而不是列表头——否则"我现在是哪条"每次打开都要自己找。
  // 这一条在切换之后才有力：fixture-pick 在列表里排第 6，不是第 0。
  smoke.check('重新打开时高亮落在当前在用的那条（不是列表头）',
    smoke.text('.provider-menu-row.is-selected .provider-menu-name') === 'fixture-pick',
    smoke.text('.provider-menu-row.is-selected .provider-menu-name') || '(没有高亮)')

  smoke.key('.provider-menu-input', 'Escape')
  const escClosed = await smoke.waitFor(() => (smoke.q('.provider-menu') ? null : true), 'Esc 关掉菜单')
  smoke.check('Esc 关掉菜单', !!escClosed, '')
  smoke.check('Esc 不改变已经选好的模型',
    smoke.text('.provider-select') === 'fixture-pick', smoke.text('.provider-select'))

  if (!(await openMenu())) return smoke.finish()
  // mousedown，不是 click：关掉它的监听就在 document 的 mousedown 上（和上下文面板、MCP 面板
  // 同一条），而 el.click() 只派发 click —— 那样测的是一扇不存在的门。
  smoke.q('.chat').dispatchEvent(new MouseEvent('mousedown', { bubbles: true }))
  const outsideClosed = await smoke.waitFor(() => (smoke.q('.provider-menu') ? null : true), '点菜单外面关掉')
  smoke.check('点菜单外面关掉它', !!outsideClosed, '')

  smoke.finish()
})()
