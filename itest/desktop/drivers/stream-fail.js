// stream-fail — 一轮 turn 的流在工具跑完之后断掉时，那一轮已经执行的步骤不能丢（issue #38）。
//
// 真正要证的是**模型在下一轮还能看见那些步骤**，而这件事只有请求里看得出来 —— 所以 driver 只负责
// 把两个状态走到：出错上屏、出错之后还能再发一条。Go 侧读第 3 个请求，断言里面带着第 1 轮那条工具
// 结果（并先断言第 2 个请求里也有它，说明它真的执行过、不是空 fixture）。
//
// 分界线值得写下来：**界面还看得见那张卡不算数**。卡片是前端自己的缓存（tool_call_start/tool_result
// 事件流出来的），就算后端把历史丢了，界面照样显示。丢的是下一轮发给模型的上下文。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // ── 第一轮：跑一条命令（这会是一个成功的工具调用，然后同一次 turn 的下一个请求断流）────
  smoke.type('.composer-input', '先跑一条命令')
  smoke.click('.send-btn')
  // 过程条先说"1 步"：那一步是 mock 的 Bash，真的执行过。
  if (!(await smoke.waitFor(
    () => { const t = smoke.text('.process-head'); return t.indexOf('1 步') >= 0 ? t : null },
    '工具真的跑了一步', 30000))) return smoke.finish()

  // ── 流断掉：界面上是那条 ⚠ 行（硬错误按"给最新气泡补一个正文 part"落地）─────────
  const failed = await smoke.waitFor(
    () => { const t = smoke.text('.chat'); return t.indexOf('⚠') >= 0 ? t : null },
    '断流之后界面上报出错误', 20000)
  smoke.check('断流在界面上说明白了（不是安静地停住）', !!failed,
    (failed || '').slice(-120))
  smoke.check('出错之后按钮回到可发送（不卡在"停止"）', !smoke.q('.stop-btn'), '')

  // 已经执行的那一步仍然在屏幕上 —— 注意这条**不能**当作历史的证明（前端有自己的缓存，见文件头）。
  const card = await smoke.waitFor(
    () => smoke.qa('.process-head').length > 0 ? smoke.q('.process-head') : null, '过程条还在')
  smoke.check('出错之后那一轮的步骤仍在界面上（前端缓存，不是历史）',
    !!card && smoke.text(card).indexOf('1 步') >= 0, smoke.text(card) || '')

  // ── 第二轮：出错之后必须还能接着对话 ─────────────────────────────────────
  // 这一条本身也是断言：一轮断流不该让会话变成不可用（状态机停在"出错"时也要能开下一轮）。
  //
  // 第一次发送可能正好撞上这一轮**收尾**的那几百毫秒：agent 在关掉事件流之前要先
  // `endCheckpointTurn`（给这一轮改过的东西落一个快照），而界面上的「出错」在更早就亮了 ——
  // 于是后端回一句「会话正在运行，这条消息没有发出去」，页面把它渲染成一行 notice。
  //
  // 那不是这条场景要测的东西（后端守着会话、不许两个 turn 并行，是对的），而读者遇到那一行也只会
  // 照它说的再发一次 —— 所以 driver 也重发。判据是 notice 的**条数**变了（那一行会留在转写里）。
  const refusals = () => (smoke.text('.chat').match(/这条消息没有发出去/g) || []).length
  const sendUntilAccepted = async (text) => {
    for (let attempt = 1; attempt <= 5; attempt++) {
      const before = refusals()
      smoke.type('.composer-input', text)
      smoke.click('.send-btn')
      for (let i = 0; i < 30; i++) {
        await smoke.sleep(200)
        if (smoke.text('.chat').indexOf('接着来。') >= 0) return attempt
        if (refusals() > before) break // 后端说这一轮还在收尾：重发
      }
    }
    return 0
  }
  const attempt = await sendUntilAccepted('接着来')
  smoke.check('断流之后能接着发下一条（会话没有卡住）', attempt > 0,
    attempt > 1 ? `第 ${attempt} 次才发出（前面撞上了收尾）` : '')

  smoke.finish()
})()
