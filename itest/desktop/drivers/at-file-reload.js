// at-file-reload — 重启后加载会话：带 @-file 的用户消息，气泡里必须是**用户打的那句**。
//
// 现场：会话记录里 `content` 是发给模型的那份（文件被内联在 UNTRUSTED FILE CONTENT 标记之间），
// `display_content` 是用户打的 `@path`。展开发生在**前端调 agent 之前**，所以记录里曾经只有展开
// 文本 —— 重启/切回会话，气泡里就是整份文件正文，用户打的 `@README.md` 不见了。
//
// 这个断言只能在**从磁盘渲染**的那一次上成立：同一个进程里切会话用的是内存副本（气泡是前端自己
// 追加的原文，永远不会错），所以转写由 fixture 预置在 messages.jsonl 里 —— 那正是重启会读到的东西。
// 会话里没有要发出去的消息，因此这一轮不该产生任何模型请求。
;(async () => {
  if (!(await smoke.waitFor('.composer-input', 'app 挂载（编辑器出现）'))) return smoke.finish()

  if (!(await smoke.waitFor('.msg-user', '预置的会话渲染出用户气泡', 15000))) return smoke.finish()
  const bubble = smoke.text('.msg-user')
  smoke.check('气泡显示用户打的 @ 引用', bubble.indexOf('@README.md') >= 0, bubble.slice(0, 120))
  // 两条都要：正文本身，以及那个标记 —— 只查标记会漏掉「内联了但标记被剥掉」的情况。
  smoke.check('气泡不含内联的文件正文', bubble.indexOf('working directory') < 0, bubble.slice(0, 200))
  smoke.check('气泡不含内联标记', bubble.indexOf('UNTRUSTED') < 0, bubble.slice(0, 200))
  smoke.finish()
})()
