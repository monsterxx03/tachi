import type { SessionCache } from '../api/cache';
import type { Device } from '../api/storage';
import type { SessionSummary } from '../api/types';
import { buildOverview, sectionOf, waitingCount } from './overview';

function s(patch: Partial<SessionSummary> & { id: string; updatedAt: string }): SessionSummary {
  return {
    title: patch.id,
    running: false,
    waiting: '',
    readOnly: false,
    heldByTask: false,
    ...patch,
  };
}

function dev(patch: Partial<Device> & { baseUrl: string; token: string }): Device {
  return { label: patch.baseUrl, ...patch } as Device;
}

describe('sectionOf', () => {
  it('等你动手优先于在跑：两个都真时归 waiting', () => {
    expect(sectionOf(s({ id: 'a', updatedAt: '', waiting: '等待你的授权', running: true }))).toBe('waiting');
    expect(sectionOf(s({ id: 'b', updatedAt: '', running: true }))).toBe('running');
    expect(sectionOf(s({ id: 'c', updatedAt: '' }))).toBe('rest');
  });
});

describe('buildOverview', () => {
  const mac = dev({ baseUrl: 'http://10.0.0.1:8787', token: 't1', deviceId: 'mac', label: 'MacBook' });
  const nas = dev({ baseUrl: 'http://10.0.0.2:8787', token: 't2', deviceId: 'nas', label: 'NAS' });
  const devices = [mac, nas];

  it('把各台的会话拼成一列，并标上机器', () => {
    const cache: SessionCache = {
      mac: { at: 1, sessions: [s({ id: 'm1', updatedAt: '2026-11-01T10:00:00Z' })] },
      nas: { at: 1, sessions: [s({ id: 'n1', updatedAt: '2026-11-01T11:00:00Z' })] },
    };
    const rows = buildOverview(devices, cache, {});
    expect(rows.map((r) => r.session.id)).toEqual(['n1', 'm1']);
    expect(rows.map((r) => r.machine)).toEqual(['NAS', 'MacBook']);
    expect(rows.map((r) => r.device)).toEqual([1, 0]);
  });

  it('等你动手的不看时间，浮到最上面；同段内再按时间', () => {
    const cache: SessionCache = {
      mac: {
        at: 1,
        sessions: [
          s({ id: 'old-wait', updatedAt: '2026-10-01T10:00:00Z', waiting: '等待你的回答' }),
          s({ id: 'new-run', updatedAt: '2026-11-01T10:00:00Z', running: true }),
          s({ id: 'new-rest', updatedAt: '2026-11-01T12:00:00Z' }),
        ],
      },
    };
    expect(buildOverview([mac], cache, {}).map((r) => r.session.id)).toEqual(['old-wait', 'new-run', 'new-rest']);
  });

  it('置顶在同一段里排在时间前面', () => {
    const cache: SessionCache = {
      mac: {
        at: 1,
        sessions: [
          s({ id: 'newer', updatedAt: '2026-11-01T12:00:00Z' }),
          s({ id: 'pinned', updatedAt: '2026-10-01T10:00:00Z', pinned: true }),
        ],
      },
    };
    expect(buildOverview([mac], cache, {}).map((r) => r.session.id)).toEqual(['pinned', 'newer']);
  });

  it('明确读不到的机器标成陈旧；还没刷新过的不标（启动时会先画缓存再刷新）', () => {
    const cache: SessionCache = {
      mac: { at: 1, sessions: [s({ id: 'm1', updatedAt: '' })] },
      nas: { at: 1, sessions: [s({ id: 'n1', updatedAt: '' })] },
    };
    const rows = buildOverview(devices, cache, { nas: false });
    expect(rows.find((r) => r.session.id === 'n1')?.stale).toBe(true);
    expect(rows.find((r) => r.session.id === 'm1')?.stale).toBe(false);
  });

  it('同一段里陈旧的行排在新鲜的行后面（连不上的机器不能盖住真的等待）', () => {
    const cache: SessionCache = {
      mac: { at: 1, sessions: [s({ id: 'old-wait', updatedAt: '2026-11-01T10:00:00Z', waiting: '等待你的授权' })] },
      nas: { at: 1, sessions: [s({ id: 'new-wait', updatedAt: '2026-10-01T10:00:00Z', waiting: '等待你的回答' })] },
    };
    const rows = buildOverview(devices, cache, { mac: false });
    expect(rows.map((r) => r.session.id)).toEqual(['new-wait', 'old-wait']);
  });

  it('没有缓存的机器不出行（不是出一行空的分组标题）', () => {
    expect(buildOverview(devices, { mac: { at: 1, sessions: [] } }, {})).toEqual([]);
  });

  it('时间完全一样时顺序稳定（不会每次刷新跳一下）', () => {
    const cache: SessionCache = {
      mac: { at: 1, sessions: [s({ id: 'b', updatedAt: '2026-11-01T10:00:00Z' })] },
      nas: { at: 1, sessions: [s({ id: 'a', updatedAt: '2026-11-01T10:00:00Z' })] },
    };
    const once = buildOverview(devices, cache, {}).map((r) => r.session.id);
    const twice = buildOverview(devices, cache, {}).map((r) => r.session.id);
    expect(once).toEqual(twice);
    expect(once).toEqual(['b', 'a']);
  });
});

describe('waitingCount', () => {
  it('只数等你的，在跑的不算', () => {
    const cache: SessionCache = {
      mac: {
        at: 1,
        sessions: [
          s({ id: 'a', updatedAt: '', waiting: '等待你的授权' }),
          s({ id: 'b', updatedAt: '', waiting: '等待你的回答' }),
          s({ id: 'c', updatedAt: '', running: true }),
        ],
      },
    };
    expect(waitingCount(cache, 'mac')).toBe(2);
    expect(waitingCount(cache, 'nas')).toBe(0);
  });
});
