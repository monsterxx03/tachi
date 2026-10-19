// mobile-api — the phone interface scenario's driver.
//
// It drives NOTHING. The subject of this scenario is the HTTP/WebSocket server the desktop
// process runs for a phone, and there is no button in the window that calls it. The driver's
// whole job is to prove the app mounted and the fixture session is on screen (the two gates
// every driver here has to pass), and then get out of the way: the runner's `live` half does
// the exchange over the wire while the process is still alive.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()
  smoke.check('窗口开在 fixture 会话上（接口那一半在 Go 里打）', true, '')
  smoke.finish()
})()
