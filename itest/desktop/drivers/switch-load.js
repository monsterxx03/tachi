// switch-load — the FIRST visit to a session must land on the newest message, and following must
// survive content that grows without a message update.
//
// switch-scroll samples the transient drift of a switch back to a session the process never
// unloaded. This one covers the other side: 「另一个会话」 exists on disk with a long transcript
// ending in a long mermaid diagram, and the app has never shown it — so clicking its row is the
// load-on-first-visit path, where the 加载会话… placeholder is on screen when the switch's own pin
// runs and the real transcript arrives afterwards. The last probe grows the content box from
// outside React (no message update, no state change — exactly what an asynchronous diagram does)
// and requires following to re-pin, which is the mechanism the whole thing rests on.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  const gap = () => {
    const el = smoke.q('.chat')
    return el ? Math.round(el.scrollHeight - el.scrollTop - el.clientHeight) : -1
  }
  const clickRow = (title) => {
    const row = smoke.qa('.session').find((s) => smoke.text(s.querySelector('.session-title')) === title)
    return row ? smoke.click(row) : false
  }
  const sample = async (label) => {
    const at = [150, 700, 1600, 3000]
    const seen = []
    let prev = 0
    for (const ms of at) {
      await smoke.sleep(ms - prev)
      prev = ms
      seen.push(`${ms}ms=${gap()}px`)
    }
    const rest = gap()
    smoke.log(label, `${seen.join(' ')}，回到最新消息按钮=${!!smoke.q('.jump-latest')}，` +
      `转写高 ${smoke.q('.chat')?.scrollHeight}px，图已渲染=${!!smoke.q('.chat .mermaid svg')}`)
    return rest
  }

  const target = '另一个会话'
  const rows = smoke.allText('.session-title')
  smoke.log('侧栏里的会话', rows.join(' | '))

  // First visit: the transcript comes off disk, the diagram renders on top of it.
  if (!clickRow(target)) return smoke.fail('点未加载的会话', '行找不到：' + rows.join(' | '))
  if (!(await smoke.waitFor(() => smoke.text('.session.active .session-title') === target, '切到未加载的会话', 10000))) return smoke.finish()
  const first = await sample('首访「' + target + '」')
  smoke.check('首访未加载的会话停在最新消息', first >= 0 && first <= 4, `距底 ${first}px`)

  // Second visit: same session, now served from the in-memory copy.
  if (!clickRow('冒烟会话')) return smoke.fail('切回空会话', '行找不到')
  if (!(await smoke.waitFor(() => smoke.text('.session.active .session-title') === '冒烟会话', '切回空会话', 10000))) return smoke.finish()
  await smoke.sleep(300)
  if (!clickRow(target)) return smoke.fail('再点未加载的会话', '行找不到')
  if (!(await smoke.waitFor(() => smoke.text('.session.active .session-title') === target, '第二次切到该会话', 10000))) return smoke.finish()
  const again = await sample('再访「' + target + '」（已缓存）')
  smoke.check('再访（缓存）也停在最新消息', again >= 0 && again <= 4, `距底 ${again}px`)

  // The mechanism, tested directly rather than inferred: while the app is following, the content
  // grows from OUTSIDE React (no message update, no state change — exactly what an asynchronous
  // diagram does) and the scroll event the app's own pin had queued is delivered BEFORE the
  // observer's re-pin, which is what happens when the growth lands in the same rendering pass.
  // Dispatching it by hand puts the handler in exactly that state, deterministically.
  //
  // Following must survive it: the reader did not move, the bottom did. Answering "has the reader
  // scrolled away?" from the distance to the bottom read this as a departure — following switched
  // off, the 回到最新消息 button appeared, and the view rested short of the newest message.
  const content = smoke.q('.chat-content')
  const chat = smoke.q('.chat')
  if (!content || !chat) return smoke.fail('取 .chat-content', '找不到')
  const filler = document.createElement('div')
  filler.style.height = '500px'
  filler.textContent = 'probe filler'
  content.appendChild(filler)
  chat.dispatchEvent(new Event('scroll'))
  await smoke.sleep(400)
  const afterGrow = gap()
  smoke.log('外部撑高 500px 之后（未移动视口）', `距底 ${afterGrow}px，回到最新消息按钮=${!!smoke.q('.jump-latest')}`)
  smoke.check('内容长高不会关掉跟随（视口没动）', afterGrow >= 0 && afterGrow <= 4 && !smoke.q('.jump-latest'),
    `距底 ${afterGrow}px，按钮=${!!smoke.q('.jump-latest')}`)

  // …and the reader's own scroll must still turn it off, or the fix would have disabled the
  // feature instead of the bug: move the viewport up by hand and following must end.
  chat.scrollTop = Math.max(0, chat.scrollHeight - chat.clientHeight - 400)
  chat.dispatchEvent(new Event('scroll'))
  await smoke.sleep(250)
  smoke.check('读者自己往上滚仍然会关掉跟随', !!smoke.q('.jump-latest'), `按钮=${!!smoke.q('.jump-latest')}`)

  smoke.finish()
})()
