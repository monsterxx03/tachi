// settings — 设置页画出来的东西，以及一次保存真的走到了文件。
//
// 这个页面是 GetSettingsSchema / settingsTable 的纯渲染器，所以值得在这里断言的就是单测看不到的
// 那两件事：**声明有没有一路送到界面**（分组与顺序、配置列表折成的卡片、下拉的可选值），以及
// **一次编辑有没有走完全程**（内存 → 文件，后者由 Go 侧读 config.yaml 断言）。
//
// 三条断言是这一页独有的、也最容易默默坏掉的：
//   · 路径不当标签：`providers.0.model` 这种字符串不该出现在表单里（卡片头说"是哪个 provider"）；
//   · 下拉的选项来自**这份配置**（`providers.*.name`），不是一张写死的表；
//   · 「自动」= 未设置，落到文件里是 `vision: null` —— 这是唯一一个"没有值"也要写出去的状态。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  const rowByKey = (key) =>
    smoke.qa('.set-row').find((r) => {
      const k = r.querySelector('.set-key')
      return !!k && smoke.text(k).replace(/⟳.*$/, '').trim() === key
    })
  const catByName = (name) => smoke.qa('.settings-cat').find((b) => smoke.text(b.firstElementChild) === name)

  // ── 进设置页 ──────────────────────────────────────────────────────────────
  const settingsBtn = smoke.qa('.footer-item').find((b) => smoke.text(b) === '设置')
  if (!settingsBtn) return smoke.fail('侧栏底部有「设置」入口', smoke.allText('.footer-item').join(' / '))
  smoke.click(settingsBtn)
  if (!(await smoke.waitFor('.settings-view', '设置页出现'))) return smoke.finish()
  // 替换对话区的是"隐藏"而不是卸载：转写、输入草稿、滚动位置都得活到返回之后（design §四）。
  // 所以这里同时断言两半——隐藏了，但还在 DOM 里。
  smoke.check(
    '对话区被隐藏但仍挂载（状态不丢）',
    !!smoke.q('.app-body.is-hidden') && !!smoke.q('.app-body .chat'),
    'is-hidden=' + !!smoke.q('.app-body.is-hidden') + ' · .chat=' + !!smoke.q('.app-body .chat')
  )

  // ── 左栏：分组与顺序都由后端给 ────────────────────────────────────────────
  const cats = smoke.qa('.settings-cat').map((b) => smoke.text(b.firstElementChild))
  const wantCats = ['通用', '模型', '工具与权限', 'MCP', '会话与上下文', '扩展能力', '高级']
  smoke.check('左栏列出全部分组，顺序与后端一致', cats.join('|') === wantCats.join('|'), cats.join(' | '))

  // ── 配置列表：一个 list item 是一张卡片 ──────────────────────────────────
  const modelCat = catByName('模型')
  if (!modelCat) return smoke.fail('左栏有「模型」', cats.join(' | '))
  smoke.click(modelCat)
  if (!(await smoke.waitFor('.set-elem', '模型组里出现 providers 的卡片'))) return smoke.finish()

  // 卡片头是"哪些 provider"，不是下标：providers.0 有十一个叶子，默认收起来。
  smoke.check('卡片头写 provider 名 + 模型名', smoke.text('.set-elem-name') === 'smoke · smoke-model',
    smoke.text('.set-elem-name'))
  // 副标题是列表项的定位（`providers[0]`），不是原始路径：`providers.0` 读起来像又一层嵌套，
  // `[0]` 才说得清"这是列表的第 0 项"。索引仍然是配置自己的（0 基），对着 config.yaml 找得到。
  smoke.check('卡片头副标题是列表项定位（providers[0]，不是 providers.0）',
    smoke.text('.set-elem-path') === 'providers[0]', smoke.text('.set-elem-path'))
  smoke.check('大卡片默认折叠', !smoke.q('.set-elem-body'), 'body=' + !!smoke.q('.set-elem-body'))
  smoke.check('折叠时路径不是标签（页面上没有 providers.0.*）',
    smoke.text('.settings-form').indexOf('providers.0.') < 0, smoke.text('.settings-form').slice(0, 120))

  if (!smoke.click(smoke.q('.set-elem-head'))) return smoke.fail('点得到卡片头', '')
  if (!(await smoke.waitFor('.set-elem-body', '点开后展开叶子'))) return smoke.finish()
  const innerKeys = smoke.allText('.set-elem-body .set-key').map((t) => t.replace(/⟳.*$/, '').trim())
  smoke.check('展开后标签是路径尾段（前缀由卡片头说了）',
    ['name', 'model', 'base_url', 'api_key'].every((k) => innerKeys.indexOf(k) >= 0), innerKeys.join(' / '))
  smoke.check('展开后仍然不出现 providers.0.*',
    smoke.text('.set-elem-body').indexOf('providers.0.') < 0, smoke.text('.set-elem-body').slice(0, 120))

  // ── 「自动」：*bool 且没有默认值的叶子是三态，不是复选框 ───────────────────
  // spec.vision 的 nil 有含义（按模型能力表自动判断），所以它必须有第三个位置。
  const visionSel = (() => {
    const r = rowByKey('spec.vision')
    return r ? r.querySelector('select') : null
  })()
  const visionOpts = visionSel ? Array.prototype.map.call(visionSel.options, (o) => o.textContent) : []
  smoke.check('spec.vision 是三选一（自动 / 开 / 关）', visionOpts.length === 3 && visionOpts[0].indexOf('自动') >= 0,
    visionOpts.join(' / '))
  smoke.check('spec.vision 当前是「开」（config.yaml 里是 true）', !!visionSel && visionSel.value === 'true',
    visionSel ? visionSel.value : '(没有 select)')

  // ── 下拉的选项来自这份配置 ───────────────────────────────────────────────
  const generalCat = catByName('通用')
  if (!generalCat) return smoke.fail('左栏有「通用」', cats.join(' | '))
  smoke.click(generalCat)
  if (!(await smoke.waitFor(() => smoke.text('.settings-title') === '通用', '切到通用分组'))) return smoke.finish()
  const tpSel = (() => {
    const r = rowByKey('title_provider')
    return r ? r.querySelector('select') : null
  })()
  const tpOpts = tpSel ? Array.prototype.map.call(tpSel.options, (o) => o.textContent) : []
  smoke.check('title_provider 是下拉，选项来自配置里的 providers',
    tpOpts.indexOf('smoke') >= 0 && tpOpts[0].indexOf('默认') >= 0, tpOpts.join(' / '))
  if (!tpSel) return smoke.finish()

  // ── 改一项 → 保存 → 汇总句 ───────────────────────────────────────────────
  smoke.pick(tpSel, 'smoke')
  const saveBtn = smoke.q('.settings-foot .btn')
  smoke.check('改过之后行被标脏、保存按钮可用',
    !!smoke.q('.set-row.is-dirty') && !!saveBtn && !saveBtn.disabled,
    'dirty=' + !!smoke.q('.set-row.is-dirty') + ' disabled=' + (saveBtn ? saveBtn.disabled : '?'))
  smoke.click(saveBtn)
  const first = await smoke.waitFor(() => {
    const t = smoke.text('.settings-notice')
    return t.indexOf('已生效') >= 0 ? t : null
  }, '第一次保存的汇总句')
  smoke.check('汇总句说清结果（后端写的那句，前端不翻译）', !!first, first || '')

  // ── 「自动」也要能存：它写的是 null ──────────────────────────────────────
  // 这一条是整页最不显然的路：把有含义的 nil 写回文件，靠的是 config.Unset。
  smoke.click(catByName('模型'))
  if (!(await smoke.waitFor('.set-elem-head', '回到模型组'))) return smoke.finish()
  smoke.click(smoke.q('.set-elem-head'))
  if (!(await smoke.waitFor('.set-elem-body', '再次展开卡片'))) return smoke.finish()
  const autoSel = (() => {
    const r = rowByKey('spec.vision')
    return r ? r.querySelector('select') : null
  })()
  if (!autoSel || !smoke.pick(autoSel, '')) return smoke.fail('把 spec.vision 选成「自动」', '')
  smoke.click(smoke.q('.settings-foot .btn'))
  const second = await smoke.waitFor(() => {
    const t = smoke.text('.settings-notice')
    return t.indexOf('已生效') >= 0 ? t : null
  }, '第二次保存的汇总句')
  smoke.check('「自动」也能保存', !!second, second || '')
  // 保存成功后页面会重读 schema，所以这里问的是"它现在显示什么"——没读回来的话这一条不会过。
  const autoAfter = (() => {
    const r = rowByKey('spec.vision')
    return r ? r.querySelector('select') : null
  })()
  smoke.check('重读之后这一项显示回「自动」（IsSet 是后端给的，不是前端猜的）',
    !!autoAfter && autoAfter.value === '', autoAfter ? autoAfter.value : '(没有 select)')

  // ── 块标题：一个 key 的叶子归到一个标题下 ────────────────────────────────
  // 这条是"web_search.xxxx / web_fetch.xxxx 能不能放在子标题下"的验收：以前每个分类是一条平面
  // 列表，两个块只能靠前缀去认；现在一个块一条标题，块内的行只写路径的余下部分。
  smoke.click(catByName('工具与权限'))
  if (!(await smoke.waitFor(() => smoke.text('.settings-title') === '工具与权限', '切到工具与权限'))) return smoke.finish()
  const heads = smoke.allText('.set-section-head')
  smoke.check('每个块一条标题（web_fetch / web_search / tool_result / permissions.bash）',
    ['web_fetch', 'web_search', 'tool_result', 'permissions.bash'].every((h) => heads.indexOf(h) >= 0),
    heads.join(' | '))
  const blockKeys = smoke.allText('.settings-form .set-key').map((t) => t.replace(/⟳.*$/, '').trim())
  smoke.check('块内的行只写余下的路径（web_search.proxy → proxy）',
    blockKeys.indexOf('proxy') >= 0 && blockKeys.indexOf('max_results') >= 0 &&
      blockKeys.indexOf('web_search.proxy') < 0 && blockKeys.indexOf('web_fetch.timeout') < 0,
    blockKeys.join(' / '))

  // ── 列表能加条目、能删条目 ───────────────────────────────────────────────
  // 一条列表在文件里得先有那一项，它的字段才存得进去（更新路径不认不存在的路径），所以 ＋ 是**立刻
  // 写盘**的：卡片随即出现（并且展开），填完再常规保存。删除同理，只是方向相反，而且会警告还有谁
  // 在引用那个名字。
  smoke.click(catByName('模型'))
  if (!(await smoke.waitFor('.set-elem-head', '回到模型组'))) return smoke.finish()
  const addBtn = smoke.qa('.set-add .btn')[0]
  if (!addBtn) return smoke.fail('模型组里有「＋ 添加一项」', smoke.text('.settings-form'))
  smoke.click(addBtn)
  const added = await smoke.waitFor(() => {
    const t = smoke.text('.settings-notice')
    return t.indexOf('加了一项') >= 0 ? t : null
  }, '添加条目的回执')
  smoke.check('＋ 立即写入并回执', !!added, added || '')
  // 新卡片出现、展开着、定位是 providers[1]（不是 providers.1）
  // 全新的条目没有名字，所以卡片头就是它的定位（providers[1]）—— 副标题此时是空的（head 里已经
  // 写了同一句话，重复一遍没有意义）。
  const cardByHead = (want) =>
    smoke.qa('.set-elem').find((c) => smoke.text(c.querySelector('.set-elem-head')).indexOf(want) >= 0)
  const newCard = await smoke.waitFor(() => cardByHead('providers[1]'), '新条目的卡片')
  smoke.check('新条目出现，头是 providers[1]', !!newCard,
    newCard ? smoke.text(newCard.querySelector('.set-elem-head')) : '')
  // 等它展开（新增的条目就是拿来填的，不该还折着）
  const opened = await smoke.waitFor(
    () => (newCard.querySelector('.set-elem-body') ? true : null), '新条目自动展开')
  smoke.check('新条目是展开的（正是要填它）', !!opened, opened ? '' : '仍然折着')

  // 没有块的列表：web_search 在沙箱配置里根本不存在，＋ 要把块建出来
  smoke.click(catByName('工具与权限'))
  if (!(await smoke.waitFor(() => smoke.text('.settings-title') === '工具与权限', '切到工具与权限（加列表）'))) return smoke.finish()
  // ＋ 按钮自己说是哪个列表（一个块里可能既有标量又有列表）
  const addLabels = smoke.allText('.set-add .btn')
  smoke.check('＋ 按钮写明加的是哪个列表', addLabels.indexOf('＋ 添加 providers') >= 0, addLabels.join(' / '))
  const wsSection = smoke.qa('.set-section').find((d) => smoke.text(d.querySelector('.set-section-head')) === 'web_search')
  const wsAdd = wsSection ? wsSection.querySelector('.set-add') : null
  if (!wsAdd) return smoke.fail('web_search 段里有 ＋', smoke.text('.settings-form'))
  smoke.click(wsAdd.querySelector('.btn'))
  const wsAdded = await smoke.waitFor(() => {
    const t = smoke.text('.settings-notice')
    return t.indexOf('web_search.providers') >= 0 && t.indexOf('加了一项') >= 0 ? t : null
  }, 'web_search 的添加回执')
  smoke.check('文件里没有这个块时也能加（块被建出来）', !!wsAdded, wsAdded || '')

  // 删掉刚加的那个 provider：两步确认，删完回执里带上"谁还在引用它"
  smoke.click(catByName('模型'))
  if (!(await smoke.waitFor('.set-elem-head', '回到模型组（删除）'))) return smoke.finish()
  const card = cardByHead('providers[1]')
  if (!card) return smoke.fail('找到要删的那张卡片', smoke.text('.settings-form').slice(0, 200))
  // 删除入口在卡片体里，所以先像用户那样把它展开。
  if (!card.querySelector('.set-elem-body')) smoke.click(card.querySelector('.set-elem-head'))
  if (!(await smoke.waitFor(() => (card.querySelector('.set-elem-body') ? true : null), '展开要删的卡片'))) return smoke.finish()
  const delBtn = Array.prototype.slice.call(card.querySelectorAll('.btn'))
    .find((b) => smoke.text(b).indexOf('删除这一项') >= 0)
  if (!delBtn) return smoke.fail('卡片里有「删除这一项」', card.innerHTML.slice(0, 200))
  // 危险入口得和普通按钮看得出区别（这是整页唯一会把东西从用户配置里拿走的按钮）。
  const plainBtn = smoke.q('.settings-foot .btn')
  smoke.check('删除入口是危险色，和普通按钮分得开',
    !!plainBtn && getComputedStyle(delBtn).color !== getComputedStyle(plainBtn).color,
    getComputedStyle(delBtn).color + ' vs 普通按钮 ' + (plainBtn ? getComputedStyle(plainBtn).color : '(无)'))
  smoke.click(delBtn)
  const confirmBtn = await smoke.waitFor(
    () => smoke.qa('.set-elem-confirm .btn.danger')[0], '删除的第二步确认')
  if (!confirmBtn) return smoke.finish()
  smoke.check('删除要确认第二次（写盘不可撤销）', true, '')
  smoke.click(confirmBtn)
  const removed = await smoke.waitFor(() => {
    const t = smoke.text('.settings-notice')
    return t.indexOf('删除') >= 0 ? t : null
  }, '删除的回执')
  smoke.check('删除回执', !!removed, removed || '')
  const gone = await smoke.waitFor(
    () => (smoke.qa('.set-elem-path').some((e) => smoke.text(e) === 'providers[1]') ? null : true), '卡片消失')
  smoke.check('删掉的卡片从页面消失', !!gone, '')

  // ── 字号下限（"设置那两个字好小"） ────────────────────────────────────────
  // 左栏那个「设置」原来是 11.5px + 大写字距，比它标着的分类（14.5px）还小 —— 一页的标题成了页脚
  // 小字。钉的是 INTENT（下限 + 标题严格大于条目），不是具体数字，跟侧栏那份（sidebar-text）同
  // 一条规矩：以后调版式不该变成测试失败。
  smoke.click(catByName('高级'))
  if (!(await smoke.waitFor(() => smoke.text('.settings-title') === '高级', '切到高级分组（采字号）'))) return smoke.finish()
  const px = (sel) => {
    const el = smoke.q(sel)
    return el ? parseFloat(getComputedStyle(el).fontSize) : 0
  }
  const textParts = {
    '左栏标题 .settings-group-label': '.settings-group-label',
    '分类 .settings-cat': '.settings-cat',
    '分类计数 .settings-cat-count': '.settings-cat-count',
    '返回 .settings-back': '.settings-back',
    '左栏脚注 .settings-legend': '.settings-legend',
    '组标题 .settings-title': '.settings-title',
    '组副标题 .settings-sub': '.settings-sub',
    '键名 .set-key': '.set-key',
    '块标题 .set-section-head': '.set-section-head',
    '说明 .set-note': '.set-note',
    '输入框 .set-input': '.set-input',
    '需重启徽标 .set-restart': '.set-restart',
    '汇总句 .settings-notice': '.settings-notice',
  }
  const sizes = {}
  for (const [k, sel] of Object.entries(textParts)) sizes[k] = px(sel)
  smoke.log('设置页字号', JSON.stringify(sizes))
  const absent = Object.entries(sizes).filter(([, v]) => v === 0).map(([k]) => k)
  if (!smoke.check('字号取样点都在（选择器没写错）', absent.length === 0, absent.join(' / '))) return smoke.finish()
  const tooSmall = Object.entries(sizes).filter(([, v]) => v < 12)
  smoke.check('设置页里没有小于 12px 的文字', tooSmall.length === 0,
    tooSmall.length ? JSON.stringify(tooSmall) : '最小 ' + Math.min(...Object.values(sizes)) + 'px')
  smoke.check('左栏标题严格大于分类条目（层级看得出来）',
    sizes['左栏标题 .settings-group-label'] > sizes['分类 .settings-cat'],
    '标题 ' + sizes['左栏标题 .settings-group-label'] + 'px vs 分类 ' + sizes['分类 .settings-cat'] + 'px')

  // 块标题必须和它下面的配置**不是同一种排印**：原来是 mono/13px，`web_fetch` 看起来就是一条
  // 没有输入框的配置项（"标题块字号样式和下面的配置完全一样，都没法区分"）。判据是字体族或字号
  // 至少差一处，不是具体数值。
  const face = (sel) => {
    const el = smoke.q(sel)
    if (!el) return null
    const cs = getComputedStyle(el)
    return { family: cs.fontFamily, size: parseFloat(cs.fontSize) }
  }
  const head = face('.set-section-head')
  const key = face('.set-key')
  smoke.check('块标题与键名不是同一种排印（标题不像配置项）',
    !!head && !!key && (head.family !== key.family || Math.abs(head.size - key.size) >= 1.5),
    head && key ? '标题 ' + head.size + 'px ' + head.family + ' vs 键名 ' + key.size + 'px ' + key.family : '取样点缺失')

  // ── 返回对话 ─────────────────────────────────────────────────────────────
  smoke.click('.settings-back')
  const back = await smoke.waitFor(
    () => (!smoke.q('.settings-view') && !smoke.q('.app-body.is-hidden') ? true : null), '返回对话')
  smoke.check('返回后设置页消失、对话区恢复', !!back, back ? '' : 'settings-view=' + !!smoke.q('.settings-view'))
  smoke.check('对话区确实还在（没有被卸载过）', !!smoke.q('.app-body .chat') && !!smoke.q('.composer-input'),
    'chat=' + !!smoke.q('.app-body .chat'))

  smoke.finish()
})()
