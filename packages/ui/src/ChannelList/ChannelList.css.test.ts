// @vitest-environment node
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

const read = (rel: string) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), 'utf8');
const list = read('./ChannelList.css');
const chrow = read('../ChannelRow/ChannelRow.css');
const slot = read('../TrailingSlot/TrailingSlot.css');
const rule = (css: string, selector: string) => new RegExp(`(?:^|\\n)${selector.replace(/[.*+?^${}()|[\]\\>]/g, '\\$&')} \\{([^}]*)\\}`).exec(css)?.[1] ?? '';

// web3.9 (UI-2, R10-R14): one row grammar for channel rows, voice rows and category headers.
describe('the row grammar (CSS pin)', () => {
  it('a channel row is var(--row-h) tall with var(--list-gap) above and below', () => {
    const d = rule(chrow, '.d-chrow');
    expect(d).toMatch(/height: var\(--row-h\);/);
    expect(d).toMatch(/margin: var\(--list-gap\) 0;/);
  });
  it('the glyph column is 14px and does not shrink', () => {
    const d = rule(chrow, '.d-chrow__glyph');
    expect(d).toMatch(/flex: none;/);
    expect(d).toMatch(/width: 0\.875rem/);
  });
  it('the name truncates with an ellipsis', () => {
    const d = rule(chrow, '.d-chrow__name');
    expect(d).toMatch(/min-width: 0;/);
    expect(d).toMatch(/text-overflow: ellipsis;/);
  });
  it('the trailing slot is a fixed column that never shrinks', () => {
    const d = rule(slot, '.d-slot');
    expect(d).toMatch(/width: var\(--slot-w\);/);
    expect(d).toMatch(/flex: none;/);
  });
  it('selected is the fill and a 2px accent bar that grows with scaleY; active, unread and mention names are 600 (Signal §5.4)', () => {
    expect(rule(chrow, '.d-chrow:is([aria-current="page"], [data-selected="true"])')).toMatch(/background: var\(--surface-hi\);/);
    expect(rule(chrow, '.d-chrow__bar')).toMatch(/width: 2px;[^]*transform: scaleY\(0\);[^]*transition: transform var\(--duration-normal\) var\(--ease-out\);/);
    expect(rule(chrow, '.d-chrow:is([aria-current="page"], [data-selected="true"]) .d-chrow__bar')).toMatch(/transform: scaleY\(1\);/);
    expect(chrow).toMatch(/:not\(\[data-mentions="0"\]\)\) \.d-chrow__name \{ font-weight: var\(--weight-label\); \}/);
    expect(chrow).not.toMatch(/inset 4px 0 0/);
  });
  it('the lock is an icon box, not text', () => {
    expect(rule(chrow, '.d-chrow__lock')).not.toMatch(/font-size|filter/);
  });
});

describe('the category row (CSS pin, Signal §5.4)', () => {
  it('is 26px (--row-h-sub) tall, 10px below the row above, padding 0 8px, gap 6px', () => {
    const d = rule(list, '.d-channel-list__cat-head');
    expect(d).toMatch(/height: var\(--row-h-sub\);/);
    expect(d).toMatch(/padding: 0 0\.5rem;/);
    expect(d).toMatch(/gap: 6px;/);
    expect(d).toMatch(/margin: 10px 0 0;/);
  });
  it('the name is 10px/600 uppercase at the wide tracking in --fg-3, --fg on hover', () => {
    const d = rule(list, '.d-channel-list__cat-head');
    expect(d).toMatch(/font-size: var\(--text-micro\);/);
    expect(d).toMatch(/font-weight: var\(--weight-label\);/);
    expect(d).toMatch(/letter-spacing: var\(--label-tracking-wide\);/);
    expect(d).toMatch(/text-transform: uppercase;/);
    expect(d).toMatch(/color: var\(--fg-3\);/);
    expect(rule(list, '.d-channel-list__cat-head:hover')).toMatch(/color: var\(--fg\);/);
  });
  it('the chevron turns -90deg over 200ms when collapsed; a hairline fills the rest of the row', () => {
    expect(rule(list, '.d-channel-list__cat-chevron')).toMatch(/transition: transform var\(--duration-turn\) var\(--ease-out\);/);
    expect(list).toMatch(/\[data-collapsed="true"\] \.d-channel-list__cat-chevron \{ transform: rotate\(-90deg\); \}/);
    const r = rule(list, '.d-channel-list__cat-rule');
    expect(r).toMatch(/flex: 1;/);
    expect(r).toMatch(/height: 1px;/);
    expect(r).toMatch(/background: var\(--hairline\);/);
  });
  it('the "+" shares the row right edge, shows on hover or focus, and always on a device without hover or on a phone', () => {
    expect(rule(list, '.d-channel-list__cat-add')).toMatch(/right: 0\.5rem;/);
    expect(list).toMatch(/\.d-channel-list__cat:focus-within \.d-channel-list__cat-add \{ opacity: 1; \}/);
    expect(list).toMatch(/@media \(hover: none\) \{ \.d-channel-list__cat-add \{ opacity: 1; \} \}/);
    expect(list).toMatch(/\[data-layout="phone"\] \.d-channel-list__cat-add \{ opacity: 1; \}/);
  });
});

describe('the sidebar head, stat line and scroller (CSS pin, Signal §5.4)', () => {
  it('pads the head 14px 12px 8px 14px; the server name is the 19px display serif italic', () => {
    const t = rule(list, '.d-channel-list__title');
    expect(t).toMatch(/padding: 14px 12px 8px 14px;/);
    expect(t).toMatch(/font-family: var\(--font-serif-italic\);/);
    expect(t).toMatch(/font-size: var\(--text-title\);/);
    expect(rule(list, '.d-channel-list__head')).toMatch(/padding: 14px 12px 8px 14px;/);
  });
  it('the stat line is 10.5px --fg-3 with a 6px --ok dot', () => {
    const d = rule(list, '.d-channel-list__stats');
    expect(d).toMatch(/font-size: var\(--text-meta\);/);
    expect(d).toMatch(/color: var\(--fg-3\);/);
    expect(d).toMatch(/padding: 0 14px 10px;/);
    expect(rule(list, '.d-channel-list__live')).toMatch(/width: 6px;[^}]*background: var\(--ok\);/);
  });
  it('the scroller is padded 4px 8px under a hairline', () => {
    const d = rule(list, '.d-channel-list__list');
    expect(d).toMatch(/padding: 4px 8px;/);
    expect(d).toMatch(/border-top: 1px solid var\(--hairline\);/);
  });
  it('the retired list-header "+" row and text join have no rules', () => {
    expect(list).not.toContain('.d-channel-list__list-head');
    expect(list).not.toContain('.d-channel-list__join');
  });
});
