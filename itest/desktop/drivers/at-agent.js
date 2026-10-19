// at-agent — `#a:` 选一个节点，以及哪些 '#' 根本不该开弹窗。
//
// 沙箱没有配任何 peer（隔离 HOME 下没有 peers 段），所以这里钉的是**路由与措辞**：'#' 先给出两个
// 命名空间、Tab 把它展开成 '#a:' 并把弹窗留在节点命名空间、那个命名空间空的时候说的是人话
// （「还没连上过任何节点」，而不是把读者扔进文件搜索），以及 '#include' 这类普通文本压根不弹窗。
//
// 「行里显示什么」由 Go 侧断（tui/at_file_test.go 的 TestAtAgentCompletion、以及 desktop 的
// SearchAgents 测试）：本机怎么称呼它、pin 住的身份、上次探测的结果 —— 三个前端同一句话。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()
  if (!(await smoke.waitForSession())) return smoke.finish()

  // ── 1. 一个 '#' 给出两个命名空间 ────────────────────────────────────────────
  smoke.type('.composer-input', '#')
  if (!(await smoke.waitFor('.at-picker', '# 打开补全弹窗'))) return smoke.finish()
  smoke.check('标题说明这是引用命名空间', smoke.text('.at-picker-title').indexOf('# 引用') >= 0,
    smoke.text('.at-picker-head'))
  if (!(await smoke.waitText('.at-picker-list', '#a:', 10000))) return smoke.finish()
  const candidates = smoke.text('.at-picker-list')
  smoke.check('#a: 与 #s: 两个候选都在',
    candidates.indexOf('#a:') >= 0 && candidates.indexOf('#s:') >= 0, candidates)
  smoke.check('候选行说得出各自是干什么的',
    candidates.indexOf('节点') >= 0 && candidates.indexOf('对话') >= 0, candidates)

  // ── 2. Tab 打开 #a:，弹窗留在新打开的命名空间里 ─────────────────────────────
  // 这一条是那个手势的全部：候选行**打开**一个命名空间，而不是结束一个引用。若它像叶子行那样
  // 插一个尾随空格，引用当场就结束了，读者会停在 "#a: " 上而看不到任何节点。
  smoke.key('.composer-input', 'Tab')
  const opened = await smoke.waitFor(() => {
    const v = smoke.q('.composer-input').value
    return v === '#a:' ? v : null
  }, 'Tab 把候选展开成 #a:（不带尾随空格）')
  if (!opened) return smoke.finish()
  smoke.check('展开成 #a: 且引用没有被空格结束', true, opened)
  if (!(await smoke.waitFor('.at-picker', '弹窗留在节点命名空间'))) return smoke.finish()
  smoke.check('标题跟着换成节点命名空间', smoke.text('.at-picker-title').indexOf('# 节点') >= 0,
    smoke.text('.at-picker-head'))

  // 沙箱没配 peer：说的是「还没连上过任何节点」，而不是退回去搜文件，也不是一句空白。措辞覆盖两种
  // 空：一台都没配，与配了但还没跟任何一台握过手（没有身份就没有可插入的引用，agentref.Search）。
  if (!(await smoke.waitText('.at-picker-empty', '还没连上过任何节点', 10000))) return smoke.finish()
  smoke.check('还没连上过任何节点时说的是人话', true, smoke.text('.at-picker-empty'))

  // ── 3. 不是引用的 '#' 是普通文本 ────────────────────────────────────────────
  // '#' 只认两个前缀，所以 C 指令、issue 号、标题都不会把弹窗叫出来 —— 这是那个规则在界面上的
  // 那一半（另一半在 agent/atfile 的扫描器里）。
  for (const plain of ['#include <stdio.h>', '#123', 'C#a:nas']) {
    smoke.type('.composer-input', '')
    await smoke.sleep(60)
    smoke.type('.composer-input', plain)
    await smoke.sleep(300) // 弹窗（defer 一下）若会出现，就在这段时间里出现
    smoke.check('普通文本不弹窗：' + plain, !smoke.q('.at-picker'), smoke.text('.at-picker-head'))
  }
  smoke.finish()
})()
