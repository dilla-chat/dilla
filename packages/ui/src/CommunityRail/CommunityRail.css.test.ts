// @vitest-environment node
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

const css = readFileSync(fileURLToPath(new URL('./CommunityRail.css', import.meta.url)), 'utf8');
const rule = (selector: string) => new RegExp(`(?:^|\\n)${selector.replace(/[.*+?^${}()|[\]\\>]/g, '\\$&')} \\{([^}]*)\\}`).exec(css)?.[1] ?? '';

// Signal (README §5.3): the 56px rail of round tiles that morph to 10px squares.
describe('the rail (CSS pin)', () => {
  it('draws its edge as an inset hairline, not a border (B3)', () => {
    const d = rule('.d-community-rail');
    expect(d).toMatch(/box-shadow: inset -1px 0 var\(--hairline\);/);
    expect(d).not.toMatch(/border/);
  });
  it('a tile is a var(--rail-tile) circle on --hover; the list is 10px from the top with 6px gaps', () => {
    const d = rule('.d-community-rail__item');
    expect(d).toMatch(/width: var\(--rail-tile\);/);
    expect(d).toMatch(/height: var\(--rail-tile\);/);
    expect(d).toMatch(/background: var\(--hover\);/);
    expect(d).toMatch(/border-radius: var\(--r-pill\);/);
    expect(d).toMatch(/transition: border-radius var\(--duration-normal\) var\(--ease-out\)/);
    const l = rule('.d-community-rail__list');
    expect(l).toMatch(/padding: 10px 0;/);
    expect(l).toMatch(/gap: 6px;/);
  });
  it('hover morphs to 10px; selected is the accent square with the ring', () => {
    expect(rule('.d-community-rail__item:hover')).toMatch(/border-radius: var\(--r-xl\);/);
    const sel = rule('.d-community-rail__item[aria-current="page"]');
    expect(sel).toMatch(/background: var\(--accent\);/);
    expect(sel).toMatch(/box-shadow: 0 0 0 2px var\(--bg-2\), 0 0 0 3\.5px var\(--accent\);/);
    expect(rule('.d-community-rail__home[aria-current="page"]')).toMatch(/background: var\(--surface-hi\);/);
  });
  it('has no left-edge pill (the prototype draws none)', () => {
    expect(css).not.toContain('__pill');
  });
  it('the add tile is a dashed --control-edge-key circle (#3A404A in signal) that turns 90deg on hover', () => {
    const d = rule('.d-community-rail__glyph');
    expect(d).toMatch(/width: var\(--rail-tile\);/);
    expect(d).toMatch(/border: 1px dashed var\(--control-edge-key\);/);
    expect(rule('.d-community-rail__join:hover .d-community-rail__glyph')).toMatch(/transform: rotate\(90deg\);/);
  });
  it('the mention badge pops in at the bottom right', () => {
    expect(rule('.d-community-rail__badge')).toMatch(/animation: d-pop var\(--duration-rail-badge\) var\(--ease-out\) both;/);
  });
});
