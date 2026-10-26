import { defaultLabel, deviceLabel, hostOf } from './label';

/**
 * 列表里那一行写什么，决定了用户能不能认出「这是哪台机器」。产品名每台安装都一样，所以它只能
 * 在「服务端什么都没说」时兜底；机器名（hostname）才是默认。
 */
describe('deviceLabel', () => {
  it('用机器自报的名字', () => {
    expect(deviceLabel({ hostname: 'nas', name: 'tachi-desktop' }, 'http://192.168.1.23:8787')).toBe('nas');
  });

  it('macOS 的 .local 后缀不带进列表', () => {
    expect(deviceLabel({ hostname: 'yejia-MacBook-Pro.local' }, 'http://192.168.1.5:8787')).toBe(
      'yejia-MacBook-Pro',
    );
  });

  it('老服务端没有 hostname 时退回产品名 + 地址', () => {
    expect(deviceLabel({ name: 'tachi-desktop' }, 'http://192.168.1.5:8787')).toBe(
      'tachi-desktop · 192.168.1.5:8787',
    );
  });

  it('什么都没有时只剩地址', () => {
    expect(deviceLabel({}, 'http://192.168.1.5:8787')).toBe('192.168.1.5:8787');
  });

  it('空白 hostname 不算名字', () => {
    expect(deviceLabel({ hostname: '   ' }, 'http://10.0.0.2:8787')).toBe('10.0.0.2:8787');
  });
});

describe('defaultLabel', () => {
  it('清空输入时回到机器名', () => {
    expect(defaultLabel({ hostname: 'nas', baseUrl: 'http://10.0.0.2:8787' })).toBe('nas');
  });

  it('改名之后 hostname 仍然留着，所以恢复默认仍然可用', () => {
    // 存的是 {label: '家里的 NAS', hostname: 'nas'} —— 后者是恢复默认的依据
    const device = { hostname: 'nas', baseUrl: 'http://10.0.0.2:8787' };
    expect(defaultLabel(device)).toBe('nas');
  });
});

describe('hostOf', () => {
  it('去掉协议和末尾斜杠', () => {
    expect(hostOf('http://192.168.1.5:8787/')).toBe('192.168.1.5:8787');
    expect(hostOf('https://nas.example.com')).toBe('nas.example.com');
  });
});
