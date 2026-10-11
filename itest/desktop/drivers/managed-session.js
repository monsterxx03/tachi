// managed-session — 一条子系统的执行记录（接收侧节点的 A2A 执行）在桌面上长什么样。
//
// 为什么要在界面上钉：这类会话的标题是从远端 prompt 生成的，行上不标，读者就会把别人机器上的
// 一次执行当成自己开的对话；而它又是**启动恢复的那个会话**（fixture 预置），所以第一眼看到的
// 就是它。真正的边界不在前端——任何从这里发出的回合都会被 agent 入口拒掉（agent/managed.go），
// 桌面的义务是不要摆出一个点了必然失败的输入框。
//
// 四问：行上有没有来源标注；顶部有没有横幅；输入框在不在；会话内容还读不读得到。
;(async () => {
  if (!(await smoke.waitForSession())) return smoke.finish()

  // ── 1. 侧栏那一行 ───────────────────────────────────────────────────────
  const tags = smoke.qa('.session .session-tag.is-managed')
  smoke.check('侧栏行上有来源标注', tags.length === 1, tags.length + ' 个')
  const tagText = tags.length ? smoke.text(tags[0]) : ''
  smoke.check('标注写的是子系统的执行记录', tagText.indexOf('子系统执行记录') >= 0, tagText)
  smoke.check('标注带 kind', tagText.indexOf('a2a_execution') >= 0, tagText)
  smoke.check('标注说明只读', tagText.indexOf('只读') >= 0, tagText)
  // 来源：本机 pin 过的对端才有名字，而沙箱没有对端目录，所以这一行说的是「未知节点 + id 前 8 位」。
  // 那句话本身就是要点：名字只能来自本机 pin 住的记录（对端自报的名字不算数），而没有记录时如实
  // 说不知道，比编一个名字或干脆不提都强。
  smoke.check('标注写明来自哪台机器', tagText.indexOf('来自 未知节点 cd726b44') >= 0, tagText)

  // ── 2. 顶部横幅（读者可能根本没看侧栏）────────────────────────────────────
  const banner = await smoke.waitFor('.managed-banner', '只读横幅出现')
  if (!banner) return smoke.finish()
  const bannerText = smoke.text(banner)
  smoke.check('横幅说明这是子系统的记录', bannerText.indexOf('子系统执行记录') >= 0, bannerText)
  smoke.check('横幅说明不能继续/回退/重试', bannerText.indexOf('不能继续') >= 0, bannerText)
  smoke.check('横幅也写明来自哪台机器', bannerText.indexOf('来自 未知节点 cd726b44') >= 0, bannerText)

  // ── 2b. 横幅和转写区是**上下**关系，不是并排 ──────────────────────────────
  //
  // 存在性与文案都对、排版照样能坏：横幅一度被放进 .chat-wrap（一个默认横排的 flex 容器，
  // 里面装着滚动区以及锚在它上面的浮动件），于是它凭 flex-shrink: 0 独占了左半屏、把消息挤成
  // 右边一条。18 项断言全过，因为没人问过"它在不在该在的位置"。几何断言就是那一问。
  const chat = smoke.q('.chat')
  const b = banner.getBoundingClientRect()
  const c = chat.getBoundingClientRect()
  smoke.check('横幅在转写区上方（不是并排）', Math.round(b.bottom) <= Math.round(c.top) + 1,
    `banner.bottom=${Math.round(b.bottom)} chat.top=${Math.round(c.top)}`)
  smoke.check('横幅与转写区同一条左边界', Math.round(b.left) === Math.round(c.left),
    `banner.left=${Math.round(b.left)} chat.left=${Math.round(c.left)}`)

  // ── 3. 没有输入框，取而代之的是同一句原因 ─────────────────────────────────
  smoke.check('受管会话上不存在输入框', smoke.qa('.composer-input').length === 0,
    smoke.qa('.composer-input').length + ' 个 .composer-input')
  const ro = smoke.qa('.composer-readonly')
  smoke.check('输入框的位置写着只读原因', ro.length === 1, ro.length + ' 个 .composer-readonly')
  smoke.check('原因里带 kind', ro.length > 0 && smoke.text(ro[0]).indexOf('a2a_execution') >= 0,
    ro.length ? smoke.text(ro[0]) : '')
  smoke.check('原因里也写明来源', ro.length > 0 && smoke.text(ro[0]).indexOf('来自 未知节点 cd726b44') >= 0,
    ro.length ? smoke.text(ro[0]) : '')

  // ── 4. 内容照常可读（只读不等于看不见）────────────────────────────────────
  const transcript = smoke.text('.chat-content')
  smoke.check('会话内容仍然可见（用户那轮）', transcript.indexOf('检查昨天的备份') >= 0, transcript.slice(0, 120))
  smoke.check('会话内容仍然可见（回答那轮）', transcript.indexOf('发现两个目录未完成备份') >= 0,
    transcript.slice(0, 120))

  smoke.finish()
})()
