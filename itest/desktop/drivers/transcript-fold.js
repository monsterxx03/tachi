// transcript-fold — 一轮的过程被折成一行；失败不藏；点开才铺开完整时序。
//
// 这一轮有两步（一条成功命令 + 一次读不到文件的失败调用）和一条结论。折叠态应当只看到
// **失败那张卡**（失败永不折叠），成功那步收在过程条里；点开后两张卡都在。过程条的文案由
// parts 推导，所以这里顺带钉住"2 步 + ⚠ 1 步失败"这两个数。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  smoke.type('.composer-input', '跑两步')
  smoke.click('.send-btn')

  if (!(await smoke.waitText('.msg-assistant .msg-content', '两步都处理完了。', 30000))) return smoke.finish()
  // 回合真的结束了再断言（停止按钮消失是"这一轮跑完"的信号），否则可能读到中间态。
  if (!(await smoke.waitFor(() => !smoke.q('.stop-btn'), '回合结束（停止按钮消失）'))) return smoke.finish()

  const strip = smoke.q('.process-head')
  if (!strip) return smoke.fail('过程条存在', '找不到 .process-head')
  const label = strip.textContent.trim()
  smoke.check('过程条给出步数与失败数', label.indexOf('2 步') >= 0 && label.indexOf('1 步失败') >= 0, label)
  smoke.check('失败时过程条染红', strip.className.indexOf('failed') >= 0, strip.className)
  smoke.check('结论正文恒显（未展开也在）',
    smoke.text('.msg-assistant').indexOf('两步都处理完了。') >= 0, smoke.text('.msg-assistant').slice(-60))

  // 折叠态：只有失败那张卡在屏幕上，成功那步收起。
  const folded = smoke.qa('.tool-status')
  smoke.check('折叠态只留失败那张卡', folded.length === 1, folded.length + ' 张')
  smoke.check('留下的那张是失败卡', folded.length === 1 && folded[0].className.indexOf('err') >= 0,
    folded.map((s) => s.className).join(','))

  // 展开：完整时序回来。
  smoke.click(strip)
  if (!(await smoke.waitFor(() => smoke.qa('.tool-status').length === 2, '展开后两张卡都在'))) return smoke.finish()
  const open = smoke.q('.process-head')
  smoke.check('展开后提示变成收起', open.textContent.indexOf('收起') >= 0, open.textContent.trim())
  smoke.check('展开态两张卡（成功 + 失败）',
    smoke.qa('.tool-status').length === 2 && smoke.qa('.tool-status.ok').length === 1,
    smoke.qa('.tool-status').map((s) => s.className).join(','))
  smoke.finish()
})()
