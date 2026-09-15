// form-text — the 新建项目 / 编辑项目 dialog's type scale and width.
//
// The form renders the roots panel's rows, so it inherited that compact popover's sizes: labels at
// 10.5px and paths at 11px, three steps below the 14px title and 13.5px field they sit between —
// the dialog read as a shrunken one. Its width was clamped the same quiet way: `.confirm-box` caps
// at 380px, which beat the form's own `width: 520px`, so the fields ellipsised paths they had room
// for. Both are pinned as FLOORS/intent rather than exact sizes, so retuning the design later is
// not a test failure.
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  if (!smoke.click('.new-project')) return smoke.fail('点新建项目', '按钮找不到')
  if (!(await smoke.waitFor('.confirm-box.project-form', '弹窗出现', 5000))) return smoke.finish()
  await smoke.sleep(300)

  const box = smoke.q('.confirm-box.project-form')
  const cs = getComputedStyle(box)
  // The border box, not clientWidth: 1px of border on each side is the difference between "the
  // dialog is 520" and "the dialog is 518", and the width rule is what this pins.
  const boxW = Math.round(box.getBoundingClientRect().width)
  smoke.log('弹窗盒子', `实际宽 ${boxW}px（border-box），width=${cs.width}，max-width=${cs.maxWidth}`)
  smoke.check('弹窗是自己的宽度（520px，不被通用 max-width 压回 380）',
    Math.abs(boxW - 520) <= 1, `${boxW}px（max-width=${cs.maxWidth}）`)

  const parts = {
    '标题 .confirm-msg': '.project-form .confirm-msg',
    '说明 .confirm-sub': '.project-form .confirm-sub',
    '字段名 .form-label': '.project-form .form-label',
    '输入框 .form-input': '.project-form .form-input',
    '路径 .form-path-value': '.project-form .form-path-value',
    '空列表 .roots-empty': '.project-form .roots-empty',
    '加目录 .roots-add': '.project-form .roots-add',
    '按钮 .btn': '.project-form .btn',
  }
  const sizes = {}
  for (const [k, sel] of Object.entries(parts)) {
    const el = smoke.q(sel)
    sizes[k] = el ? parseFloat(getComputedStyle(el).fontSize) : 0
  }
  smoke.log('字号', JSON.stringify(sizes))
  const missing = Object.entries(sizes).filter(([, v]) => v === 0).map(([k]) => k)
  const tooSmall = Object.entries(sizes).filter(([, v]) => v > 0 && v < 12)
  if (!smoke.check('每一块文字都在（选择器没写错）', missing.length === 0, missing.join(' / '))) return smoke.finish()
  smoke.check('表单里没有小于 12px 的文字', tooSmall.length === 0,
    tooSmall.length ? JSON.stringify(tooSmall) : `最小 ${Math.min(...Object.values(sizes))}px`)
  smoke.finish()
})()
