import { fileKind } from './fileKind';

const md = { mimeType: 'text/markdown', name: 'report.md' };

describe('fileKind', () => {
  it('renders markdown rather than dumping its source', () => {
    expect(fileKind(md)).toBe('markdown');
    expect(fileKind({ mimeType: 'text/x-markdown', name: 'README' })).toBe('markdown');
    // text/markdown 也以 text/ 开头：判成 text 就是这次修的那个 bug（一份 md 当源码摊在屏幕上）
    expect(fileKind(md)).not.toBe('text');
  });

  it('falls back to the extension when the mime says plain text', () => {
    expect(fileKind({ mimeType: 'text/plain', name: 'notes.markdown' })).toBe('markdown');
    expect(fileKind({ mimeType: 'application/octet-stream', name: 'CHANGELOG.MD' })).toBe('markdown');
  });

  it('does not mistake a name that merely contains md', () => {
    expect(fileKind({ mimeType: 'text/plain', name: 'cmd.txt' })).toBe('text');
    expect(fileKind({ mimeType: 'application/octet-stream', name: 'notes.md.bak' })).toBe('other');
  });

  it('keeps the other three kinds', () => {
    expect(fileKind({ mimeType: 'image/png', name: 'shot.png' })).toBe('image');
    expect(fileKind({ mimeType: 'text/plain', name: 'log.txt' })).toBe('text');
    expect(fileKind({ mimeType: 'application/json', name: 'data.json' })).toBe('text');
    expect(fileKind({ mimeType: 'application/pdf', name: 'spec.pdf' })).toBe('other');
  });
});
