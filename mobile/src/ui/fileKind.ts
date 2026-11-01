import type { AttachmentRef } from '../api/types';

/** 附件那一屏按哪一种看 —— 图片、markdown、纯文本，还是「手机打不开」。 */
export type FileKind = 'image' | 'markdown' | 'text' | 'other';

/** 没有 mime 可用时按名字兜底；大小写不敏感，`.MD` 也是 markdown。 */
const MARKDOWN_EXT = /\.(md|markdown|mdx)$/i;

/**
 * 一个附件该怎么看。
 *
 * mime 是权威的（桌面那边按扩展名给：`.md` → `text/markdown`），但它不是唯一可信的来源 ——
 * 旧记录里可能是 `text/plain`，而文件本身明明是 markdown，所以扩展名再兜一次底。
 *
 * **markdown 必须在 text 之前判**：`text/markdown` 也以 `text/` 开头，先判 text 就等于
 * 永远走不到渲染那一步 —— 附件阅读器当初就是这么把一份 md 当源码摊在屏幕上的。
 */
export function fileKind(file: Pick<AttachmentRef, 'mimeType' | 'name'>): FileKind {
  const mime = file.mimeType.toLowerCase();
  if (mime.startsWith('image/')) return 'image';
  if (mime === 'text/markdown' || mime === 'text/x-markdown' || MARKDOWN_EXT.test(file.name)) {
    return 'markdown';
  }
  if (mime.startsWith('text/') || mime === 'application/json') return 'text';
  return 'other';
}
