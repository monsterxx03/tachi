// voice-input — the composer's microphone: ⌘⇧M and the button are one toggle, and the text lands in
// the BOX rather than going out.
//
// The microphone itself is not driven here (a real recording would raise a TCC prompt on the
// machine running the suite and capture the room): the app is launched with TACHI_DEMO_VOICE_TEXT,
// so a finished recording answers with a known line instead of opening the mic. What that leaves
// under test is exactly the half a smoke suite can own — the two states, the two entry points, and
// where the text ends up. The runner asserts the other half from Go: nothing was sent.
;(async () => {
  const VOICE_TEXT = '帮我看一下这个报错'
  const TYPED = '先写几个字，'

  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // The button is only rendered when the backend says this machine can transcribe, so its
  // presence is also the assertion that VoiceInputAvailable() reached the page.
  if (!(await smoke.waitFor('.mic-btn', '麦克风按钮出现（这台机器能转写）', 8000))) return smoke.finish()
  smoke.check('空闲时按钮不是录音态', !smoke.q('.mic-btn.is-recording'), '')

  // Type something first: the dictation has to be SPLICED AT THE CARET (the reader may be halfway
  // through a sentence), and a box that gets replaced instead would still pass a "text is present"
  // check. This is what makes the difference observable.
  smoke.type('.composer-input', TYPED)
  const usersBefore = smoke.qa('.msg-user').length

  smoke.click('.mic-btn')
  if (!(await smoke.waitFor('.mic-btn.is-recording', '按钮进入录音态（后端推来的状态）', 8000))) return smoke.finish()
  smoke.check('点击后按钮进入录音态', true, smoke.q('.mic-btn').className)
  smoke.check('录音中不动输入框', smoke.q('.composer-input').value === TYPED, smoke.q('.composer-input').value)

  smoke.click('.mic-btn')
  const landed = await smoke.waitFor(
    () => {
      const v = smoke.q('.composer-input').value
      return v.indexOf(VOICE_TEXT) >= 0 ? v : null
    },
    '转写结果落进输入框',
    15000
  )
  if (!landed) return smoke.finish()
  smoke.check('转写结果插在已经打好的文字之后', landed.indexOf(TYPED) === 0 && landed.indexOf(VOICE_TEXT) > 0, landed)
  if (!(await smoke.waitGone('.mic-btn.is-recording', '录音态结束（按钮回到空闲）', 8000))) return smoke.finish()

  // The point of the desktop button: it is a DRAFT. The device's voice goes straight out because it
  // has no keyboard; here the reader gets to fix a misheard word first, so nothing may be sent.
  const usersAfter = smoke.qa('.msg-user').length
  smoke.check('没有自动发出（对话里没有多出用户消息）', usersAfter === usersBefore, `${usersBefore} → ${usersAfter}`)
  const send = smoke.q('.send-btn')
  smoke.check('文字确实在框里（发送键因此可用）', !!send && !send.disabled, send ? `disabled=${send.disabled}` : '找不到发送键')

  // ⌘⇧M, the same toggle reached by key. It is dispatched from wherever the focus is (the composer,
  // which insertAtCaret just focused) on purpose: the shortcut has to work WHILE the reader is
  // typing — that is the whole point of dictating into a draft.
  smoke.key(null, 'M', { metaKey: true, shiftKey: true })
  if (!(await smoke.waitFor('.mic-btn.is-recording', '快捷键进入录音态', 8000))) return smoke.finish()
  smoke.check('⌘⇧M 开始录音（与按钮同一个切换）', true, '')
  smoke.key(null, 'M', { metaKey: true, shiftKey: true })
  const twice = await smoke.waitFor(
    () => {
      const v = smoke.q('.composer-input').value
      return v.split(VOICE_TEXT).length - 1 >= 2 ? v : null
    },
    '快捷键结束后文字再落一次',
    15000
  )
  if (!twice) return smoke.finish()
  smoke.check('⌘⇧M 结束录音，文字插在光标处', true, twice)
  smoke.check('快捷键这一路也没有发出去', smoke.qa('.msg-user').length === usersAfter, `${usersAfter} → ${smoke.qa('.msg-user').length}`)

  smoke.finish()
})()
