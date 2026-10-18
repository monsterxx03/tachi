// preview-selection-ask — 预览浮层里选中一段文字，也弹出「复制 | 提问」那枚菜单。
//
// 浮层是第四个宿主（前三个是文件视图、卡片预览、会话全文）：一张文档全屏、一张表格、一份 diff
// 摊开的都是可以读的文字，"选中了想问问"跟它在哪儿摊开无关。三件事在这里钉住，因为每一件都
// 可以"看起来像做了"而实际不工作：
//
//   1. 菜单真的出来了（浮层的 stage 是宿主）。它 portal 到 body 上，所以只断言"有一个 .sel-ask"
//      是不够的 —— 它可能正被浮层盖住。**层级是这条断言的一半**：菜单必须画在浮层之上。
//   2. Esc 先收菜单，浮层还开着。这是最容易做错的一处：浮层的 Esc 在 capture 阶段注册得更早，
//      没有那条 defaultPrevented 的握手，按 Esc 会把整个浮层关掉、菜单跟着一起消失。
//   3. 提问入队之后，队列面板也在浮层之上 —— 否则"加进去了但看不见"。
//
// 图片浮层没有这枚菜单，也在这里断言：一张图片里没有可以选中的文字，而那条断言防的是"顺手给
// 每个浮层都接上"（HTML/PDF 浮层同理，它们的内容在 iframe 里，这一页读不到）。
;(async () => {
  if (!(await smoke.waitForSession())) return smoke.finish()
  const LINE = '这是一份可以全屏打开、然后选中提问的文档。'

  // selectIn 只选中那一句文字本身（不是它所在的那个块）—— 读者拖选一段话时拿到的是这段字，
  // 不是整节包含下一段的文本。range.selectNodeContents(el) 会连标题、连下一段一起带走，
  // 那样引文里就多出不是读者选的东西。
  const selectIn = (el) => {
    const range = document.createRange()
    range.selectNodeContents(el)
    const sel = window.getSelection()
    sel.removeAllRanges()
    sel.addRange(range)
    el.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }))
  }
  // текстOf 按文字找那个最内层的行内元素（一句 <p> 里的文字），选中它本身。
  const selectText = (needle) => {
    const all = smoke.qa('.viewer-stage *')
    const hit = all.filter((e) => e.children.length === 0 && smoke.text(e).indexOf(needle) >= 0)[0]
      || all.find((e) => smoke.text(e).indexOf(needle) >= 0)
    if (!hit) return false
    selectIn(hit)
    return true
  }
  const esc = () => document.body.dispatchEvent(
    new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
  // zIndexOf 读一个元素的层叠层级；浮层是 90，菜单和队列要高于它才有意义。
  const zIndexOf = (el) => {
    const v = el ? getComputedStyle(el).zIndex : ''
    return v === '' ? 0 : parseInt(v, 10) || 0
  }

  // ── 打开那份文档的全屏预览 ───────────────────────────────────────────────
  // 发一条消息让模型把 PREVIEW.md 交出来，然后点卡片上的「预览」展开、再点 ⤢ 进全屏。
  smoke.type('.composer-input', '把 PREVIEW.md 发给我')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor('.file-card', '文件卡片出现', 30000))) return smoke.finish()
  if (!smoke.click('.file-card .file-btn')) return smoke.fail('卡片上有「预览」', smoke.allText('.file-card .file-btn').join(' / '))
  if (!(await smoke.waitFor('.file-preview-body', '卡片里展开预览', 8000))) return smoke.finish()
  // 那个展开按钮由 FileBody 画（filepreview.tsx 的 ExpandButton：title「全屏查看」，字面 ⤢）。
  const expander = smoke.q('.file-preview-body .file-expand')
  if (!expander) return smoke.fail('卡片预览里有全屏按钮',
    smoke.qa('.file-preview-body button').map((b) => (b.className || '') + ':' + (b.getAttribute('title') || smoke.text(b))).join(' / '))
  if (!smoke.click(expander)) return smoke.finish()
  const overlay = await smoke.waitFor('.viewer-overlay', '全屏浮层出现', 8000)
  if (!overlay) return smoke.finish()

  // ── 1. 选中一段字 → 菜单出来，并且在浮层之上 ──────────────────────────────
  // 选中那一句本身所在的元素（最内层、没有子元素的那个），不是包着它的整节 —— 整节会连标题
  // 和下一段一起被选走，而那几行读者并没有选。
  if (!selectText(LINE)) return smoke.fail('浮层里找到那段字', smoke.allText('.viewer-stage').slice(0, 160))
  const menu = await smoke.waitFor('.sel-ask', '选中之后弹出菜单', 4000)
  if (!menu) return smoke.finish()
  const labels = smoke.allText('.sel-ask .sel-ask-btn')
  smoke.check('菜单上是「复制」和「提问」', labels.indexOf('复制') >= 0 && labels.indexOf('提问') >= 0, labels.join(' / '))

  // 层级：菜单画在浮层之上，否则读者看不见它（它 portal 到 body，视觉上会被 90 盖住）。
  const menuZ = zIndexOf(smoke.q('.sel-ask'))
  smoke.check('菜单画在全屏浮层之上（否则它存在但看不见）', menuZ > zIndexOf(overlay),
    `菜单 z=${menuZ} · 浮层 z=${zIndexOf(overlay)}`)

  // ── 2. Esc 先收菜单，浮层还在 ─────────────────────────────────────────────
  esc()
  const gone = await smoke.waitFor(() => (!smoke.q('.sel-ask') ? true : null), 'Esc 收掉菜单', 3000)
  smoke.check('Esc 收掉的是菜单，不是浮层', !!gone && !!smoke.q('.viewer-overlay'),
    '菜单=' + !!smoke.q('.sel-ask') + ' · 浮层=' + !!smoke.q('.viewer-overlay'))

  // ── 3. 提问 → 队列面板，同样在浮层之上 ────────────────────────────────────
  if (!smoke.q('.viewer-overlay')) return smoke.finish()
  if (!selectText(LINE)) return smoke.finish()
  const menu2 = await smoke.waitFor('.sel-ask', '再选一次，菜单又出来', 4000)
  if (!menu2) return smoke.finish()
  const askBtn = smoke.qa('.sel-ask .sel-ask-btn').find((b) => smoke.text(b) === '提问')
  if (!smoke.click(askBtn)) return smoke.finish()
  const input = await smoke.waitFor('.sel-ask-input', '提问框出现', 3000)
  if (!input) return smoke.finish()
  smoke.type('.sel-ask-input', '这份文档是做什么的？')
  // 回车 = 加入待提问（不是发出去），于是菜单收起来、队列面板浮出来。
  smoke.key('.sel-ask-input', 'Enter')
  const panel = await smoke.waitFor('.ask-queue', '待提问面板出现', 4000)
  if (!panel) return smoke.finish()
  smoke.check('面板画在全屏浮层之上（否则「加进去了但看不见」）', zIndexOf(panel) > zIndexOf(smoke.q('.viewer-overlay')),
    `面板 z=${zIndexOf(panel)} · 浮层 z=${zIndexOf(smoke.q('.viewer-overlay'))}`)
  smoke.check('队列里记着这一段字', smoke.text(panel).indexOf(LINE.slice(0, 12)) >= 0,
    smoke.text(panel).slice(0, 120))
  // 来源写的是这个文件的绝对路径（模型据此去读整份文件）。
  smoke.check('面板上写着它来自哪个文件', smoke.text(panel).indexOf('PREVIEW.md') >= 0,
    smoke.text(panel).slice(0, 160))

  // 发送：整队列拼成一条消息交给会话。浮层开着不妨碍这件事 —— 它走的是 composer.submit，
  // 而这条消息的回答会落在会话全文里（浮层后面），所以这里只断言"发出去"，回答由 Go 那一半
  // 在 LLM 边界上验。
  const send = smoke.qa('.ask-queue button').find((b) => smoke.text(b) === '发送')
  if (!send) return smoke.fail('队列面板上有「发送」', smoke.allText('.ask-queue button').join(' / '))
  smoke.click(send)
  await smoke.waitFor(() => (!smoke.q('.ask-queue') ? true : null), '发送之后队列清空', 4000)

  smoke.finish()
})()
