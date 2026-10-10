// 子系统拥有的会话（今天是接收侧节点的 A2A 执行，session.KindA2AExecution）。
//
// 为什么列表和详情都要标出来：这类会话的标题是从远端 prompt 生成的，行上不标就等于把
// 别人的执行记录当成读者自己的对话（设计 §2.1）。
//
// 这里只负责说清它是什么。真正的边界不在前端——控制台本来就只读会话文件，任何入口想
// 驱动这类会话都会被 agent 入口拒掉（agent/managed.go），前端不做判定也是对的。
//
// 措辞与 TUI 的 managedRowSuffix 一致：同一个事实，两个前端用同一句话。

export function isManaged(kind?: string): boolean {
  return !!kind
}

/** 标签文本；普通会话返回空串，调用方据此决定要不要渲染。 */
export function managedLabel(kind?: string): string {
  return kind ? `子系统执行记录 ${kind} · 只读` : ''
}
