// composer-page-turn — ⌃U / ⌃D 在输入框里也翻半页（焦点在框里时才按下这两个键的那一刻，是读者最想
// 回看上文的时候）。
//
// 这里量的全是几何与焦点，不量文案：判据是 .chat 的 scrollTop 变了多少 —— 半页是 clientHeight 的一半，
// 所以"变了"必须是**接近半屏**的位移，只动了几像素那种（比如"最新一页被重新钉了一下"）不算。
//
// 前提单独判一句，因为它不成立时后面全是恒真：转写要真的能滚，而且要有个 > 0 的基线（开屏钉在底部）。
// 焦点也一样 —— 用 document.activeElement 派发按键，焦点不在输入框里就当场失败，而不是悄悄改成"在
// 消息区按也能滚"（那是本来就有的行为，正是这条场景要区分开的另一半）。
//
// 反向那一句同样要紧：G / gg 是普通字母。输入框这一层若顺手把它们也拦了，读者就再也打不出 g。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()
  // 种子转写上屏（fixture 那一段填充的最后几个字）。
  if (!(await smoke.waitFor(() => (smoke.text('.chat').indexOf('翻半页真的翻得动') >= 0 ? true : null),
    '种子转写上屏', 15000))) return smoke.finish()

  const chat = () => smoke.q('.chat')
  const geo = () => {
    const el = chat()
    return el ? `top=${Math.round(el.scrollTop)} h=${el.scrollHeight} c=${el.clientHeight}` : '(没有 .chat)'
  }

  // ── 前提：转写够长，且开屏钉在底部（基线 > 0）──────────────────────────────
  const base = await smoke.waitFor(() => {
    const el = chat()
    if (!el || el.scrollHeight <= el.clientHeight + 10) return null // 不可滚就没得测
    // 半页都要翻不动的话，"往上翻"就没有意义了 —— 基线太浅说明 fixture 不够长。
    return el.scrollTop >= el.clientHeight / 2 ? el.scrollTop : null
  }, '转写够长且钉在底部（基线不清零）', 15000)
  if (!base) return smoke.fail('转写可以滚动，且开屏在底部', geo())
  smoke.log('基线', geo())

  // ── 焦点真的在输入框里（后面每一步都按在 document.activeElement 上）────────
  const ta = smoke.q('.composer-input')
  ta.focus()
  smoke.check('焦点在输入框里（这一条场景的全部前提）', document.activeElement === ta,
    'activeElement=' + (document.activeElement ? document.activeElement.tagName + '.' + document.activeElement.className : 'null'))

  // 草稿：文字的归属还在这个框里。
  smoke.type('.composer-input', '这段话还没发出去')
  const draft = () => { const el = smoke.q('.composer-input'); return el ? el.value : null }

  const halfPage = () => Math.round(chat().clientHeight * 0.5)

  // ── ⌃U：往上翻半页 ────────────────────────────────────────────────────────
  smoke.key(document.activeElement, 'u', { ctrlKey: true })
  const upTop = chat().scrollTop
  smoke.check('输入框里按 ⌃U：转写往上翻（≈半页）',
    base - upTop >= halfPage() * 0.8,
    `基线 ${Math.round(base)} → ${Math.round(upTop)}（半页 ${halfPage()}）· ${geo()}`)
  smoke.check('翻页不动输入框里的字', draft() === '这段话还没发出去', JSON.stringify(draft()))

  // ── 反向：G 是普通字母，输入框里不该被当成"跳到最新" ────────────────────────
  const beforeG = chat().scrollTop
  smoke.key(document.activeElement, 'G')
  await smoke.sleep(120) // 真跳的话是一次 layout 之后的滚动，给它一帧
  smoke.check('输入框里按 G：没有被当成跳转键（字母留给文本框）',
    Math.abs(chat().scrollTop - beforeG) <= 2,
    `${Math.round(beforeG)} → ${Math.round(chat().scrollTop)}`)

  // ── ⌃D：翻回下半页 ────────────────────────────────────────────────────────
  smoke.key(document.activeElement, 'd', { ctrlKey: true })
  const downTop = chat().scrollTop
  smoke.check('输入框里按 ⌃D：转写往回翻（≈半页）',
    downTop - beforeG >= halfPage() * 0.8,
    `${Math.round(beforeG)} → ${Math.round(downTop)}（半页 ${halfPage()}）· ${geo()}`)
  smoke.check('翻回来就是翻回来（回到基线，不多不少）',
    Math.abs(downTop - base) <= 2,
    `基线 ${Math.round(base)} → ${Math.round(downTop)}`)

  smoke.finish()
})()
