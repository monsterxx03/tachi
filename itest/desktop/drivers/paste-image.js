// paste-image — a screenshot pasted into the box must reach the model as an image, and the box must
// show it as a PICTURE rather than a path.
//
// The clipboard is simulated IN THE PAGE: a real `paste` event carrying PNG bytes, which is exactly
// what the clipboard holds after a screenshot (bytes with no path — the reason the composer stores
// it through a binding instead of splicing a path in). The Go side then asserts those bytes arrived
// at the LLM boundary; this half proves the gesture works, that the box shows a thumbnail with no
// `@/path` in the text, and that the sent bubble draws the picture.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // Must stay identical to pasteImageB64 in scenarios.go — the Go side looks for exactly these
  // bytes in the request.
  const b64 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=='
  const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))
  const file = new File([bytes], 'screenshot.png', { type: 'image/png' })
  const dt = new DataTransfer()
  dt.items.add(file)

  const box = smoke.q('.composer-input')
  if (!box) return smoke.fail('取输入框', '找不到 .composer-input')
  box.focus()
  const dispatched = box.dispatchEvent(new ClipboardEvent('paste', {
    clipboardData: dt, bubbles: true, cancelable: true,
  }))
  smoke.log('粘贴事件已派发', `dispatchEvent=${dispatched}`)

  // The paste lands as an attachment: a thumbnail in the strip, addressed through /local (the same
  // asset route markdown images use) — not as text.
  const thumb = await smoke.waitFor('.composer-attachment img', '粘贴得到一张缩略图', 8000)
  if (!thumb) {
    smoke.log('输入框此刻的值', JSON.stringify((smoke.q('.composer-input') || {}).value || ''))
    smoke.log('是否出现了失败提示', smoke.text('.paste-notice') || '(没有)')
    return smoke.finish()
  }
  smoke.check('缩略图指向存下来的那张图', (thumb.getAttribute('src') || '').indexOf('/local?p=') === 0,
    thumb.getAttribute('src') || '')
  smoke.check('输入框里没有出现文件路径', ((smoke.q('.composer-input') || {}).value || '').indexOf('/pasted/') < 0,
    JSON.stringify((smoke.q('.composer-input') || {}).value || ''))
  smoke.check('粘贴成功时没有失败提示', !smoke.q('.paste-notice'), smoke.text('.paste-notice'))

  // Send it: a message of one picture plus a sentence, and the reply proves the turn ran.
  smoke.type('.composer-input', '这张图是什么')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.chat', '图看到了', 30000))) return smoke.finish()

  // The bubble shows what the reader attached — the picture, not the reference that carried it.
  const inBubble = await smoke.waitFor('.msg-user .user-images img', '发出的气泡里画出那张图', 8000)
  if (!inBubble) return smoke.finish()
  smoke.check('气泡里没有退回成 @ 路径', smoke.text('.msg-user').indexOf('/pasted/') < 0,
    smoke.text('.msg-user').slice(0, 160))
  smoke.check('气泡里留着用户自己写的字', smoke.text('.msg-user').indexOf('这张图是什么') >= 0,
    smoke.text('.msg-user').slice(0, 160))
  smoke.finish()
})()
