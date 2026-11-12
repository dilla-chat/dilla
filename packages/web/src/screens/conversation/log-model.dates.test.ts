// @vitest-environment node
import { afterEach, expect, it, vi } from 'vitest';
import { dayLabel } from './log-model.ts';

afterEach(() => { vi.unstubAllGlobals(); });

it('names an older day in the system region: a Swedish month under sv-SE, an English one under en-US', () => {
  const ts = new Date(2026, 9, 3, 12, 0).getTime() / 1000;
  const now = new Date(2026, 10, 3, 12, 0).getTime();
  vi.stubGlobal('navigator', { language: 'sv-SE' });
  expect(dayLabel(ts, now)).toMatch(/okt/);
  vi.stubGlobal('navigator', { language: 'en-US' });
  expect(dayLabel(ts, now)).toBe('Oct 3, 2026');
});
