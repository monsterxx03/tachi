import { humanTokens } from './num';

describe('humanTokens', () => {
  it('keeps small numbers exact', () => {
    expect(humanTokens(0)).toBe('0');
    expect(humanTokens(835)).toBe('835');
    expect(humanTokens(999)).toBe('999');
  });

  it('uses one decimal only where it means something', () => {
    expect(humanTokens(1000)).toBe('1k');
    expect(humanTokens(1234)).toBe('1.2k');
    expect(humanTokens(9949)).toBe('9.9k');
  });

  it('rounds to whole k above that — a token count is not that precise', () => {
    expect(humanTokens(12_345)).toBe('12k');
    expect(humanTokens(200_000)).toBe('200k');
    expect(humanTokens(999_499)).toBe('999k');
  });

  it('switches to M at a million', () => {
    expect(humanTokens(1_000_000)).toBe('1M');
    expect(humanTokens(1_250_000)).toBe('1.3M');
  });

  it('never prints NaN or a negative', () => {
    expect(humanTokens(Number.NaN)).toBe('0');
    expect(humanTokens(-5)).toBe('0');
  });
});
