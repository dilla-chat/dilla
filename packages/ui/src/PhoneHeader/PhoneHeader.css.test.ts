// @vitest-environment node
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

const read = (name: string) => readFileSync(fileURLToPath(new URL(name, import.meta.url)), 'utf8');
const css = read('./PhoneHeader.css');
const base = read('../styles/base.css');
const rule = /(?:^|\n)\.d-phone-header \{([^}]*)\}/.exec(css)?.[1] ?? '';

/** The header's border-box height for a safe-area inset, from the rule's own numbers (rem = 16 px). */
function height(inset: number, zeroedPadding: boolean): number {
  expect(rule).toMatch(/box-sizing: content-box;/);
  const min = /min-height: calc\(([\d.]+)rem - (\d+)px\);/.exec(rule);
  expect(min, 'min-height is calc(Nrem - Npx)').not.toBeNull();
  const content = Number(min?.[1]) * 16 - Number(min?.[2]);
  const border = Number(/border-bottom: (\d+)px solid/.exec(rule)?.[1]);
  const padTop = zeroedPadding ? 0 : /padding: env\(safe-area-inset-top\) /.test(rule) ? inset : NaN;
  return content + padTop + border;
}

// Signal §5.10: the bar is 52 with its hairline, and the notch's inset only adds to it where the header itself clears the
// notch. A header whose padding-top base.css zeroes (the ring or the link banner cleared it) is exactly 52.
describe('the phone header box (CSS pin)', () => {
  it('is 52 high with no inset, whichever state', () => {
    expect(height(0, false)).toBe(52);
    expect(height(0, true)).toBe(52);
  });
  it('is 52 plus the inset when it clears the notch, and 52 when its padding is zeroed', () => {
    expect(height(47, false)).toBe(99);
    expect(height(47, true)).toBe(52);
  });
  it('is zeroed by base.css in the ring and link-banner states', () => {
    expect(base).toMatch(/\[data-layout="phone"\]:has\(\.d-app-shell__ring\) \.d-app-shell__main \.d-phone-header \{ padding-top: 0; \}/);
    expect(base).toMatch(/\[data-layout="phone"\]:has\(\.d-link-banner:not\(:empty\)\) \.d-phone-header \{ padding-top: 0; \}/);
  });
});
