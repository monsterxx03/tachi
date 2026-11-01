import type { SessionSummary } from '../api/types';
import type { AgentPanelRow } from '../api/types';
import { agentRef, hashQuery, hashRefAt, matchAgents, matchSessions, sessionRef, spliceRef } from './refs';

function session(id: string, title: string): SessionSummary {
  return { id, title, updatedAt: '刚刚', running: false, waiting: '', readOnly: false, heldByTask: false };
}

describe('hashRefAt', () => {
  it('finds the reference the caret sits in', () => {
    expect(hashRefAt('看看 #s:ab', 8)).toEqual({ start: 3, query: 's:ab' });
  });

  it('starts at the beginning of the text too', () => {
    expect(hashRefAt('#s:', 3)).toEqual({ start: 0, query: 's:' });
  });

  it('reads only up to the caret, not the whole text', () => {
    // caret 是字符之间的位置：4 落在 'a' 后面，所以读到的是 's:a'，剩下的 'bcd' 不算
    expect(hashRefAt('#s:abcd', 4)).toEqual({ start: 0, query: 's:a' });
  });

  it('is null once the caret leaves the reference behind a space', () => {
    expect(hashRefAt('#s:ab tail', 9)).toBeNull();
  });

  it('does not treat a # glued to a word as a reference', () => {
    // C#、邮箱、URL 里的 fragment：都不是引用的开头
    expect(hashRefAt('C#', 2)).toBeNull();
    expect(hashRefAt('a#b', 3)).toBeNull();
  });

  it('is null when there is no # at all', () => {
    expect(hashRefAt('普通的一句话', 6)).toBeNull();
    expect(hashRefAt('', 0)).toBeNull();
  });
});

describe('hashQuery', () => {
  it('opens the session list for the session prefix', () => {
    expect(hashQuery('s:')).toEqual({ kind: 'sessions', rest: '' });
    expect(hashQuery('s:清理')).toEqual({ kind: 'sessions', rest: '清理' });
  });

  it('treats a bare # and the prefix being typed as candidates', () => {
    expect(hashQuery('')).toEqual({ kind: 'candidates' });
    expect(hashQuery('s')).toEqual({ kind: 'candidates' });
  });

  it('leaves everything else alone', () => {
    // 这是「#include 不会被弹窗打断」的那条规则在手机上的同一个答案
    for (const q of ['include', '123', 'x', 'S:']) {
      expect(hashQuery(q)).toEqual({ kind: 'none' });
    }
  });
});

describe('matchSessions', () => {
  const rows = [session('s1', '清理构建目录'), session('s2', 'Mobile refs'), session('s3', '')];

  it('lists every other conversation for an empty query', () => {
    expect(matchSessions(rows, '', 's9', 10).map((s) => s.id)).toEqual(['s1', 's2', 's3']);
  });

  it('filters by title, case-insensitively, and by id prefix', () => {
    expect(matchSessions(rows, 'MOBILE', 's9', 10).map((s) => s.id)).toEqual(['s2']);
    expect(matchSessions(rows, '清理', 's9', 10).map((s) => s.id)).toEqual(['s1']);
    expect(matchSessions(rows, 's3', 's9', 10).map((s) => s.id)).toEqual(['s3']);
  });

  it('never offers the conversation being written in', () => {
    expect(matchSessions(rows, '', 's1', 10).map((s) => s.id)).toEqual(['s2', 's3']);
  });

  it('stops at the limit', () => {
    expect(matchSessions(rows, '', 's9', 2).map((s) => s.id)).toEqual(['s1', 's2']);
  });
});

describe('spliceRef', () => {
  it('replaces what was typed after the mark and leaves the caret after the ref', () => {
    const out = spliceRef('看看 #s:ab 吧', 3, 8, sessionRef('s1'));
    expect(out.value).toBe('看看 #s:s1 吧');
    expect(out.caret).toBe(8);
  });

  it('adds the separating space when text follows immediately', () => {
    const out = spliceRef('#s:ab后面还有字', 0, 5, sessionRef('s1'));
    expect(out.value).toBe('#s:s1 后面还有字');
    expect(out.caret).toBe(6);
  });

  it('adds the trailing space at the end of the text too, so the picker closes', () => {
    const out = spliceRef('#s:', 0, 3, sessionRef('s1'));
    expect(out.value).toBe('#s:s1 ');
    expect(out.caret).toBe(6);
  });
});

describe('hashQuery 的 a: 命名空间', () => {
  it('opens the node list for the agent prefix', () => {
    expect(hashQuery('a:')).toEqual({ kind: 'agents', rest: '' })
    expect(hashQuery('a:9e47')).toEqual({ kind: 'agents', rest: '9e47' })
  })

  it('still opens the session list for s:', () => {
    expect(hashQuery('s:清理')).toEqual({ kind: 'sessions', rest: '清理' })
  })
});

describe('matchAgents', () => {
  const row = (id: string, label: string): AgentPanelRow => ({
    name: label || id,
    label,
    id,
    url: 'http://x:8790',
    note: '',
    reach: '可达',
    skills: [],
    err: '',
  });
  const rows = [row('9e47c7a3aaaa', 'nas'), row('db0906d1bbbb', ''), row('', 'unpinned')]

  it('never offers a row this machine holds no identity for', () => {
    // 空的 #a: 看着像引用、插进去解析不了：宁可没有这一行
    expect(matchAgents(rows, '', 10).map((r) => r.id)).toEqual(['9e47c7a3aaaa', 'db0906d1bbbb'])
  })

  it('filters by label or by identity prefix', () => {
    expect(matchAgents(rows, 'NAS', 10).map((r) => r.id)).toEqual(['9e47c7a3aaaa'])
    expect(matchAgents(rows, 'db09', 10).map((r) => r.id)).toEqual(['db0906d1bbbb'])
  })

  it('renders the reference with the identity, which is what agentref resolves', () => {
    expect(agentRef('9e47c7a3aaaa')).toBe('#a:9e47c7a3aaaa')
  })
});
