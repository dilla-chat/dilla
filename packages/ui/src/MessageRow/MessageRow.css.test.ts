// @vitest-environment node
// Task 7 (README §5.5): the prototype's row is a fixed 44px time gutter, a 30px avatar and the content, 10px apart. jsdom
// cannot lay out, so the rules are pinned as text; the stories show them in a real browser (parity shots, check-stories).
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

const css = readFileSync(fileURLToPath(new URL('./MessageRow.css', import.meta.url)), 'utf8');
/** The whole rule that starts a line with `selector {`, across lines. */
const rule = (selector: string): string => {
  const start = css.split('\n').findIndex(line => line.startsWith(`${selector} {`));
  if (start < 0) throw new Error(`no rule ${selector}`);
  const rest = css.split('\n').slice(start).join('\n');
  return rest.slice(0, rest.indexOf('}') + 1).replace(/\s+/g, ' ');
};

describe('the Signal message row', () => {
  it('is the prototype grid: 44px gutter, 30px avatar, content; 10px apart; 7px 16px 3px 0 padding', () => {
    const row = rule('.d-message-row');
    expect(row).toContain('grid-template-columns: 2.75rem /* 44px */ 1.875rem /* 30px */ minmax(0, 1fr)');
    expect(row).toContain('column-gap: 0.625rem');
    expect(row).toContain('padding: 0.4375rem 1rem 0.1875rem 0');
    expect(rule('.d-message-row[data-grouped="true"]')).toContain('padding: 0.0625rem 1rem 0.0625rem 0');
  });
  it('enters with d-up over 380ms, staggered 40ms per first-paint row, filled backwards only (no lasting stacking context)', () => {
    expect(rule('.d-message-row')).toMatch(/animation: d-up var\(--duration-enter\) var\(--ease-out\) backwards;\s*animation-delay: calc\(var\(--d-enter-i, 0\) \* var\(--duration-stagger\)\)/);
  });
  it('hovers with bg-3 and a 2px accent bar; a mention of me is warn-soft with the mention bar', () => {
    expect(css).toMatch(/\.d-message-row:hover, \.d-message-row\[data-hover="true"\] \{ background: var\(--bg-3\); box-shadow: inset 2px 0 0 var\(--accent\); \}/);
    expect(rule('.d-message-row[data-mention="true"]')).toContain('background: var(--warn-soft); box-shadow: inset 2px 0 0 var(--mention)');
  });
  it('keeps the gutter time on one line, clipped inside its column', () => {
    const gutter = rule('.d-message-row__gutter');
    expect(gutter).toContain('white-space: nowrap');
    expect(gutter).toContain('overflow: hidden');
    expect(gutter).toContain('text-align: right');
  });
  it('floats the toolbar at top -12px, right 16px, rising 3px in', () => {
    const tb = rule('.d-message-row__toolbar');
    expect(tb).toContain('top: -0.75rem; right: 1rem');
    expect(tb).toContain('transform: translateY(3px)');
  });
});
