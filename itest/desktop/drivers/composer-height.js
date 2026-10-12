// composer-height — the input box grows when the reader drags its top edge, and only that far.
//
// The fact under test is a layout one, so it is measured (bounding boxes), not inferred: the box
// gets taller, the conversation gives up the room, the growth stops at the ceiling, and dragging
// back to the floor leaves no height at all — which is what "nothing is persisted" has to mean.
//
// The drag listens on the WINDOW (a pointer that leaves the 8px strip mid-drag must keep
// resizing), so the moves are dispatched there; setPointerCapture would refuse a synthetic
// pointer it never saw go down.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  const box = () => Math.round(smoke.q('.composer-input').getBoundingClientRect().height)
  const chatH = () => Math.round(smoke.q('.chat').getBoundingClientRect().height)
  if (!smoke.q('.composer-resizer')) return smoke.fail('输入框有高度手柄', '找不到 .composer-resizer')

  const initial = box()
  const chat0 = chatH()
  smoke.check('输入框默认就是一行高', initial >= 20 && initial <= 30, `${initial}px`)

  const x = () => {
    const r = smoke.q('.composer-resizer').getBoundingClientRect()
    return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + 4) }
  }
  // The handle is re-queried for EVERY gesture: a drag re-renders the box, and an element captured
  // before that is not guaranteed to be the one on screen (measured: a stale reference swallowed
  // the second drag, which is how a "ceiling" assertion ended up passing without dragging at all).
  const drag = async (dy) => {
    const el = smoke.q('.composer-resizer')
    const at = x()
    const id = Date.now() // a fresh pointer id per gesture, like a real one
    el.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, pointerId: id, clientX: at.x, clientY: at.y }))
    // Snapshot the drag state HERE: the class is gone by the time the assertion's detail string is
    // built, so reading it later would report the end of the gesture, not the press.
    const dragging = { on: document.body.classList.contains('is-resizing-y'), body: document.body.className }
    window.dispatchEvent(new PointerEvent('pointermove', { bubbles: true, pointerId: id, clientX: at.x, clientY: at.y + dy }))
    window.dispatchEvent(new PointerEvent('pointerup', { bubbles: true, pointerId: id, clientX: at.x, clientY: at.y + dy }))
    return dragging
  }

  // Up is taller: the handle sits on the TOP edge, so the box grows away from the pointer.
  const pressed = await drag(-200)
  smoke.check('按下手柄即进入拖拽态（指针事件到了手柄）', pressed.on, `body="${pressed.body}"`)
  const grew = await smoke.waitFor(() => box() > initial + 100, '向上拖后输入框变高', 3000)
  smoke.check('向上拖动加高输入框', !!grew, `${initial} → ${box()}px`)
  // The room comes from the conversation: that is the trade the height is making, and if the
  // transcript kept its height instead, the box would be covering it rather than taking room.
  const chatShrank = await smoke.waitFor(() => chatH() < chat0, '对话区让出高度', 3000)
  smoke.check('加高输入框时对话区变矮', !!chatShrank, `${chat0} → ${chatH()}px`)

  // The ceiling: a drag far past it stops at the window's share, not at the pointer.
  const before = box()
  await drag(-5000)
  const max = Math.round(window.innerHeight * 0.6) // COMPOSER_INPUT_MAX_FRACTION in composer.tsx
  const capped = await smoke.waitFor(() => box() > before, '拖到顶后高度再长一截', 3000)
  smoke.check('加高有上限（停在窗口的 60%）', !!capped && Math.abs(box() - max) <= 2,
    `${before} → ${box()}px，上限 ${max}px`)

  // Back to the floor = "not resized": the stylesheet's height again, with no inline height left.
  await drag(5000)
  const reset = await smoke.waitFor(() => Math.abs(box() - initial) <= 1, '拖回底部回到默认高度', 3000)
  smoke.check('拖回底部即回到默认高度', !!reset, `${box()} vs ${initial}px`)
  smoke.check('回到默认后不再留内联高度', !smoke.q('.composer-input').style.height,
    `inline="${smoke.q('.composer-input').style.height}"`)

  // The keyboard path: the handle is a real focusable separator, not a decoration.
  const handle = smoke.q('.composer-resizer')
  handle.focus()
  handle.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true, cancelable: true }))
  const keyed = await smoke.waitFor(() => box() > initial, '↑ 也能加高', 3000)
  smoke.check('焦点在手柄上时 ↑ 也能加高', !!keyed, `${initial} → ${box()}px`)

  // …and the same ±, without leaving the field: ⌘⇧↑/↓ is bound in the textarea, so the hand that is
  // typing can resize the box it types into. The pair has to come back down to the floor too — that is
  // the same "not resized" (no inline height) the drag proves, reached from the other end.
  const keyIn = (key, mods) => {
    smoke.q('.composer-input').dispatchEvent(smoke.keyEvent(key, mods))
  }
  smoke.q('.composer-input').focus()
  const midHot = box()
  keyIn('ArrowUp', { metaKey: true, shiftKey: true })
  const hotGrew = await smoke.waitFor(() => box() > midHot, '⌘⇧↑ 加高输入框', 3000)
  smoke.check('焦点在输入框里时 ⌘⇧↑ 加高', !!hotGrew, `${midHot} → ${box()}px`)
  const tall = box()
  keyIn('ArrowDown', { metaKey: true, shiftKey: true })
  const hotShrank = await smoke.waitFor(() => box() < tall, '⌘⇧↓ 变矮输入框', 3000)
  smoke.check('⌘⇧↓ 变矮', !!hotShrank, `${tall} → ${box()}px`)
  keyIn('ArrowDown', { metaKey: true, shiftKey: true })
  const hotFloor = await smoke.waitFor(() => Math.abs(box() - initial) <= 1, '⌘⇧↓ 一路回到默认高度', 3000)
  smoke.check('⌘⇧↓ 到底即回到默认高度', !!hotFloor, `${box()} vs ${initial}px`)
  // The boundary: ⌘↓ (no ⇧) is the text system's own "caret to the end of the box" and must stay that
  // way — a handler that widened to bare ⌘arrows would take a caret gesture away silently.
  const beforePlain = box()
  keyIn('ArrowDown', { metaKey: true })
  smoke.check('单独的 ⌘↓ 不动高度（光标手势留给文本框）', box() === beforePlain, `${beforePlain} → ${box()}px`)

  // The box hugs its text: a couple more lines make it taller with nobody dragging, and emptying
  // the draft puts it back on the floor. That self-sizing is what lets the resting box be ONE line
  // tall — without it a one-line box would either scroll a long draft away or reserve a second
  // line it is not using, which is the blank under the text this replaced.
  smoke.type('.composer-input', '一行\n两行\n三行')
  const grewWithText = await smoke.waitFor(() => box() > initial + 20, '多行草稿让输入框自己长高', 3000)
  smoke.check('草稿变多时输入框自己长高（不用拖）', !!grewWithText, `${initial} → ${box()}px`)
  smoke.type('.composer-input', '')
  const backToFloor = await smoke.waitFor(() => Math.abs(box() - initial) <= 1, '清空草稿后回到一行高', 3000)
  smoke.check('清空草稿后回到一行高（默认高度只有一个）', !!backToFloor, `${box()} vs ${initial}px`)
  smoke.check('回到一行高后不再留内联高度', !smoke.q('.composer-input').style.height,
    `inline="${smoke.q('.composer-input').style.height}"`)

  smoke.finish()
})()
