// @vitest-environment node
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

const css = readFileSync(fileURLToPath(new URL('./base.css', import.meta.url)), 'utf8');

// The no-zoom rules (Jonas, 2026-10-16; R10, R11): a field below 16px makes iOS zoom the page on focus.
const NO_ZOOM = `@media (max-width: 47.99rem), (pointer: coarse) and (max-height: 31.99rem) {
  .d-root :is(input:not([type="checkbox"], [type="radio"], [type="range"], [type="button"], [type="submit"], [type="reset"]), textarea, [contenteditable="true"]) { font-size: 1rem; }
}`;

describe('base.css phone rules', () => {
  it('holds the 16px text-field rule under the phone breakpoint, byte for byte (Task 11: selects keep the prototype size)', () => {
    expect(css).toContain(NO_ZOOM);
  });
  it('sets touch-action: manipulation on .d-root', () => {
    expect(css).toMatch(/\.d-root \{ touch-action: manipulation; \}/);
  });
  it('pads the fixed ring banner by the notch', () => {
    expect(css).toMatch(/\.d-app-shell__ring \{[^}]*padding-top: env\(safe-area-inset-top\)/);
  });
  it('pads a call bar by the home indicator when no tab bar follows it, and the composer then drops its own', () => {
    expect(css).toMatch(/__callbar:not\(:has\(\+ \.d-app-shell__tabbar:not\(\[hidden\]\)\)\) \{[^}]*padding-bottom: env\(safe-area-inset-bottom\)/);
    expect(css).toMatch(/:has\(\.d-app-shell__callbar\) \.d-composer \{ padding-bottom: 0\.5rem; \}/);
    expect(css).toMatch(/\[data-typing="true"\]\):has\(\.d-app-shell__tabbar\) \.d-composer \{ padding-bottom: 0\.5rem; \}/);
    expect(css).toMatch(/\.d-app-shell__tabbar\[hidden\], [^{]*\[data-typing="true"\] \.d-app-shell__tabbar \{ display: none; \}/);
    // Task 11 (Jonas, Q4): a phone sheet covers the whole screen, the tab bar included.
    expect(css).toMatch(/\.d-app-shell__aside, [^{]*__backdrop \{ bottom: 0; \}/);
  });
  it('leaves fullscreen settings to the modal frame and pads the thread sheet composer once', () => {
    expect(css).not.toMatch(/\.d-settings-frame[^{}]*\[data-modeless\]/);
    expect(css).toMatch(/\[data-layout="phone"\] \.d-app-shell__banner \{ position: relative; z-index: 21; \}/);
    expect(css).toMatch(/\.d-thread-pane\[data-phone\] \.d-composer \{ padding-bottom: 0\.5rem; \}/);
  });
  it('pads the phone grid, the ring and the aside by the landscape side insets', () => {
    expect(css).toMatch(/\[data-layout="phone"\] \.d-app-shell__grid \{[^}]*padding-left: env\(safe-area-inset-left\); padding-right: env\(safe-area-inset-right\)/);
    expect(css).toMatch(/__ring \{[^}]*padding-left: env\(safe-area-inset-left\)/);
    expect(css).toMatch(/__aside \{[^}]*padding-left: env\(safe-area-inset-left\)/);
  });
  it('a desktop aside grid has the top bar row, the panel area and no status row', () => {
    expect(css).toMatch(/__grid--aside \{[^}]*grid-template-areas: "topbar topbar topbar topbar" "banner banner banner banner" "rail sidebar main aside" "rail panel main aside"; \}/);
  });
  it('hides the list pane main with the hidden attribute winning over its flex display', () => {
    const shellCss = readFileSync(fileURLToPath(new URL('../AppShell/AppShell.css', import.meta.url)), 'utf8');
    expect(shellCss).toContain('.d-app-shell__main[hidden] { display: none; }');
  });
  it('never disables user scaling and no longer caps the sidebar at 35dvh', () => {
    expect(css).not.toContain('user-scalable');
    expect(css).not.toContain('35dvh');
  });
});

describe('web3.9 base.css rules (UI-1)', () => {
  it('sets the phone row tokens on the phone shell and on [data-phone]', () => {
    expect(css).toContain('.d-app-shell[data-layout="phone"], [data-phone] { --row-h: 2.75rem; --row-h-sub: 2.25rem; --row-h-2: 3.25rem; --slot-w: 44px; --list-gap: 0.0625rem; }');
  });
  it('keeps the prototype\'s 38px rail tiles on a phone (Jonas 2026-10-27, supersedes Ruling N)', () => {
    expect(css).not.toContain('--rail-tile: 44px');
  });
  it('insets the focus ring on a list row (R18), after the global ring', () => {
    const rule = '[data-row]:focus-visible { outline-offset: -2px; }';
    expect(css).toContain(rule);
    expect(css.indexOf(rule)).toBeGreaterThan(css.indexOf('.d-root :focus-visible { outline: var(--focus-ring); outline-offset: 2px; }'));
  });
  it('gives a button, a segmented option and an icon button 44px on a phone layout (H4)', () => {
    expect(css).toContain('.d-app-shell[data-layout="phone"] :is(.d-btn, .d-segmented__option, .d-icon-button):not(.d-phone-header__actions *, .d-channel-list__head-btn, .d-user-panel *) { min-height: 2.75rem; min-width: 2.75rem; }');
  });
  it('adds no @media of its own for it (the phone-query count stays with phone-query.test.ts)', () => {
    expect(css.split('(max-width: 47.99rem), (pointer: coarse)').length - 1).toBe(1);
  });
});

