// transcript-fold — 一轮的过程被折成一行；失败不藏；点开才铺开完整时序。
//
// 这一轮有两步（一条成功命令 + 一次读不到文件的失败调用）和一段收尾结论，**每轮还各带一段正文**。
// 折叠态应当只看到：**失败那张卡**（失败永不折叠）+ 收尾结论；点开后完整时序（含那段中间正文）
// 都在。过程条的文案由 parts 推导，所以这里顺带钉住"2 步 + ⚠ 1 步失败 + 含 1 段过程说明"。
//
// 中间那段正文是这条 driver 的另一半，也是被问过的问题："失败了，失败卡外露，可上一轮的正文
// 也被折进这轮的时间线了 —— 是不是折叠出了问题？" 答案：不是。折叠规则只有一条 —— 该轮**最后
// 一段** text 恒显，其余 text 全折进过程条 —— 它跟有没有失败无关（`turnView` 的判定里没有失败
// 条件），召回靠过程条上的「含 N 段过程说明」。所以这里断了两个方向：带失败那一轮、以及
// **整轮没有失败**的对照轮，中间正文一样是折起来的；而每轮的正文都留在自己的气泡里（没有被并到
// 下一轮的时间线里 —— 气泡数量就是这件事的判据）。
;(async () => {
  const midMark = '第二步：读一个不存在的文件。'
  const midFolded = (where) => smoke.text(where).indexOf(midMark) < 0

  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  smoke.type('.composer-input', '跑两步')
  smoke.click('.send-btn')

  if (!(await smoke.waitText('.msg-assistant .msg-content', '两步都处理完了。', 30000))) return smoke.finish()
  // 回合真的结束了再断言（停止按钮消失是"这一轮跑完"的信号），否则可能读到中间态。
  if (!(await smoke.waitFor(() => !smoke.q('.stop-btn'), '回合结束（停止按钮消失）'))) return smoke.finish()

  const strip = smoke.q('.process-head')
  if (!strip) return smoke.fail('过程条存在', '找不到 .process-head')
  const label = strip.textContent.trim()
  smoke.check('过程条给出步数与失败数', label.indexOf('2 步') >= 0 && label.indexOf('1 步失败') >= 0, label)
  // 失败由**计数**说出来，不靠整条染色：这一行仍然带 `failed` 标记（DOM 层的语义锚点，也是计数
  // span 的依据），但底色必须和没有失败的那一轮一样——两天前它还是整条红底，把"有一小步失败"
  // 变成了整轮的警报。两轮底色的比较在对照轮那段（那时才有第二个过程条可对照）。
  // 底色必须在过程条**离开 live 之后**才读：停止按钮消失与那一行改类名不是同一帧，先读到的
  // 是 `live` 的 --accent-soft（蓝调），而对照轮读的是收尾后的 wash —— 两轮"底色相同"会红在
  // 一个只差一帧的中间态上（实测差的就是这半帧）。
  if (!(await smoke.waitFor(() => !smoke.q('.process-head.live'), '过程条不再是 live', 5000))) return smoke.finish()
  // 底色不是「类名一换就定了」：换类名之后 CSS 还在做一次过渡，`getComputedStyle` 在这一段里
  // 返回的是**插值中的中间色**（实测同一处读到过 rgba(69,87,168,.082) 这种 live 的蓝调，也读到过
  // 落在 wash 上的灰）。所以要等颜色自己不动了再取值 —— 两轮用同一把尺子量。
  const settledBg = async (el) => {
    let prev = getComputedStyle(el).backgroundColor
    for (let i = 0; i < 50; i++) {
      await smoke.sleep(40)
      const now = getComputedStyle(el).backgroundColor
      if (now === prev) return now
      prev = now
    }
    return prev
  }
  const failedBg = await settledBg(strip)
  smoke.check('失败的行带标记，但不整条染色', strip.className.indexOf('failed') >= 0, strip.className)
  smoke.check('过程条提示中间还有正文（召回的口子）', label.indexOf('含 2 段过程说明') >= 0, label)
  smoke.check('结论正文恒显（未展开也在）',
    smoke.text('.msg-assistant').indexOf('两步都处理完了。') >= 0, smoke.text('.msg-assistant').slice(-60))

  // 折叠态：只有失败那张卡在屏幕上，成功那步收起。
  const folded = smoke.qa('.tool-status')
  smoke.check('折叠态只留失败那张卡', folded.length === 1, folded.length + ' 张')
  smoke.check('留下的那张是失败卡', folded.length === 1 && folded[0].className.indexOf('err') >= 0,
    folded.map((s) => s.className).join(','))

  // 中间那段正文（失败自己那一轮的输出）在折叠态下不可见——这是规则，不是丢内容；展开后必须回来。
  smoke.check('折叠态下中间正文不可见（规则本身，与失败无关）', midFolded('.msg-assistant'),
    smoke.text('.msg-assistant').slice(0, 120))
  smoke.check('失败这一轮的正文没有并到别的气泡里（一轮一个气泡）',
    smoke.qa('.msg-assistant').length === 1, smoke.qa('.msg-assistant').length + ' 个助手气泡')

  // 展开：完整时序回来。
  smoke.click(strip)
  if (!(await smoke.waitFor(() => smoke.qa('.tool-status').length === 2, '展开后两张卡都在'))) return smoke.finish()
  const open = smoke.q('.process-head')
  smoke.check('展开后提示变成收起', open.textContent.indexOf('收起') >= 0, open.textContent.trim())
  smoke.check('展开态两张卡（成功 + 失败）',
    smoke.qa('.tool-status').length === 2 && smoke.qa('.tool-status.ok').length === 1,
    smoke.qa('.tool-status').map((s) => s.className).join(','))
  smoke.check('展开后中间正文回来了（是折起来，不是丢掉）',
    !midFolded('.msg-assistant'), smoke.text('.msg-assistant').slice(0, 120))

  // 对照轮：整轮没有失败，中间正文一样折起来——"被折"和"失败"没有因果关系。
  smoke.type('.composer-input', '再跑一轮对照')
  smoke.click('.send-btn')
  if (!(await smoke.waitText('.msg-assistant .msg-content', '对照轮的收尾结论。', 30000))) return smoke.finish()
  if (!(await smoke.waitFor(() => !smoke.q('.stop-btn'), '对照轮结束'))) return smoke.finish()
  const controlStrip = smoke.qa('.process-head')[smoke.qa('.process-head').length - 1]
  smoke.check('对照轮没有失败（过程条不带失败标记）',
    !!controlStrip && controlStrip.className.indexOf('failed') < 0,
    controlStrip ? controlStrip.className : '(没有过程条)')
  // 两级比较：有失败的那一轮与这一轮底色完全相同 —— 失败不是用颜色说的，是用那一行文字说的。
  const controlBg = controlStrip ? await settledBg(controlStrip) : ''
  smoke.check('两轮底色相同（失败不染色）',
    !!controlStrip && controlBg === failedBg,
    controlStrip ? controlBg + ' vs ' + failedBg : '(没有过程条)')
  const bubbles = smoke.qa('.msg-assistant')
  const lastText = bubbles.length ? bubbles[bubbles.length - 1].textContent.replace(/\s+/g, ' ').trim() : ''
  smoke.check('对照轮：中间正文同样折起来（与失败无关）',
    lastText.indexOf('对照轮第一步：') < 0, lastText.slice(0, 120))
  smoke.check('对照轮的收尾结论恒显', lastText.indexOf('对照轮的收尾结论。') >= 0, lastText.slice(-60))
  smoke.check('两轮各自一个气泡（正文没有跨轮并进来）',
    bubbles.length === 2, bubbles.length + ' 个助手气泡')
  smoke.finish()
})()
