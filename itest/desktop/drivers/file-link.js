// file-link — 回复里指向**本地文件**的链接点下去要开应用内的预览，不是交给系统浏览器（issue #49）。
//
// 现场：工作目录里有一个 README.md，回复给它一个**相对路径**的链接（`[README.md](./README.md)`），
// 另加一个指向不存在路径的链接。相对路径是这条链路的要害：它和正文里的图片一样，相对**会话工作
// 目录**解析（lib.ts 的 linkTargetPath / toLocalAsset 是并列的两条规则）—— 链接渲染器拿不到
// workDir 时，这一条就退化成「点了没反应」，而下面「浮层里装着这个文件的内容」正好只有真的解析
// 对了才可能绿。
//
// 断言分两半。driver 侧（这里）：点 README 那条必须开出预览浮层，且浮层里**真的装着这个文件的
// 内容**（空框也会有 .viewer-overlay，所以「有浮层」本身不算证据）；点不存在那条**不能**开浮层。
// Go 侧（scenarios.go 的 after）：整个过程一次启动器调用都没有 —— 那才是「它没有走浏览器那条路」
// 的证据，因为 launcher 在脚本运行里被换成记录器（desktop/demo.go 的 TACHI_DEMO_OPEN_LOG）。
//
// MARKER 与 scenarios.go 的 fileLinkMarker 必须一致（driver 读不到 Go 常量），和 external-link 的
// LINK、paste-image 的 payload 同一个约定。
const MARKER = 'file-link smoke marker'

;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  smoke.type('.composer-input', '给两个链接')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.chat', '看这个文件', 30000))) return smoke.finish()

  // 按**名字**找（链接上写的字），不按位置：一段话里几个链接只有标签分得开。
  const byLabel = (label) =>
    smoke.qa('.assistant-text a.md-link').filter((a) => smoke.text(a) === label)[0] || null

  const file = await smoke.waitFor(() => byLabel('README.md'), '回答里渲染出指向本地文件的链接', 8000)
  if (!file) return smoke.finish()

  // 它仍是个真链接：路径挂在 href 上（读屏念得出目标、悬停看得到），而 title 说明点下去会发生什么
  // —— 这里点的是预览，不是跳转。
  smoke.check('链接带着它的路径', (file.getAttribute('href') || '') === './README.md',
    file.getAttribute('href') || '(没有 href)')
  smoke.check('悬停能看出点下去是预览',
    (file.getAttribute('title') || '').indexOf('点击预览') >= 0,
    file.getAttribute('title') || '(没有 title)')

  smoke.click(file)
  if (!(await smoke.waitFor('.viewer-overlay .viewer-doc', '本地文件链接开出预览浮层', 8000))) return smoke.finish()
  const shown = smoke.text('.viewer-overlay')
  smoke.check('浮层里是那个文件的内容，不是空框', shown.indexOf(MARKER) >= 0, shown.slice(0, 140))

  // Esc 关掉它（和附件预览同一个浮层、同一个关闭手势）。
  document.body.dispatchEvent(
    new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
  if (!(await smoke.waitFor(() => !smoke.q('.viewer-overlay'), 'Esc 关掉预览浮层', 3000))) return smoke.finish()

  // 指向不存在路径的链接：不开浮层。这一条只能「等一小会儿再断它没发生」——要断的是一个**非事件**，
  // waitFor 断不了。上面的正向断言是同一次运行里同一手势的对照，所以它不是一条永远为真的断言。
  // 它没开浮层之后的去向（落到 OpenURL，被后端按 scheme 拒掉）由 Go 侧的启动器记录兜底。
  const missing = byLabel('没有这个')
  if (!missing) return smoke.fail('回答里渲染出指向不存在路径的链接', smoke.text('.assistant-text'))
  smoke.click(missing)
  await smoke.sleep(600)
  smoke.check('路径不存在时不开浮层（按老行为交给后端去拒）', !smoke.q('.viewer-overlay'), '')
  smoke.finish()
})()
