// settings — 设置页画出来的东西，以及一次保存真的走到了文件。
//
// 这个页面是 GetSettingsSchema / settingsTable 的纯渲染器，所以值得在这里断言的就是单测看不到的
// 那两件事：**声明有没有一路送到界面**（分组与顺序、配置列表折成的卡片、下拉的可选值），以及
// **一次编辑有没有走完全程**（内存 → 文件，后者由 Go 侧读 config.yaml 断言）。
//
// 三条断言是这一页独有的、也最容易默默坏掉的：
//   · 路径不当标签：`providers.0.model` 这种字符串不该出现在表单里（卡片头说"是哪个 provider"）；
//   · 下拉的选项来自**这份配置**（`providers.*.name`），不是一张写死的表；
//   · 「自动」= 未设置，落到文件里是那个键**不出现** —— 这是唯一一个"没有值"也有表达的状态。
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

  // ── 列表能加条目、能删条目（暂存，点保存才写盘） ──────────────────────────
  // 一条列表此前必须"先在文件里"，它的字段才存得进去（更新路径不认不存在的路径），所以 ＋ 原来是
  // **立刻写盘**的。现在它只是**暂存**：新条目是页面上的一个待办，点保存时和它的字段值一起写进
  // config.yaml（一次写盘，见 config.ListEdits 的顺序规则）。于是"＋ 了又反悔"什么都不写 ——
  // 这正是「返回对话」暗示的语义，也是立刻写盘做不到的。
  smoke.click(catByName('模型'))
  if (!(await smoke.waitFor('.set-elem-head', '回到模型组'))) return smoke.finish()
  const addBtn = smoke.qa('.set-add .btn')[0]
  if (!addBtn) return smoke.fail('模型组里有「＋ 添加一项」', smoke.text('.settings-form'))
  smoke.click(addBtn)
  const added = await smoke.waitFor(() => {
    const t = smoke.text('.settings-notice')
    return t.indexOf('加了一项') >= 0 ? t : null
  }, '添加条目的回执')
  smoke.check('＋ 只是暂存，回执说清"点保存才写进 config.yaml"',
    !!added && added.indexOf('config.yaml') >= 0, added || '')
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
  // 而且它**能填**：条目还不在配置里，所以它的叶子在 schema 里没有值可走 —— 字段是从元素类型
  // 描述出来的（stagedFields）。没有这一条，＋ 画出来的就是一张空卡片，而那正是立刻写盘存在的理由。
  const newCardKeys = smoke.allText('.set-elem-body .set-key').map((t) => t.replace(/⟳.*$/, '').trim())
  smoke.check('暂存的条目有输入框（叶子从元素类型声明，不靠配置里的值）',
    ['name', 'model', 'api_key'].every((k) => newCardKeys.indexOf(k) >= 0), newCardKeys.join(' / '))

  // 没有块的列表：web_search 在沙箱配置里根本不存在，＋ 要能加（保存时把块建出来）
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
  smoke.check('文件里没有这个块时也能加（暂存，保存时建块）', !!wsAdded, wsAdded || '')
  // 新加的条目必须展开着，即使它所在的列表被声明为"一律折叠"：＋ 造出来的那一条就是拿来填的，
  // 声明折针对的是"已经在那儿、你只是来看看"的条目（justAdded 优先，见 ElementCard）。
  const wsNewCard = await smoke.waitFor(() => {
    const sec = smoke.qa('.set-section').find((d) => smoke.text(d.querySelector('.set-section-head')) === 'web_search')
    const card = sec ? sec.querySelector('.set-elem') : null
    return card && card.querySelector('.set-elem-body') ? card : null
  }, '新加的 web_search 条目展开')
  smoke.check('声明为折叠的列表里，＋ 加出来的条目仍然展开', !!wsNewCard, '')
  if (wsNewCard) smoke.click(wsNewCard.querySelector('.set-elem-head'))

  // 删掉一个**已经存在**的 provider：两步确认，回执里带上"谁还在引用它"。删是**暂存**的 ——
  // 卡片要到保存之后才消失，这正是"点保存才生效"的意思。
  smoke.click(catByName('模型'))
  if (!(await smoke.waitFor('.set-elem-head', '回到模型组（删除）'))) return smoke.finish()
  const card = cardByHead('providers[0]')
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
  smoke.check('删除要确认第二次', true, '')
  smoke.click(confirmBtn)
  const removed = await smoke.waitFor(() => {
    const t = smoke.text('.settings-notice')
    return t.indexOf('标记删除') >= 0 ? t : null
  }, '删除的回执')
  smoke.check('删除只是暂存，回执说清"点保存后才写进 config.yaml"',
    !!removed && removed.indexOf('config.yaml') >= 0, removed || '')
  // 暂存：卡片还在（保存才生效），而且「谁还在引用它」的提醒不能等到保存才说。
  smoke.check('标了删除的卡片还在页面上（保存才生效）',
    !!cardByHead('providers[0]'), smoke.allText('.set-elem-head').join(' / '))
  smoke.check('删除的回执点出还在引用这个名字的键', (removed || '').indexOf('title_provider') >= 0, removed || '')

  // ── 保存才写盘：暂存的加/删 + 填进去的字段值，一次写进 config.yaml ────────
  // 这一段是整套改动的验收：新条目的定位（providers[1]）在保存之前**不在配置里**，它的字段值却在
  // 页面上填得进去；保存时两半一起写 —— 先建条目、再把值写进它（顺序见 config.ListEdits），
  // 同时删掉标记了的那一项。Go 侧读回文件来断言（这一页看不见磁盘）。
  // 这一页曾经被 ⌘, 重开过，回来停在第一个分组，所以先走到模型组。
  smoke.click(catByName('模型'))
  if (!(await smoke.waitFor(() => smoke.text('.settings-title') === '模型', '回到模型组（保存）'))) return smoke.finish()
  const addCard2 = await smoke.waitFor(() => cardByHead('providers[1]'), '暂存的新条目还在')
  if (!addCard2) return smoke.fail('保存前新条目还在页面上', smoke.allText('.set-elem-head').join(' / '))
  if (!addCard2.querySelector('.set-elem-body')) smoke.click(addCard2.querySelector('.set-elem-head'))
  if (!(await smoke.waitFor(() => (addCard2.querySelector('.set-elem-body') ? true : null), '展开新条目'))) return smoke.finish()
  const fill = (label, value) => {
    const row = Array.prototype.slice.call(addCard2.querySelectorAll('.set-row'))
      .find((r) => smoke.text(r.querySelector('.set-key')).replace(/⟳.*$/, '').trim() === label)
    if (!row) return false
    const input = row.querySelector('input, textarea')
    if (!input) return false
    smoke.type(input, value)
    return true
  }
  if (!fill('name', 'added-by-smoke') || !fill('model', 'added-model')) {
    return smoke.fail('给新条目填 name / model', addCard2.innerHTML.slice(0, 300))
  }
  // 新条目上的 `spec.vision` 也设成「开」：它是**可选**叶子。这样下一次把它改回「自动」时走的是
  // config.Unset 那条路 —— 文件里那个键直接不出现（omitempty），回读是 nil。两条路都在这一场里
  // 走到，而"未设置"这个状态本身在这页上是必须能存下去的（design §八之四）。
  const visionInNew = Array.prototype.slice.call(addCard2.querySelectorAll('.set-row'))
    .find((r) => smoke.text(r.querySelector('.set-key')).replace(/⟳.*$/, '').trim() === 'spec.vision')
  const visionNewSel = visionInNew ? visionInNew.querySelector('select') : null
  if (!visionNewSel || !smoke.pick(visionNewSel, 'true')) return smoke.fail('把新条目的 spec.vision 设成「开」', '')
  smoke.check('填了新条目之后保存按钮可用（新条目的字段也是普通草稿）',
    !!smoke.q('.settings-foot .btn') && !smoke.q('.settings-foot .btn').disabled, '')
  smoke.click(smoke.q('.settings-foot .btn'))
  const savedAll = await smoke.waitFor(() => {
    const t = smoke.text('.settings-notice')
    return t.indexOf('已生效') >= 0 ? t : null
  }, '加/删/改一起保存的汇总句')
  smoke.check('一次保存带走了暂存的加、删与字段值', !!savedAll, savedAll || '')
  // 保存成功后页面重读 schema：新条目现在是配置里的真条目（头写的是刚填的名字），标记删除的那一项没了。
  const afterSave = await smoke.waitFor(() => {
    const heads = smoke.allText('.set-elem-head')
    return heads.some((h) => h.indexOf('added-by-smoke') >= 0)
      && !heads.some((h) => h.indexOf('smoke-model') >= 0) ? heads : null
  }, '保存后：新条目成了真条目，被删的那一项不在了')
  smoke.check('保存后新条目成为真条目（名字来自刚填的值），删掉的那一项不在了',
    !!afterSave, afterSave ? afterSave.join(' / ') : smoke.allText('.set-elem-head').join(' / '))

  // 再把它改回「自动」并保存：界面上唯一能表达"没有值"的状态，落到文件里是那个键不出现。
  // 这一条必须落在**还活着的**元素上（上一步把 fixture 里那个删掉了），所以问的是刚加的那个。
  const savedCard = cardByHead('added-by-smoke')
  if (!savedCard) return smoke.fail('找到保存后的新条目', smoke.allText('.set-elem-head').join(' / '))
  if (!savedCard.querySelector('.set-elem-body')) smoke.click(savedCard.querySelector('.set-elem-head'))
  if (!(await smoke.waitFor(() => (savedCard.querySelector('.set-elem-body') ? true : null), '展开新条目（自动）'))) return smoke.finish()
  const visionBack = Array.prototype.slice.call(savedCard.querySelectorAll('.set-row'))
    .find((r) => smoke.text(r.querySelector('.set-key')).replace(/⟳.*$/, '').trim() === 'spec.vision')
  const visionBackSel = visionBack ? visionBack.querySelector('select') : null
  if (!visionBackSel || !smoke.pick(visionBackSel, '')) return smoke.fail('把新条目的 spec.vision 改回「自动」', '')
  smoke.click(smoke.q('.settings-foot .btn'))
  const savedAuto = await smoke.waitFor(() => {
    const t = smoke.text('.settings-notice')
    return t.indexOf('已生效') >= 0 ? t : null
  }, '「自动」保存的汇总句')
  smoke.check('新条目上的「自动」也能保存（写出去的是一片没有这个键的元素）', !!savedAuto, savedAuto || '')

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

  // ── 声明的折叠：web_search / web_fetch 的条目一律折起来 ────────────────────
  // 大小规则（>3 叶才折）对这两个列表是错的：一个 web_search provider 只有三个叶子，却是那一页上
  // 最大的东西 —— 三个展开的条目会把这一页真正的内容（proxy / timeout / max_results）压到下面去。
  // 所以折叠是**声明**（SettingListVO.folded，见 settingFoldedLists）：声明到没到界面由这条钉住，
  // 而"它没有变成恒真"（一个被人扫的列表仍然按大小）由 driver 的提供商卡片那条钉住（大卡片默认折）。
  smoke.click(catByName('工具与权限'))
  if (!(await smoke.waitFor(() => smoke.text('.settings-title') === '工具与权限', '切到工具与权限（折叠）'))) return smoke.finish()
  const wsSection2 = smoke.qa('.set-section').find((d) => smoke.text(d.querySelector('.set-section-head')) === 'web_search')
  if (!wsSection2) return smoke.fail('web_search 段还在', smoke.text('.settings-form').slice(0, 200))
  // 这一页在 fixture 里原来是空的，加过一项之后正好有一张卡：三个叶子、默认折着。
  const wsCard = wsSection2.querySelector('.set-elem')
  if (!wsCard) return smoke.fail('web_search 段里有卡片', wsSection2.innerHTML.slice(0, 200))
  smoke.check('web_search 的条目默认折叠（三个叶子也折）', !wsCard.querySelector('.set-elem-body'),
    'body=' + !!wsCard.querySelector('.set-elem-body'))

  // 返回对话（点那条路）——这一页真正的出口，也顺手把"折着的卡片仍然点得开"试掉。
  smoke.click('.settings-back')
  const back = await smoke.waitFor(
    () => (!smoke.q('.settings-view') && !smoke.q('.app-body.is-hidden') ? true : null), '返回对话')
  smoke.check('返回后设置页消失、对话区恢复', !!back, back ? '' : 'settings-view=' + !!smoke.q('.settings-view'))
  smoke.check('对话区确实还在（没有被卸载过）', !!smoke.q('.app-body .chat') && !!smoke.q('.composer-input'),
    'chat=' + !!smoke.q('.app-body .chat'))

  // ── 键盘：⌘, 进来，Esc 回去 ──────────────────────────────────────────────
  // 两个手势都是给"整页"这个形态补的入口/出口，所以放在最后 —— Esc 那条会把页面收掉，页面没了就
  // 没法再断言别的了。
  //
  // Esc 必须 CLAIM 掉这个键（capture + stopPropagation）：对话区是隐藏而不是卸载的，它自己的 Esc
  // 监听还活着，不声明的话同一个 Esc 会把下面留着没关的面板也关掉 —— 而这一页的契约是"页面之外
  // 什么都不动"。⌘, 反过来只对会话页生效（设置页上按它是无操作），两条合起来就是"进得去、出得来"。
  smoke.key('body', ',', { metaKey: true })
  if (!(await smoke.waitFor('.settings-view', '⌘, 打开设置页'))) return smoke.finish()
  smoke.check('⌘, 打开设置页', true, '')
  // 折叠的卡片仍然点得开（折叠不是禁用）。页面重开时停在自己的第一个分组（通用，那里没有列表卡片），
  // 所以先像用户那样走到有卡片的一页去。
  const toolsCat = catByName('工具与权限')
  if (!toolsCat) return smoke.fail('左栏有「工具与权限」', smoke.allText('.settings-cat').join(' | '))
  smoke.click(toolsCat)
  if (!(await smoke.waitFor(() => smoke.text('.settings-title') === '工具与权限', '切到工具与权限（展开）'))) return smoke.finish()
  const wsCard2 = smoke.qa('.set-section .set-elem')[0]
  if (!wsCard2) return smoke.fail('工具与权限页里有卡片', smoke.text('.settings-form').slice(0, 200))
  smoke.check('折着的卡片点得开', smoke.click(wsCard2.querySelector('.set-elem-head')), '')
  smoke.check('点开后叶子出来了',
    await smoke.waitFor(() => (wsCard2.querySelector('.set-elem-body') ? true : null), '展开卡片') ? true : false, '')

  smoke.key('body', 'Escape')
  const escBack = await smoke.waitFor(
    () => (!smoke.q('.settings-view') && !smoke.q('.app-body.is-hidden') ? true : null), 'Esc 回到会话')
  smoke.check('在设置页按 Esc 回到会话界面', !!escBack, escBack ? '' : 'settings-view=' + !!smoke.q('.settings-view'))

  smoke.finish()
})()
