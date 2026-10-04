// api-error-mid-turn — 一轮 turn 的下一次请求被 API 拒了（400）时，这一轮已经执行的步骤不能丢。
//
// 与 stream-fail.js 是同一个部位的两半：那一个断的是**流**（工具跑完之后流坏掉），这一个断的是**请求**
// 自己（HTTP 状态错误，流根本没建立）。界面上两者长得一样（都是 ⚠ 行 + 「出错」），走的后端路径却是
// 两条 —— 所以两边都要有场景，而真正要证的是同一件事：**模型在下一轮还能看见那些步骤**。判据在 Go 侧
// （读第 3 个请求），driver 只负责把两个状态走到：出错上屏、出错之后还能再发一条。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // ── 第一轮：跑一条命令（这会是一个成功的工具调用，然后同一次 turn 的下一个请求被拒）────
  smoke.type('.composer-input', '先跑一条命令')
  smoke.click('.send-btn')
  if (!(await smoke.waitFor(
    () => { const t = smoke.text('.process-head'); return t.indexOf('1 步') >= 0 ? t : null },
    '工具真的跑了一步', 30000))) return smoke.finish()

  // ── 请求被拒：界面上是那条 ⚠ 行 ─────────────────────────────────────────────
  const failed = await smoke.waitFor(
    () => { const t = smoke.text('.chat'); return t.indexOf('⚠') >= 0 ? t : null },
    '被拒之后界面上报出错误', 20000)
  smoke.check('请求被拒在界面上说明白了（不是安静地停住）', !!failed,
    (failed || '').slice(-120))
  smoke.check('出错之后按钮回到可发送（不卡在"停止"）', !smoke.q('.stop-btn'), '')

  // ── 第二轮：出错之后必须还能接着对话 ─────────────────────────────────────
  // 重发的那套（第一次可能撞上这一轮收尾的那几百毫秒，后端会回一句「会话正在运行」）与 stream-fail.js
  // 完全同理，理由写在那个文件里，这里不重复。
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
  smoke.check('请求被拒之后能接着发下一条（会话没有卡住）', attempt > 0,
    attempt > 1 ? `第 ${attempt} 次才发出（前面撞上了收尾）` : '')

  smoke.finish()
})()
