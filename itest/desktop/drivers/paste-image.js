// paste-image — a screenshot pasted into the box must reach the model as an image.
//
// The clipboard is simulated IN THE PAGE: a real `paste` event carrying PNG bytes, which is exactly
// what the clipboard holds after a screenshot (bytes with no path — the reason the composer stores
// it through a binding instead of splicing a path in). The Go side then asserts those bytes arrived
// at the LLM boundary; this half proves the gesture works and that what lands in the box is a
// reference, not the image's textual form or nothing at all.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

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

  const got = await smoke.waitFor(
    () => ((smoke.q('.composer-input') || {}).value || '').indexOf('/pasted/') >= 0 ? smoke.q('.composer-input').value : null,
    '输入框里出现粘贴得到的引用', 8000)
  if (!got) {
    smoke.log('输入框此刻的值', JSON.stringify((smoke.q('.composer-input') || {}).value || ''))
    smoke.log('是否出现了失败提示', smoke.text('.paste-notice') || '(没有)')
    return smoke.finish()
  }
  smoke.check('粘贴得到的是 @ 引用（不是空白、也不是图片的文字形式）',
    got.trim().startsWith('@') && got.trim().endsWith('.png'), got.trim())
  smoke.check('粘贴成功时没有失败提示', !smoke.q('.paste-notice'), smoke.text('.paste-notice'))

  // Send it: the reference alone is a message, and the reply proves the turn ran.
  smoke.type('.composer-input', got.trim() + ' 这张图是什么')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.chat', '图看到了', 30000))) return smoke.finish()
  smoke.finish()
})()
