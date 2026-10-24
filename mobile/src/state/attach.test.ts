import { displayOf } from './attach';

// 气泡里的文件名那一行：服务端写记录时用同一个规则（desktop/mobile_upload.go），
// 这里钉的是两边必须一致的那两句话。
describe('displayOf', () => {
  it('without files it is just what the reader typed', () => {
    expect(displayOf('  看看这个  ', [])).toBe('看看这个');
  });

  it('puts one line per file under the words', () => {
    expect(displayOf('看看这个', ['报告.pdf'])).toBe('看看这个\n\n📎 报告.pdf');
    expect(displayOf('这两个', ['a.jpg', 'b.pdf'])).toBe('这两个\n\n📎 a.jpg\n📎 b.pdf');
  });

  it('a file-only message is the file lines alone', () => {
    expect(displayOf('', ['a.jpg'])).toBe('📎 a.jpg');
  });
});
