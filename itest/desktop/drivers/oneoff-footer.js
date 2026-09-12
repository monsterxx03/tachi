// oneoff-footer — the turn-level review entry, and the state machine it became.
//
// The chip used to start a review that streamed a whole turn into the conversation (and left
// that turn looking like one that had changed files). Now the review is a side-channel run:
// the chip goes 评审本轮改动 → 评审中… → 已评审 N 条 · 查看, where N is the backend's tally of
// ReportFinding calls, and 查看 opens the panel the run's process went to.
//
// The scope here is the file the turn wrote, so this scenario runs in a git repository with an
// uncommitted change — a scoped review refuses to start without a baseline.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '写一个 NOTES.md')
  smoke.click('.send-btn')

  // The chip only exists on a turn that changed something, so its arrival is also the proof
  // that the WriteFile round landed (a fragment diff on the card feeds the same aggregate).
  const before = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('评审本轮改动') >= 0) || null,
    'footer 出现「评审本轮改动」', 30000)
  if (!before) return smoke.finish()
  smoke.check('有改动的回合才有评审入口', true, smoke.qa('.diff-chip').map((b) => b.textContent).join(' | '))
  smoke.check('评审前没有已评审状态', !smoke.qa('.diff-chip').some((b) => b.textContent.indexOf('已评审') >= 0), '')

  // The chip's MIDDLE state — the one that was reported missing (「点击评审按钮后，立刻会变成
  // 已评审查看，但没法点击，应该是评审中」). Two things made it invisible: the pending state was
  // cleared as soon as the fork had been STARTED (while the record on disk already carried this
  // turn's id, so the chip jumped straight to 已评审 with the button disabled by the very run it
  // reported on), AND it is a short-lived state whose length is the run's — a few hundred ms under
  // the mock, seconds to minutes for a real review. So this does not wait for the state, it
  // SAMPLES the chip and asserts the sequence contains it: what is being pinned is that the chip
  // passes through 评审中…, and that it stays clickable there.
  const seen = []
  const tick = setInterval(() => {
    const chip = smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('评审') >= 0)
    if (!chip) return
    const text = smoke.text(chip)
    if (!seen.length || seen[seen.length - 1].text !== text) seen.push({ text, disabled: chip.disabled })
  }, 10)
  before.click()
  const after = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('已评审') >= 0) || null,
    'footer 变成「已评审 N 条 · 查看」', 30000)
  clearInterval(tick)
  const trail = seen.map((s) => s.text).join(' → ')

  const pending = seen.find((s) => s.text.indexOf('评审中') >= 0)
  smoke.check('评审期间 chip 经过「评审中…」（而不是直接跳到已评审）', !!pending, trail)
  smoke.check('「评审中…」可以点（去看过程）', !!pending && !pending.disabled,
    pending ? `disabled=${pending.disabled}` : '')
  smoke.check('评审结束后 chip 报出意见条数', !!after, after ? smoke.text(after) : trail)
  // The FINAL state is read off the element the wait returned, not off the trail: the trail is
  // for states whose whole life is the run (评审中…), and its 10ms ticks can be starved while the
  // webview is busy — the last change before the wait resolved was missed in one run, which
  // turned a steady state into an intermittent failure.
  smoke.check('「已评审 · 查看」可以点（会话忙也不禁用）', !!after && !after.disabled,
    after ? `disabled=${after.disabled}` : '(没找到已评审的 chip)')
  smoke.check('条数来自后端对 ReportFinding 的计数（mock 报了 1 条）',
    !!after && smoke.text(after).indexOf('1 条') >= 0, after ? smoke.text(after) : '')

  // The run's process is NOT in the conversation — that was the original complaint: a review
  // turn that looked like a turn which had changed files. The turn's OWN cards stay (they are
  // the turn: one per file it wrote), so the assertion is about the review's: its
  // ReportFinding call must not appear.
  const chat = smoke.text('.chat')
  smoke.check('对话里没有评审的回复', chat.indexOf('评审完成：1 条意见。') < 0, '')
  const names = smoke.qa('.chat .tool-name').map((e) => e.textContent)
  smoke.check('对话里没有评审的 ReportFinding 卡', !names.some((n) => n.indexOf('ReportFinding') >= 0), names.join('|'))
  smoke.check('对话里只有本轮自己的工具卡（两个文件两张）', names.length === 2, names.join('|'))

  // ...and the chip is now the way TO the run.
  if (!(await smoke.waitFor('.oneoff-panel', '评审开始即打开面板', 8000))) return smoke.finish()
  if (!smoke.click('.oneoff-toggle')) return smoke.fail('关掉面板', '按钮找不到')
  if (!(await smoke.waitFor(() => !smoke.q('.oneoff-panel'), '面板已收起', 5000))) return smoke.finish()
  after.click()
  const reopened = await smoke.waitFor('.oneoff-panel', '点「查看」重新打开面板', 5000)
  smoke.check('完成后 chip 变成回到面板的入口', !!reopened, '')

  // The panel opens on 过程 and is showing THIS review, with the reply the mock produced.
  const replay = await smoke.waitFor(
    () => smoke.text('.oneoff-body').indexOf('评审完成：1 条意见。') >= 0 ? smoke.text('.oneoff-body') : null,
    '面板回放这次评审', 8000)
  smoke.check('面板回放出这次评审的回复', !!replay, (replay || '').slice(0, 80))

  // P3: the 意见 pane pairs the run's OWN findings with the diff of the files it reviewed —
  // 「完整 diff」 lands there. This is the fix for "点完整 diff 什么也看不到".
  const diffChip = smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('完整 diff') >= 0)
  if (!diffChip) return smoke.fail('找到「完整 diff」入口', '按钮找不到')
  diffChip.click()
  if (!(await smoke.waitFor('.oneoff-tab.active', '面板切到意见区', 8000))) return smoke.finish()
  smoke.check('「完整 diff」切到意见 + diff 区', smoke.text('.oneoff-tab.active').indexOf('意见') >= 0, smoke.text('.oneoff-tab.active'))
  const pane = await smoke.waitFor(() => smoke.text('.oneoff-body').indexOf('NOTES.md') >= 0 ? smoke.text('.oneoff-body') : null,
    '意见区显示被评审的文件 diff', 10000)
  smoke.check('意见区里有被评审文件的 diff', !!pane && pane.indexOf('NOTES.md') >= 0, (pane || '').slice(0, 100))
  smoke.check('意见挂在 diff 行上（有 finding 行）', !!smoke.q('.oneoff-body .finding'), '')
  smoke.check('意见区报出条数', (pane || '').indexOf('评审意见 1 条') >= 0, '')

  // The pane needs the FILE GROUPS, not just the text: the diff is a backend round trip of its
  // own, and the finding's own path (「其它文件」 renders it too) used to satisfy the wait above
  // while the diff itself was still on its way — so the fold assertions below ran against a
  // pane that had nothing to fold.
  const groups = await smoke.waitFor(() => (smoke.qa('.diff-file').length >= 2 ? smoke.qa('.diff-file') : null),
    '意见区渲染出被评审文件的 diff 组', 15000)
  if (!groups) {
    smoke.log('诊断：面板此刻的状态', smoke.text('.oneoff-body').slice(0, 200))
    return smoke.finish()
  }

  // ── The file groups, their fold, and the walk between findings ───────────────
  // The turn wrote two files and the review reported ONE finding, so the two rules can be
  // told apart: a file with a finding opens (the review is what the reader came for), one
  // without stays shut (its diff is context, not news).
  // Groups are found by their path, not by index: the order is git's, not the driver's.
  const group = (name) => smoke.qa('.diff-file').find((g) => smoke.text(g.querySelector('.diff-path')).indexOf(name) >= 0)
  const notes = group('NOTES.md')
  const other = group('OTHER.md')
  smoke.check('diff 里有两个被评审的文件', !!notes && !!other,
    smoke.allText('.diff-file .diff-path').join(' | '))
  smoke.check('有意见的文件默认展开', !!notes && !!notes.querySelector('.diff-line'), '')
  smoke.check('没意见的文件默认折叠', !!other && !other.querySelector('.diff-line'), '')
  const otherToggle = other && other.querySelector('.diff-file-toggle')
  smoke.check('折叠态报给无障碍层', !!otherToggle && otherToggle.getAttribute('aria-expanded') === 'false',
    otherToggle ? 'aria-expanded=' + otherToggle.getAttribute('aria-expanded') : '(无按钮)')

  // Folding is the reader's decision too: collapsing the file with the finding drops its rows…
  const notesToggle = notes && notes.querySelector('.diff-file-toggle')
  if (!notesToggle) return smoke.fail('找到 NOTES.md 的折叠按钮', '按钮找不到')
  notesToggle.click()
  if (!(await smoke.waitFor(() => {
    const g = group('NOTES.md')
    return g && !g.querySelector('.diff-line')
  }, '折叠 NOTES.md 后不再渲染差异行', 4000))) return smoke.finish()

  // …and the ↑/↓ walk has to UNFOLD it before it can land on a finding inside (the two
  // features meet here: a jump target in a folded file has no row to scroll to).
  const jumpDown = smoke.qa('.diff-jump-btn').find((b) => (b.getAttribute('aria-label') || '') === '下一条意见')
  if (!jumpDown) return smoke.fail('意见区有「下一条意见」按钮', smoke.allText('.diff-jump-btn').join('|'))
  jumpDown.click()
  const landed = await smoke.waitFor(() => {
    const g = group('NOTES.md')
    const row = g && g.querySelector('.finding.is-jump-target')
    return row ? { open: !!g.querySelector('.diff-line'), row } : null
  }, '下一条意见把目标行标出来', 5000)
  smoke.check('跳转先展开所属文件，再落到那一条', !!landed && landed.open, landed ? 'open=' + landed.open : '')
  smoke.check('跳转计数报出位置', smoke.text('.diff-jump-pos') === '1/1', smoke.text('.diff-jump-pos'))

  // The send bar: the action sits at the LEFT end, so a long diff (whose horizontal scroll
  // used to carry the right end of the bar off-screen) cannot hide the way out.
  const go = smoke.q('.diff-sendbar-go')
  smoke.check('发出口的文案是「发给 Tachi」', !!go && smoke.text(go) === '发给 Tachi', go ? smoke.text(go) : '(无按钮)')
  const bodyEl = smoke.q('.oneoff-body')
  if (bodyEl && go) {
    bodyEl.scrollLeft = bodyEl.scrollWidth // harmless when the pane does not scroll sideways
    const b = bodyEl.getBoundingClientRect()
    const g = go.getBoundingClientRect()
    smoke.check('横向滚到底后按钮仍在面板可视区内', g.left >= b.left - 1 && g.right <= b.right + 1,
      `button=[${Math.round(g.left)},${Math.round(g.right)}] pane=[${Math.round(b.left)},${Math.round(b.right)}]` +
      ` scrollW/${bodyEl.clientWidth}=${bodyEl.scrollWidth}`)
  }

  // Re-running from the pane: the record carries the scope, so "review these files again" also
  // works from a historical entry. The mock's second review reports TWO findings, which is how
  // the chip proves it is showing the new run rather than the old one.
  const rerun = smoke.qa('.oneoff-body .diff-open').find((b) => b.textContent.indexOf('重新评审') >= 0)
  if (!rerun) return smoke.fail('意见区有「重新评审」按钮', '按钮找不到')
  rerun.click()
  const again = await smoke.waitFor(
    () => smoke.qa('.diff-chip').find((b) => b.textContent.indexOf('已评审 2 条') >= 0) || null,
    '重跑后 chip 报出新的条数', 30000)
  smoke.check('按同一范围重跑，chip 报出新条数（2 条）', !!again,
    again ? smoke.text(again) : smoke.qa('.diff-chip').map((b) => b.textContent).join(' | '))

  // The walk over the NEW run's payload: two findings, so it can be seen to advance — and to
  // start from the beginning again, because the position belongs to the run (the pane keeps
  // its state across a switch in the switcher, so a stale index would land mid-list).
  if (!(await smoke.waitFor(
    () => (smoke.text('.oneoff-body').indexOf('评审意见 2 条') >= 0 ? smoke.text('.oneoff-body') : null),
    '意见区换成重跑那一次的 2 条意见', 15000))) return smoke.finish()
  const next2 = smoke.qa('.diff-jump-btn').find((b) => (b.getAttribute('aria-label') || '') === '下一条意见')
  if (!next2) return smoke.fail('重跑后仍有「下一条意见」按钮', '按钮找不到')
  next2.click()
  if (!(await smoke.waitFor(() => smoke.text('.diff-jump-pos') === '1/2', '走到第 1 条', 5000))) return smoke.finish()
  smoke.check('新一次评审的走势从第 1 条重新开始', true, smoke.text('.diff-jump-pos'))
  next2.click()
  const last = await smoke.waitFor(() => {
    const hit = smoke.q('.finding[data-finding-idx="1"].is-jump-target')
    return smoke.text('.diff-jump-pos') === '2/2' && hit ? smoke.text('.diff-jump-pos') : null
  }, '走到第 2 条并把高亮落在它身上', 5000)
  smoke.check('再走一步到第 2 条（计数与高亮指向同一条）', !!last, smoke.text('.diff-jump-pos'))

  // ── The walk has to stay reachable while the pane is SCROLLED ─────────────────
  // This is what the ↑/↓ pair's home is about. The second finding sits 60 lines down a file of
  // 80, so the jump scrolls the pane — exactly the state in which a header-hosted pair was off
  // the top of the screen and the reader had to scroll back up to jump again
  // (「一往下跳箭头就看不到了」). The bar it lives in now is sticky, so it does not move.
  const scrolled = await smoke.waitFor(() => {
    const pane = smoke.q('.oneoff-body')
    return pane && pane.scrollTop > 0 ? Math.round(pane.scrollTop) : null
  }, '跳到第 2 条后面板确实滚动了', 4000)
  smoke.check('这次跳转把面板滚了下去（否则下面那条断言没有对象）', scrolled !== null,
    scrolled === null ? 'scrollTop=0，面板没有被滚动' : `scrollTop=${scrolled}`)
  const walkBox = (() => {
    const el = smoke.q('.diff-jump'), pane = smoke.q('.oneoff-body')
    if (!el || !pane) return null
    const a = el.getBoundingClientRect(), p = pane.getBoundingClientRect()
    return { visible: a.top >= p.top - 1 && a.bottom <= p.bottom + 1, a, p }
  })()
  smoke.check('面板滚动后 ↑/↓ 仍在可视区内（它跟着 sticky 浮动条）', !!walkBox && walkBox.visible, walkBox
    ? `jump=[${Math.round(walkBox.a.top)},${Math.round(walkBox.a.bottom)}] pane=[${Math.round(walkBox.p.top)},${Math.round(walkBox.p.bottom)}]`
    : '找不到 .diff-jump 或 .oneoff-body')

  // The resize handle's drag is NOT asserted here. Its fix (the width is written straight to the
  // panel, not through a state update per pointermove) is «左右拖动时很卡», and the property that
  // would show it — "the width follows the pointer in the same task" — cannot be READ from a
  // driver on this machine: with Reduce motion ON, base.css's blanket `transition-duration:
  // 0.01ms !important` makes every property change a real CSSTransition (`transition-property`
  // defaults to `all`), so a same-task rect read returns the OLD width even with the fix in place
  // (measured: inline "380px", computed "420px", `CSSTransition:width`, 380 one frame later). The
  // drag's behaviour is oneoff-panel's business, measured after the commit.

  // ── Esc closes the panel (the × says it does) ─────────────────────────────────
  // The button's own tooltip is 「关闭（Esc）」, and until now nothing in the panel listened: the
  // viewers' Esc handling lives in ViewerOverlay, which this column is not — so the promise was
  // simply false.
  //
  // WHO OWNS THE FIRST PRESS is decided by the reader's focus, and BOTH orderings are pinned
  // here, because the panel yields to a focused field on purpose — and the composer's Esc is the
  // same gesture as the panel's note field: /review leaves the focus IN the composer, exactly
  // where the reader typed it, so the case is the common one, not a corner.
  //   · focus in the composer (typed /review there) → the first Esc leaves the input (the focus
  //     goes to the message area) and the panel stays; the next press closes it;
  //   · focus in the panel's own note field → the same: it blurs, the next press closes;
  //   · nobody claiming it (focus on a div, on the body — where a click on a div leaves it in
  //     WebKit) → one press closes.
  // The handler must therefore not depend on the focus being "in" the panel: it is a
  // window-level listener that only refuses a key somebody else already claimed.
  const panelOpen = () => !!smoke.q('.oneoff-panel')
  const activeLabel = () => {
    const el = document.activeElement
    return el ? (el.className ? '.' + String(el.className).split(' ')[0] : el.tagName) : '(无)'
  }
  const pressEsc = (target) => (target || document.body).dispatchEvent(
    new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))

  // (1) The composer has the focus — where /review left it.
  const composerInput = smoke.q('.composer-input')
  smoke.check('下方输入框在（下面这条要用它）', !!composerInput, '')
  if (composerInput) {
    composerInput.focus()
    pressEsc(composerInput) // a real key event on the field: React's handler runs, then the panel's
    await smoke.sleep(80)
    smoke.check('焦点在下方输入框，第一次 Esc 只离开输入框（面板不关）',
      panelOpen() && document.activeElement !== composerInput,
      `open=${panelOpen()} active=${activeLabel()}`)
    pressEsc() // the focus is on the message area now
    const closed = await smoke.waitFor(() => !panelOpen(), '第二次 Esc 关掉面板', 3000)
    smoke.check('焦点让出这个键之后，一次 Esc 就关掉面板（× 的提示是真的）', !!closed, '')
    if (!smoke.click('.oneoff-toggle')) return smoke.fail('重新打开面板', '按钮找不到')
    if (!(await smoke.waitFor('.oneoff-panel', '重新打开面板（下面那条要用它）', 3000))) return smoke.finish()
  }

  // (2) The focus is in the panel's own note field: it owns the first Esc, so a half-typed
  // note survives the key that closes the pane.
  const noteField = smoke.q('.oneoff-body .finding-comment')
  smoke.check('意见区有补充说明输入框（下面两条要用它）', !!noteField, noteField ? '' : '找不到 .finding-comment')
  if (noteField) {
    noteField.focus()
    pressEsc(noteField)
    await smoke.sleep(60)
    smoke.check('输入框里的第一次 Esc 只退出输入（面板不关）',
      panelOpen() && document.activeElement !== noteField,
      `open=${panelOpen()} active=${activeLabel()}`)
    pressEsc() // focus is on the body now — where a click on a div leaves it too
    const closed = await smoke.waitFor(() => !panelOpen(), '再按一次 Esc 关掉面板', 3000)
    smoke.check('再按一次 Esc 就关掉面板（× 的提示是真的）', !!closed, '')
    // …and put it back: the run's Go-side check reads the persisted "panel is open" flag.
    smoke.click('.oneoff-toggle')
    const reopened = await smoke.waitFor('.oneoff-panel', '重新打开面板', 3000)
    smoke.check('重新打开面板（把「开着」留给落盘的断言）', !!reopened, '')
  }

  smoke.finish()
})()
