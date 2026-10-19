// mobile-prefs — the window's half of the two-switch sync.
//
// The phone's half runs in the runner's `live` hook, concurrently with this driver: it changes
// the model and the approval mode over HTTP. All this driver does is wait for the window to show
// the SAME pair — which it can only do if the backend told it (agent:session_prefs), because
// nothing in this window was clicked.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const modelBtn = () => smoke.q('button.provider-select')
  const approval = () => smoke.q('select.approval-select')

  const modelNow = () => (modelBtn() ? smoke.text(modelBtn()) : '')
  const approvalNow = () => (approval() ? approval().value : '')

  // 起始那一对（smoke / 需要审批）由 Go 那一半在 WS 快照上断言。这里**不能**断言它：live 与
  // 这个驱动并发跑，接口往往比窗口先就绪，等驱动读到控件时改动已经落地了——那样这条检查会
  // 红在一个完全正确的实现上（它第一次就是红的）。
  //
  // live 会依次切到 smoke-alt、打开跳过审批。给它足够的时间：它要等接口起来、开一条 WS。
  const changed = await smoke.waitFor(
    () => (modelNow() === 'smoke-alt' && approvalNow() === 'skip' ? true : null),
    '手机改的两个开关传到窗口',
    30000,
  )
  smoke.check('另一块屏改了模型与审批，窗口立刻跟上', !!changed, modelNow() + ' / ' + approvalNow())

  // A turn cannot be started by this scenario, so nothing else can move these controls: reading
  // them once more after the wait pins the claim that the change STUCK rather than flickered.
  await smoke.sleep(400)
  smoke.check('改动留在窗口上（不是一帧的动画）',
    modelNow() === 'smoke-alt' && approvalNow() === 'skip', modelNow() + ' / ' + approvalNow())

  // 反向：这一半（窗口）自己把审批改回去。sync 的另一半是「桌上改的，手机也要知道」——live
  // 那一半正等着这个事件。用 pick（React 的 select 只认原型上的 setter + change）。
  if (!smoke.pick('select.approval-select', 'ask')) return smoke.finish()
  const back = await smoke.waitFor(() => (approvalNow() === 'ask' ? true : null),
    '窗口自己把审批改回「需要审批」', 5000)
  smoke.check('窗口改回审批（另一块屏由 Go 那一半断言收到了）', !!back, approvalNow())
  smoke.finish()
})()
