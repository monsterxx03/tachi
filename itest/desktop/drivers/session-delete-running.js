// session-delete-running — 运行中的会话不给删：右键菜单里「删除」是禁用的，并给出原因。
//
// 这一侧只能验可见的规则（禁用的按钮点不动，所以"拒绝了"这件事从这里验不到）；真正的执行力在
// Go 单测 TestDeleteSessionRefusesARunningTurn —— 菜单可能是回合开始之前开出来的，那时只能靠
// 后端拦。两半合起来才是这条规则：前端提示，后端说了算。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '跑很久')
  smoke.click('.send-btn')
  // 会话真的在跑，才有"运行中"这个前提。
  if (!(await smoke.waitFor('.stop-btn', '第一轮开始运行（停止按钮出现）'))) return smoke.finish()

  const row = smoke.q('.session.active')
  if (!row) return smoke.fail('找到当前会话行', '找不到 .session.active')
  row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 60, clientY: 200 }))
  const menu = await smoke.waitFor('.ctx-menu', '会话行的右键菜单出现', 3000)
  if (!menu) return smoke.finish()

  const items = smoke.qa('.ctx-item')
  const del = items.find((b) => b.textContent.trim() === '删除')
  smoke.check('菜单里仍有「删除」这一项', !!del, smoke.allText('.ctx-item').join(' / '))
  smoke.check('运行中的会话：删除被禁用', !!del && del.disabled, del ? `disabled=${del.disabled}` : '')
  smoke.check('禁用时说明原因（title 里说"正在运行"）',
    !!del && (del.title || '').indexOf('正在运行') >= 0, del ? del.title : '')
  smoke.check('只有删除被禁用，其余项照常',
    items.length === 3 && items.filter((b) => b.textContent.trim() !== '删除').every((b) => !b.disabled),
    items.map((b) => `${b.textContent.trim()}:${b.disabled}`).join(' / '))
  smoke.finish()
})()
