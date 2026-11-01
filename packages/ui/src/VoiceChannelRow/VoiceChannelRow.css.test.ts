// @vitest-environment node
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

const row = readFileSync(fileURLToPath(new URL('./VoiceChannelRow.css', import.meta.url)), 'utf8');
const tile = readFileSync(fileURLToPath(new URL('../CallTile/CallTile.css', import.meta.url)), 'utf8');

/** The declarations of the first rule whose selector is exactly `selector`. */
function rule(css: string, selector: string): string {
  const m = new RegExp(`(?:^|\\n)${selector.replace(/[.[\]]/g, '\\$&')} \\{([^}]*)\\}`).exec(css);
  if (m === null) throw new Error(`no rule for ${selector}`);
  return m[1];
}

// web3.8 (U3, R29): the name and its glyphs share one non-wrapping, centred line.
describe('inline state glyph rows (CSS pin)', () => {
  it.each([
    ['.d-call-tile__caption', tile, '0.375rem'],
  ])('%s is display: flex; align-items: center; flex-wrap: nowrap; with its gap', (selector, css, gap) => {
    const d = rule(css, selector);
    expect(d).toMatch(/display: flex;/);
    expect(d).toMatch(/align-items: center;/);
    expect(d).toMatch(/flex-wrap: nowrap;/);
    expect(d).toContain(`gap: ${gap};`);
  });
  it.each([
    ['.d-voice-row__person-name', row],
    ['.d-call-tile__name', tile],
  ])('%s truncates: ellipsis, min-width: 0, flex: 0 1 auto', (selector, css) => {
    const d = rule(css, selector);
    expect(d).toMatch(/min-width: 0;/);
    expect(d).toMatch(/flex: 0 1 auto;/);
    expect(d).toMatch(/text-overflow: ellipsis;/);
  });
  it.each([
    ['.d-voice-row__state', row],
    ['.d-call-tile__state', tile],
  ])('%s does not shrink', (selector, css) => {
    expect(rule(css, selector)).toMatch(/flex: none;/);
  });
});

// web3.9 (UI-2, R10-R13, R25): one row grammar with ChannelRow.
describe('the voice row grammar (CSS pin)', () => {
  it('.d-voice-row is --row-h tall with --list-gap above and below', () => {
    const d = rule(row, '.d-voice-row');
    expect(d).toMatch(/height: var\(--row-h\);/);
    expect(d).toMatch(/margin: var\(--list-gap\) 0;/);
  });
  it('the glyph column is 14px and does not shrink', () => {
    const d = rule(row, '.d-voice-row__glyph');
    expect(d).toMatch(/flex: none;/);
    expect(d).toMatch(/width: 0\.875rem/);
  });
  it('a person row is --row-h-sub (26px) tall, 12px --fg-strong, gap 8px; the list is 30px in (Signal §5.4)', () => {
    const d = rule(row, '.d-voice-row__person');
    expect(d).toMatch(/height: var\(--row-h-sub\);/);
    expect(d).toMatch(/gap: 8px;/);
    expect(d).toMatch(/font-size: var\(--text-ui\);/);
    expect(d).toMatch(/color: var\(--fg-strong\);/);
    expect(rule(row, '.d-voice-row__people')).toMatch(/padding: 0 0\.5rem 0 30px;/);
  });
  it('the join icon ring is inset', () => {
    expect(row).toMatch(/\.d-voice-row__join \.d-icon-button:focus-visible \{ outline-offset: -2px; \}/);
  });
  it('speaking is only a ring on the avatar: 0 0 0 2px bg-2, 0 0 0 3.5px --live (--ok), over 150ms', () => {
    expect(row).toMatch(/\[data-speaking="true"\] \.d-avatar \{ box-shadow: 0 0 0 2px var\(--bg-2\), 0 0 0 3\.5px var\(--ok\); \}/);
    expect(rule(row, '.d-voice-row__avatar .d-avatar')).toMatch(/transition: box-shadow var\(--duration-fast\);/);
  });
  it('the join icon sits over the row slot at the same right edge and shows on hover or focus', () => {
    const d = rule(row, '.d-voice-row__join');
    expect(d).toMatch(/right: 0\.5rem;/);
    expect(d).toMatch(/opacity: 0;/);
    expect(row).toMatch(/\.d-voice-item:focus-within \.d-voice-row__join \{ opacity: 1; \}/);
  });
  it('the count never shrinks and the name cuts first', () => {
    expect(rule(row, '.d-voice-row__count')).toMatch(/flex: none;/);
    expect(rule(row, '.d-voice-row__name')).toMatch(/text-overflow: ellipsis;/);
  });
  it('V1, Signal: a joined row has the --live glyph and a --fg 600 name; the current row the accent glyph and the 2px bar', () => {
    expect(row).not.toContain('.d-voice-row__connected');
    expect(row).toMatch(/\.d-voice-row\[data-joined="true"\] \.d-voice-row__glyph \{ color: var\(--ok\); \}/);
    expect(row).toMatch(/\.d-voice-row\[data-joined="true"\] \{ color: var\(--fg\); \}/);
    expect(row).toMatch(/\[data-joined="true"\]\) \.d-voice-row__name \{ font-weight: var\(--weight-label\); \}/);
    expect(row).toMatch(/\.d-voice-row\[aria-current="page"\] \.d-voice-row__bar \{ transform: scaleY\(1\); \}/);
  });
  it('V2: the participant slot grows with its glyphs, 8px apart, right-aligned to the row trailing edge', () => {
    const d = rule(row, '.d-voice-row__person > .d-slot');
    expect(d).toMatch(/width: auto;/);
    expect(d).toMatch(/gap: 8px;/);
    expect(d).toMatch(/justify-content: flex-end;/);
  });
  it('the count is 10.5px --fg-3, tabular', () => {
    const d = rule(row, '.d-voice-row__count');
    expect(d).toMatch(/font-size: var\(--text-meta\);/);
    expect(d).toMatch(/color: var\(--fg-3\);/);
    expect(d).toMatch(/font-variant-numeric: tabular-nums;/);
  });
});
