// file-find — ⌘F inside a previewed FILE, the third surface that hosts find.
//
// The other two (a diff, a review's report) are pinned by oneoff-footer and oneoff-report; this
// one needs a file CARD to get to: the card's 预览 unfolds the inline preview and its ⤢ opens the
// document viewer, which is the surface that owns the find bar. The file repeats one line, so the
// count is a number this driver can name — and the walk has somewhere to walk to.
//
// What is pinned, in order: ⌘F opens the bar and focuses the field; typing counts what is really
// there; the hits are PAINTED (the CSS Custom Highlight registry, not just a counter); Enter walks
// them; and Esc closes the BAR without closing the viewer behind it — the ownership rule that the
// whole find surface is built on.
;(async () => {
  const LINE = '一行笔记，用来被查找。'
  const HITS = 12

  const cmdF = (target) => (target || document.body).dispatchEvent(
    new KeyboardEvent('keydown', { key: 'f', metaKey: true, bubbles: true, cancelable: true }))
  const esc = (target) => (target || document.body).dispatchEvent(
    new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
  const hits = () => (window.CSS && CSS.highlights ? (CSS.highlights.get('tachi-find') || { size: 0 }).size : -1)

  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '把 LONG.md 发给我')
  smoke.click('.send-btn')

  // A delivered file is NOT process: the card is pinned to the turn's tail, so it has to be on
  // screen with the process row still folded — nothing to click first. It used to live INSIDE the
  // fold, which is not "collapsed" but "not in the DOM at all", and the reader had to open the
  // process row to find the file they asked for; that is what this first wait pins.
  if (!(await smoke.waitText('.msg-assistant', '已经把', 30000))) return smoke.finish()
  const card = await smoke.waitFor('.msg-assistant .turn-files .file-card', '附件卡出现（无需展开过程条）', 15000)
  smoke.check('折叠态下附件卡就在屏幕上（不用点过程条）', !!card, '')
  // …and there is exactly ONE: the strip stands in for the process, so the card must not also sit
  // inside the timeline (two cards, one file, reads as two files).
  smoke.check('附件卡只在尾部一处（时序里没有第二张）',
    smoke.qa('.file-card').length === 1 && smoke.qa('.process-timeline .file-card').length === 0,
    smoke.qa('.file-card').length + ' 张 / 时序里 ' + smoke.qa('.process-timeline .file-card').length + ' 张')
  // The tail is the point: the file group is the LAST thing the turn renders, after its prose.
  const parts = smoke.q('.msg-assistant .turn-parts')
  const last = parts && parts.lastElementChild
  smoke.check('附件组是该轮正文之后的最后一块', !!last && String(last.className).indexOf('turn-files') >= 0,
    last ? String(last.className) : '(找不到 .turn-parts)')
  smoke.check('一轮只发了文件就没有过程条（没有别的东西可折）', !smoke.q('.msg-assistant .process-head'),
    smoke.q('.msg-assistant .process-head') ? '居然有过程条' : '')

  // A file gets to the viewer in two steps: the attachment card (预览), then the card's ⤢.
  const preview = await smoke.waitFor(
    () => smoke.qa('.file-card .file-btn').find((b) => b.textContent === '预览') || null,
    '卡片上有「预览」', 8000)
  if (!preview) return smoke.fail('卡片有「预览」按钮', smoke.allText('.file-card .file-btn').join(' | '))
  preview.click()
  const expand = await smoke.waitFor('.file-expand', '展开的卡片有全屏入口（⤢）', 8000)
  if (!expand) return smoke.finish()
  expand.click()
  if (!(await smoke.waitFor('.viewer-overlay .viewer-doc', '预览浮层打开', 8000))) return smoke.finish()

  // The registry is what paints the hits: without it there is nothing to find WITH, so say so
  // here rather than letting every assertion below fail for one reason.
  smoke.check('这个 WebKit 有 CSS Custom Highlight（查找的绘制方式）',
    typeof CSS !== 'undefined' && !!CSS.highlights, String(typeof CSS) + '/' + String(CSS && CSS.highlights))

  // ── ⌘F opens the bar and focuses it ────────────────────────────────────────
  cmdF()
  const bar = await smoke.waitFor('.viewer-overlay .find-bar', '⌘F 在预览浮层里打开查找条', 5000)
  smoke.check('⌘F 在文件预览里打开查找条', !!bar, '')
  smoke.check('查找条拿到焦点（可以直接开始打字）',
    document.activeElement === smoke.q('.find-bar .find-input'),
    document.activeElement ? '.' + String(document.activeElement.className) : '(无)')

  // ── Typing counts what is really there ─────────────────────────────────────
  smoke.type('.find-bar .find-input', LINE)
  const counted = await smoke.waitFor(
    () => (smoke.text('.find-count') === '1/' + HITS ? smoke.text('.find-count') : null),
    '查找条数出 ' + HITS + ' 处命中', 8000)
  smoke.check('命中数是文件里真实的条数（1/' + HITS + '）', !!counted, smoke.text('.find-count'))
  smoke.check('命中被画在正文里（高亮注册表里有 ' + HITS + ' 段）', hits() === HITS, String(hits()))

  // ── Enter walks them ───────────────────────────────────────────────────────
  smoke.key('.find-bar .find-input', 'Enter')
  const walked = await smoke.waitFor(
    () => (smoke.text('.find-count') === '2/' + HITS ? smoke.text('.find-count') : null),
    'Enter 走到下一处命中', 5000)
  smoke.check('Enter 走到下一处（2/' + HITS + '）', !!walked, smoke.text('.find-count'))
  smoke.key('.find-bar .find-input', 'Enter', { shiftKey: true })
  const back = await smoke.waitFor(
    () => (smoke.text('.find-count') === '1/' + HITS ? smoke.text('.find-count') : null),
    'Shift+Enter 走回上一处', 5000)
  smoke.check('Shift+Enter 走回上一处（1/' + HITS + '）', !!back, smoke.text('.find-count'))

  // ── A query with no hits says so in words ──────────────────────────────────
  smoke.type('.find-bar .find-input', '这里绝对没有这一段')
  const none = await smoke.waitFor(
    () => (smoke.text('.find-count') === '无匹配' ? '无匹配' : null),
    '没有命中时给出「无匹配」', 5000)
  smoke.check('查不到时说的是「无匹配」而不是 0/0', !!none, smoke.text('.find-count'))
  smoke.check('没有命中就不画高亮', hits() === 0, String(hits()))

  // ── Esc closes the BAR, not the viewer behind it ───────────────────────────
  // The bar is the innermost surface that wants this key, so it owns it: if the viewer closed
  // here instead, ⌘F-while-typing would throw the document away (see src/find.tsx).
  smoke.type('.find-bar .find-input', LINE)
  await smoke.waitFor(() => (smoke.text('.find-count') === '1/' + HITS ? true : null), '命中回来', 5000)
  esc()
  const gone = await smoke.waitFor(() => !smoke.q('.find-bar'), 'Esc 关掉查找条', 3000)
  smoke.check('Esc 关掉查找条', !!gone, '')
  smoke.check('Esc 只关了查找条，预览浮层还在', !!smoke.q('.viewer-overlay'), '')
  smoke.check('关掉查找条后高亮也撤了（不留一脸黄）', hits() === 0, String(hits()))

  // …and a second Esc closes the viewer, the way it did before find existed.
  esc()
  const closed = await smoke.waitFor(() => !smoke.q('.viewer-overlay'), '第二次 Esc 关掉浮层', 3000)
  smoke.check('第二次 Esc 关掉预览浮层', !!closed, '')

  smoke.finish()
})()
