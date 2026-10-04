// chain-orphan — a conversation that WAS compacted, whose pre-compaction half is not in the list.
//
// That is what a parent deleted by hand (or swept by session cleanup's max_keep) leaves behind: the
// child's record still links to it, but the sidebar has no row to fold under the child, and the
// conversation used to come back looking like one that had never been compacted. The row has to say
// what became of the other half — the child's own record carries the parent's title for exactly
// this — and it has to be a NOTE rather than a control: there is nothing left to unfold.
//
// The fixture is seeded by the runner (main.go's orphanChain), because a link whose other end does
// not exist is a session record no driver can write.
;(async () => {
  if (!(await smoke.waitFor('.session-list .session', '侧栏出现会话行'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const rows = () => smoke.qa('.session')
  const target = rows().filter((r) => smoke.text(r).indexOf('压缩过的会话') >= 0)[0]
  smoke.check('被压缩过的那条会话就在侧栏里', !!target, rows().map((r) => smoke.text(r)).join(' | '))
  if (!target) return smoke.finish()

  const note = await smoke.waitFor('.session-chain-gone', '压缩前那一节的说明出现', 5000)
  if (!note) return smoke.finish()
  smoke.check('说明写的是压缩前那一节（标题来自子会话自己的记录）',
    smoke.text(note).indexOf('上一次对话') >= 0, smoke.text(note))
  smoke.check('说明写出了它已经不在列表里', smoke.text(note).indexOf('不在列表') >= 0, smoke.text(note))

  // A NOTE, not an opener: nothing here can be unfolded, so there is no disclosure control at all —
  // an entry that looks clickable and does nothing is the failure this scenario exists to prevent.
  smoke.check('它是说明，不是可展开的入口', smoke.qa('.session-chain').length === 0,
    smoke.qa('.session-chain').length + ' 个展开入口')
  smoke.check('说明本身不可点', !!(note && note.tagName !== 'BUTTON'), note ? note.tagName : '')

  // …and the row itself is an ordinary conversation: the orphaned half does not cost the reader the
  // session they still have.
  if (!smoke.click(target)) return smoke.fail('点击这一行', '找不到行')
  const active = await smoke.waitFor(
    () => (smoke.text('.session.active').indexOf('压缩过的会话') >= 0 ? smoke.q('.session.active') : null),
    '切到这条会话', 10000)
  smoke.check('这一行照样能打开', !!active, active ? smoke.text(active) : '')

  smoke.finish()
})()
