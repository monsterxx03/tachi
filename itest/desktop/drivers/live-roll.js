// live-roll — 跑着的那几行字必须"滚动"着换状态，而不是瞬切。
//
// 切换最快的是助手气泡里的运行占位行：模型每在"思考"与"起工具"之间来回一次，它就换一次文案
// （正在思考… ↔ 正在执行…）。模型数据一快，瞬切看起来就是不相干的词闪过去 —— 滚动才读得出
// "同一个东西在换内容"。过程条的实时行同理（正在 X · 第 N 步）。
//
// 断言分两半：
//   - 结构：这些文字必须装在会滚动的盒子里（瞬切也能过其他断言，所以这是唯一能钉住"滚动"存在的
//     地方）；
//   - 行为：每一次文本变化，都要看到动画真的在跑 —— "每一次"是这半句的全部意思：只要求"看到过
//     动画"的话，一次滚动 + 九次瞬切照样通过。
//
// reduced-motion 不是"不判"：base.css 换的是动效的种类（位移→淡入，见那里的 .roll 一条），所以
// 这里两种模式下都判"每次切换有动效"，只是要求的名字不同 —— 本机开着 reduced-motion，跑到的是
// 淡入那一支；关掉该设置的机器跑的是 roll-up 那一支，判法一样。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  // 编辑器比会话先出现：启动时那是个空壳（currentId 还没选上），此时打进去的字会落进一个没人
  // 渲染的桶里 —— 发送键一样会亮，点下去输入框清空、没有请求也没有消息，看起来像"turn 没起来"。
  // 标题栏的会话 id 才是"应用已经选好会话"（同 session-running.js 的等待），先等它再动手。
  if (!(await smoke.waitFor('.session-id', '会话加载完成（标题栏出现会话 id）'))) return smoke.finish()

  smoke.type('.composer-input', '跑一条命令，再回来说一声')
  // 发送这一步单独判一句：编辑器是受控输入，文字若没进 React 状态，发送按钮就一直 disabled，
  // 点它什么也不会发生 —— 而失败会以"占位行没出现"的样子出现，把真因埋掉 20 秒。
  if (!(await smoke.waitFor(() => { const b = smoke.q('.send-btn'); return b && !b.disabled }, '发送按钮可用（文字进了编辑器）', 5000))) {
    const ta = smoke.q('.composer-input')
    smoke.check('编辑器真的收到了文字', false, 'textarea value=' + JSON.stringify(ta ? ta.value : null))
    return smoke.finish()
  }
  smoke.click('.send-btn')

  // ① 运行占位行出现，文字在滚动盒里
  if (!(await smoke.waitFor('.msg-assistant .running', '运行占位行出现', 20000))) return smoke.finish()
  smoke.check('占位行文字在滚动盒里', !!smoke.q('.msg-assistant .running .roll'),
    smoke.q('.msg-assistant .running') ? smoke.q('.msg-assistant .running').outerHTML.slice(0, 140) : '找不到 .running')

  // 这台机器开不开 reduced-motion 决定该看到哪个动画名，所以先问清楚。base.css 用的是
  // matchMedia 认的那个设置，这里问的是同一件事。
  const reduced = !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches)
  const want = reduced ? 'roll-in' : 'roll-up'
  smoke.log('动效模式', (reduced ? 'reduced-motion：位移换成淡入' : '常规：滚动') + '（本次要求 ' + want + '）')

  const box = () => smoke.q('.msg-assistant .running')
  const inner = () => smoke.q('.msg-assistant .running .roll-inner')

  // 采样"到场的那一行"，不是整个盒子。滚动时盒子里同时有两行（正在离场的、刚到场的），盒子的
  // 文本因此是"新状态 + 上一个状态"；按盒子采样会把每次切换数成两次（滚动开始一次、离场行被
  // 丢掉时一次），再要求两次都有动画 —— 那读数只会是"切换次数比动画次数多一倍"。:last-child
  // 恰好就是刚到的那一行，也就是当前状态。
  const stateOf = () => {
    if (!box()) return null // 回合结束，占位行收掉
    const el = smoke.q('.msg-assistant .running .roll-line:last-child')
    return el ? smoke.text(el) : smoke.text(box())
  }
  const animNames = () => {
    const el = inner()
    if (!el || !el.getAnimations) return []
    return el.getAnimations().filter((a) => a.playState === 'running').map((a) => a.animationName || 'css')
  }

  // flips：每一次状态切换一条，随后自己认领一次动画。动画是随这次提交建立的（180ms 滚动 /
  // 140ms 淡入），而最小停留是 260ms，所以"变化后 200ms 内"必然整个落在这一次的动画窗口里，
  // 不会被下一次切换的动画顶替。每条都带一个相对发送的毫秒数：判词用不到它，但"为什么多出一次
  // 切换"这种时序问题只能靠它回答。
  const t0 = Date.now()
  const flips = []
  const seen = []
  let last = stateOf()
  if (last !== null) seen.push(last)
  const watch = setInterval(() => {
    const now = stateOf()
    if (now === null) return
    if (now !== last) {
      flips.push({ from: last, to: now, at: Date.now(), ts: Date.now() - t0, roll: '' })
      seen.push(now)
      last = now
    }
    const cur = flips.length ? flips[flips.length - 1] : null
    if (!cur || cur.roll) return
    const names = animNames()
    if (names.length) cur.roll = names.join(',')
  }, 25)

  // ② 工具那段：过程条的实时行也在滚动盒里（同一套机制的另一处使用点）
  if (!(await smoke.waitFor('.process-head.live .process-now', '过程条实时行出现', 25000))) return smoke.finish()
  smoke.check('过程条实时行也在滚动盒里', !!smoke.q('.process-now.roll'),
    smoke.q('.process-head.live') ? smoke.q('.process-head.live').outerHTML.slice(0, 160) : '找不到 .process-head.live')
  // 实时行的标题（命令原文）随 agent:tool 的第二个事件到，比实时行本身晚，所以要等它 —— 采样太早
  // 会把"还没到"读成"没有"。
  const titled = await smoke.waitFor(() => smoke.text('.process-now').indexOf('sleep') >= 0, '实时行报出那条命令', 4000)
  smoke.check('过程条实时行报的是那条命令', !!titled, smoke.text('.process-now'))

  // ③ 回合收尾（工具跑完，模型再回一句话）
  if (!(await smoke.waitText('.msg-assistant .msg-content', '两步都跑完了', 40000))) return smoke.finish()
  await smoke.sleep(300)
  clearInterval(watch)

  const trail = flips.map((f) => (f.ts / 1000).toFixed(1) + 's ' + f.from + '→' + f.to + ' ' + (f.roll || '✗无动效')).join(' ｜ ')
  const quiet = flips.filter((f) => !f.roll)
  const misnamed = flips.filter((f) => f.roll && f.roll.indexOf(want) < 0)
  // 场景保证至少两次翻转：起工具→执行，工具完模型再说话→又回思考。实测还有第三次：占位行刚出现
  // 那一刻报的是"正在执行…"，第一个事件到达才纠正成"正在思考…"（App.tsx 的 runningLabel 把
  // "非 thinking"一律读成"正在执行"，包括回合还没开始的 idle）。所以这里只要求 >= 2，不钉次数。
  smoke.check('占位行走过思考与执行两种状态（有东西可换）',
    seen.indexOf('正在思考…') >= 0 && seen.indexOf('正在执行…') >= 0, seen.join('→') + ' ｜ ' + trail)
  smoke.check('占位行切换过状态', flips.length >= 2, flips.length + ' 次切换 ｜ ' + trail)
  smoke.check('每次切换都有动效（不是瞬切）', flips.length >= 2 && quiet.length === 0,
    quiet.length + '/' + flips.length + ' 次切换没看到动效 ｜ ' + trail)
  smoke.check(reduced ? 'reduced-motion 下动效是淡入，不是位移' : '常规模式下动效是滚动（roll-up）',
    flips.length >= 2 && misnamed.length === 0,
    misnamed.length + '/' + flips.length + ' 次切换动效名不对 ｜ ' + trail)

  smoke.finish()
})()
