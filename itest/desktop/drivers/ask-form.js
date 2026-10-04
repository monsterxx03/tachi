// ask-form — the AskUserQuestion form the agent parks a turn on, and the one keystroke that
// finishes it: Enter inside the free-text field.
//
// The form is inline in the transcript (it takes the place of the pending AskUserQuestion tool
// card), with option rows and a single-line free-text box. Enter in that box has no other job —
// the box cannot hold a newline — so it submits, and this scenario is what holds that to it: the
// reader types their own answer and presses Enter once, then the form is gone and the turn runs
// on. ⌘↩ still submits from anywhere, so the assertion is on the BARE key.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  smoke.type('.composer-input', '需要你确认一下')
  // The send button's own state is the evidence that the text reached React: it is disabled while
  // the box is empty, so reporting both here separates "the app ignored the send" from "the click
  // landed on a disabled button" (which reads identically from the outside).
  const btn = smoke.q('.send-btn')
  smoke.log('发送前：输入框 / 按钮',
    JSON.stringify((smoke.q('.composer-input') || {}).value) + ' / disabled=' + (btn ? btn.disabled : 'missing'))
  smoke.click('.send-btn')

  // The agent's question, parked: the form replaces the tool card that asked it.
  if (!(await smoke.waitFor('.ask-form', '提问表单出现'))) return smoke.finish()
  const q = smoke.q('.ask-form .ask-question')
  smoke.check('表单带着问题本身出现', !!q && smoke.text(q).indexOf('用哪个方案') >= 0, q ? smoke.text(q) : '')
  smoke.check('选项行都在', smoke.qa('.ask-form .ask-option').length === 2, String(smoke.qa('.ask-form .ask-option').length))

  const box = smoke.q('.ask-form .ask-input')
  if (!box) return smoke.fail('自由输入框存在', '找不到 .ask-input')
  smoke.check('自由输入框可以输入（单行，Enter 无处可去）', true)

  // Type the reader's own answer and press Enter — no modifier, no click on 提交.
  if (!smoke.type(box, '两个都不选，按我的来')) return smoke.fail('填入自由回答', 'textarea/input 找不到')
  smoke.key(box, 'Enter')

  // The form is gone and the turn continued: the answer went back through AnswerQuestion.
  if (!(await smoke.waitGone('.ask-form', '表单消失'))) return smoke.finish()
  smoke.check('Enter 提交后表单立刻消失', true)
  if (!(await smoke.waitText('.chat', '收到你的自由回答', 30000))) return smoke.finish()
  smoke.check('回答之后本轮继续跑完', true)

  smoke.finish()
})()
