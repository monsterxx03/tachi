// skip-dream — 会话右键菜单能把它自己排除在记忆整理之外。
//
// 标记是会话记录上的 skip_dream。整理入口在按域分组之前就把它过滤掉，所以它对 channel /
// desktop / TUI 都生效，而不是某一个前端的规矩；它也活过重启。driver 验菜单文案随状态翻转
// （跳过 ↔ 恢复），Go 侧再验它真的落盘——界面状态活不过重启，磁盘上的字段才是下次整理读的。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const openMenu = async (label) => {
    const row = smoke.q('.session.active')
    if (!row) {
      smoke.fail('找到当前会话行', '没有 .session.active')
      return null
    }
    row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 60, clientY: 200 }))
    return smoke.waitFor('.ctx-menu', label, 3000)
  }
  const item = (text) => smoke.qa('.ctx-item').find((b) => b.textContent.trim() === text)

  if (!(await openMenu('右键菜单出现'))) return smoke.finish()
  smoke.check('默认菜单项是「跳过记忆整理」', !!item('跳过记忆整理'), smoke.allText('.ctx-item').join(' / '))
  item('跳过记忆整理')?.click()
  // The menu closes on click, so the only immediate evidence is this line.
  const skippedToast = await smoke.waitFor('.app-toast', '点完出现一行提示', 2000)
  smoke.check('跳过之后有一行提示', !!skippedToast && smoke.text(skippedToast).includes('已跳过'),
    skippedToast ? smoke.text(skippedToast) : '没有 .app-toast')

  if (!(await openMenu('跳过之后再次打开菜单'))) return smoke.finish()
  smoke.check('跳过之后菜单项变成「恢复记忆整理」', !!item('恢复记忆整理'), smoke.allText('.ctx-item').join(' / '))
  item('恢复记忆整理')?.click()
  const restoredToast = await smoke.waitFor(() => (smoke.text(smoke.q('.app-toast')) || '').includes('已恢复'), '恢复之后有一行提示', 2000)
  smoke.check('恢复之后有一行提示', !!restoredToast, restoredToast ? '' : '提示没有变成「已恢复」')

  if (!(await openMenu('恢复之后再次打开菜单'))) return smoke.finish()
  smoke.check('恢复之后菜单项变回「跳过记忆整理」', !!item('跳过记忆整理'), smoke.allText('.ctx-item').join(' / '))
  // 最后停在「已跳过」：Go 侧要读 meta.json 验证这个状态。
  item('跳过记忆整理')?.click()

  smoke.finish()
})()
