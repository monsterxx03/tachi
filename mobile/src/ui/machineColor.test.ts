import { machineColor, machineHue } from './machineColor';

describe('machineHue', () => {
  it('同一个键永远同一个色相（换手机、重装都一样）', () => {
    expect(machineHue('mock-mac')).toBe(machineHue('mock-mac'));
    expect(machineHue('a'.repeat(64))).toBe(machineHue('a'.repeat(64)));
  });

  it('大量键会铺满整条色相带（不是挤在几个值上）', () => {
    const hues = new Set<number>();
    for (let i = 0; i < 500; i++) hues.add(machineHue(`device-${i}`));
    expect(hues.size).toBe(24);
  });

  it('色相在 0–345 之间、按 15 度一档', () => {
    for (const key of ['a', 'b', 'c', '长一点的键', 'x'.repeat(100)]) {
      const hue = machineHue(key);
      expect(hue).toBeGreaterThanOrEqual(0);
      expect(hue).toBeLessThan(360);
      expect(hue % 15).toBe(0);
    }
  });
});

describe('machineColor', () => {
  it('深浅两个主题各一套亮度，都带同一个色相', () => {
    const hue = machineHue('mock-mac');
    expect(machineColor('mock-mac', false)).toBe(`hsl(${hue}, 48%, 38%)`);
    expect(machineColor('mock-mac', true)).toBe(`hsl(${hue}, 55%, 70%)`);
  });
});
