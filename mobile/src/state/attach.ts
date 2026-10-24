/**
 * 带文件的消息在气泡里长什么样：用户自己的话，加上每个文件一行名字。
 *
 * 服务端写记录时用的是同一条规则（`desktop/mobile_upload.go` 的 `mobileMessageWithFiles`），
 * 而记录是两边重读的那一份 —— 这里镜像它，只是为了让这条消息在发出去的那一瞬间就长得一样，
 * 而不是「先显示一句纯文本，过一会儿重读快照后多出一行」。
 *
 * 名字而不是路径：手机屏幕窄，一长串 `/Users/…/uploads/IMG_2043.jpg` 会把话挤没。
 */
export function displayOf(text: string, names: string[]): string {
  const said = text.trim();
  if (names.length === 0) return said;
  const lines = names.map((n) => `📎 ${n}`).join('\n');
  return said ? `${said}\n\n${lines}` : lines;
}
