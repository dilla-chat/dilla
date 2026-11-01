import { describe, it, expect } from 'vitest';
import { renderCss, kebab } from './css.ts';

describe('kebab', () => {
  it('maps token keys to custom property names', () => {
    expect(kebab('bg2')).toBe('bg-2');
    expect(kebab('surfaceHi')).toBe('surface-hi');
    expect(kebab('accentInk')).toBe('accent-ink');
    expect(kebab('fg')).toBe('fg');
  });
});

describe('renderCss', () => {
  const css = renderCss();
  it('sets the signal theme on :root and the others on data-theme', () => {
    expect(css).toMatch(/:root\s*{[^}]*--bg:\s*#08090B;/);
    expect(css).toMatch(/\[data-theme="signal"\]\s*{[^}]*--bg:\s*#08090B;/);
    expect(css).toMatch(/\[data-theme="light"\]\s*{[^}]*--bg:\s*#F5F6F5;/);
    expect(css).toMatch(/\[data-theme="high-contrast"\]\s*{[^}]*--link-underline:\s*1;/);
    expect(css.toLowerCase()).not.toContain('mesh');
  });
  it('emits --knob: white on signal and light, black on high-contrast (the toggle knob)', () => {
    expect(css).toMatch(/\[data-theme="signal"\]\s*{[^}]*--knob:\s*#FFFFFF;/);
    expect(css).toMatch(/\[data-theme="light"\]\s*{[^}]*--knob:\s*#FFFFFF;/);
    expect(css).toMatch(/\[data-theme="high-contrast"\]\s*{[^}]*--knob:\s*#000000;/);
  });
  it('emits the prototype values the scale lacked (ruling K)', () => {
    expect(css).toMatch(/\[data-theme="signal"\]\s*{[^}]*--track-off:\s*#2A303A;/);
    expect(css).toMatch(/\[data-theme="signal"\]\s*{[^}]*--accent-chip:\s*rgba\(90,168,230,0\.16\);/);
    expect(css).toContain('--shadow-knob: 0 1px 3px rgba(0,0,0,0.4);');
    expect(css).toContain('--shadow-1:');
    expect(css).toContain('--duration-toggle: 240ms;');
    expect(css).toMatch(/--text-btn: 0\.71875rem;/);
    expect(css).toMatch(/--text-input: 0\.8125rem;/);
    expect(css).toMatch(/--text-chip: 0\.59375rem;/);
    expect(css).toMatch(/--text-tag: 0\.53125rem;/);
    expect(css).toContain('--tracking-soft: 0.04em;');
  });
  it('emits the reminders popover values (task 6b fix round 1)', () => {
    expect(css).toMatch(/\[data-theme="signal"\]\s*{[^}]*--warn-tint:\s*rgba\(255,209,106,0\.14\);/);
    expect(css).toMatch(/\[data-theme="signal"\]\s*{[^}]*--fg-chip:\s*#C9CED6;/);
    expect(css).toContain('--shadow-popover: 0 20px 50px rgba(0,0,0,0.6);');
    expect(css).toContain('--duration-popover: 200ms;');
    expect(css).toMatch(/\[data-motion="reduced"\]\s*{[^}]*--duration-popover: 0ms;/);
  });
  // Task 9
  it('emits the overlay values (task 9)', () => {
    expect(css).toContain('--shadow-profile: 0 24px 60px rgba(0,0,0,0.6);');
    expect(css).toContain('--shadow-status: 0 16px 40px rgba(0,0,0,0.55);');
    expect(css).toMatch(/--text-card: 0\.9375rem;/);
    expect(css).toContain('--duration-scrim: 160ms;');
    expect(css).toContain('--duration-dialog: 240ms;');
    expect(css).toContain('--duration-profile: 260ms;');
    for (const k of ['scrim', 'dialog', 'profile']) expect(css).toMatch(new RegExp(`\\[data-motion="reduced"\\]\\s*{[^}]*--duration-${k}: 0ms;`));
  });
  it('emits the control edges (task 6b, prototype parity in signal)', () => {
    expect(css).toMatch(/\[data-theme="signal"\]\s*{[^}]*--control-edge:\s*#2A303A;/);
    expect(css).toMatch(/\[data-theme="signal"\]\s*{[^}]*--control-edge-field:\s*#252A33;/);
    expect(css).toMatch(/\[data-theme="signal"\]\s*{[^}]*--control-edge-key:\s*#3A404A;/);
    expect(css).toMatch(/\[data-theme="light"\]\s*{[^}]*--control-edge:\s*#6F776F;/);
    expect(css).toMatch(/\[data-theme="high-contrast"\]\s*{[^}]*--control-edge-field:\s*#FFFFFF;/);
  });
  it('emits the top bar values (task 5, rulings K and L)', () => {
    expect(css).toMatch(/\[data-theme="signal"\]\s*{[^}]*--line-strong:\s*#2A303A;/);
    expect(css).toMatch(/\[data-theme="light"\]\s*{[^}]*--line-strong:\s*#B4BBB4;/);
    expect(css).toMatch(/\[data-theme="high-contrast"\]\s*{[^}]*--line-strong:\s*#FFFFFF;/);
    expect(css).toMatch(/\[data-theme="signal"\]\s*{[^}]*--brand-ink:\s*#FFFFFF;/);
    expect(css).toMatch(/--text-ui: 0\.75rem;/);
    expect(css).toMatch(/--text-badge: 0\.5625rem;/);
    expect(css).toContain('--duration-pop: 400ms;');
    expect(css).toContain('--duration-turn: 200ms;');
    expect(css).toContain('--duration-aside: 280ms;');
    expect(css).toContain('--duration-bar: 600ms;');
    expect(css).toMatch(/\[data-motion="reduced"\]\s*{[^}]*--duration-aside: 0ms;[^}]*--duration-bar: 0ms;/);
    expect(css).toMatch(/prefers-reduced-motion: reduce[\s\S]*--duration-aside: 0ms;[\s\S]*--duration-bar: 0ms;/);
    expect(css).toMatch(/prefers-reduced-motion: reduce[\s\S]*--duration-pop: 0ms;[\s\S]*--duration-turn: 0ms;/);
    expect(css).toMatch(/\[data-motion="reduced"\]\s*{[^}]*--duration-pop: 0ms;[^}]*--duration-turn: 0ms;/);
  });
  // Task 10
  it('emits the settings and sign-in values (task 10, the prototype literals)', () => {
    const root = css.slice(0, css.indexOf('}'));
    for (const decl of ['--accent-tag: rgba(90,168,230,0.14);', '--danger-edge: rgba(255,110,110,0.4);', '--danger-faint: rgba(255,110,110,0.08);',
      '--accent-ring: rgba(90,168,230,0.15);', '--brand-glow: rgba(0,87,153,0.28);', '--accent-pick: rgba(90,168,230,0.06);',
      '--text-heading: 1.25rem;', '--shadow-card: 0 30px 80px rgba(0,0,0,0.55);', '--duration-modal: 260ms;', '--duration-card: 420ms;', '--duration-step: 360ms;']) {
      expect(root).toContain(decl);
    }
  });
  // Task 10b
  it('emits the sensitivity meter values (task 10b, the prototype literals); the segment fade is never zeroed', () => {
    const root = css.slice(0, css.indexOf('}'));
    for (const decl of ['--meter-off: #161A20;', '--ok-line: rgba(124,255,142,0.3);', '--warn-faint: rgba(255,209,106,0.08);',
      '--warn-line: rgba(255,209,106,0.3);', '--duration-meter-segment: 70ms;']) expect(root).toContain(decl);
    expect(css).not.toContain('--duration-meter-segment: 0ms;');
  });
  it('zeroes --duration-toggle under reduced motion', () => {
    expect(css).toMatch(/prefers-reduced-motion: reduce[\s\S]*--duration-toggle: 0ms;/);
  });
  it('follows the OS light setting only when no theme is chosen', () => {
    expect(css).toMatch(/@media \(prefers-color-scheme: light\)\s*{\s*:root:not\(\[data-theme\]\)\s*{[^}]*--bg:\s*#F5F6F5;[^}]*--accent:\s*#005799;/);
  });
  it('follows the OS contrast preference only when no theme is chosen', () => {
    expect(css).toMatch(/@media \(prefers-contrast: more\)\s*{\s*:root:not\(\[data-theme\]\)\s*{[^}]*--bg:\s*#000000;[^}]*--accent:\s*#9CCBFF;/);
    // and it comes after the colour-scheme block, so a user who asks for
    // more contrast gets it even when the OS is set to light.
    expect(css.indexOf('@media (prefers-contrast: more)')).toBeGreaterThan(css.indexOf('@media (prefers-color-scheme: light)'));
  });
  it('emits densities', () => {
    expect(css).toMatch(/\[data-density="compact"\]\s*{[^}]*--avatar-size:\s*1\.75rem;/);
    expect(css).toMatch(/:root\s*{[^}]*--row-pad-y:\s*0\.375rem;/);
  });
  it('zeroes every duration but the meter, in the media query and under data-motion', () => {
    const zeroed = ['press', 'fast', 'normal', 'slow', 'enter', 'badge', 'toast', 'pulse', 'caret', 'flash', 'hover', 'tab', 'spin', 'dock', 'rail-badge'];
    const blocks = [
      css.match(/@media \(prefers-reduced-motion: reduce\)\s*{\s*:root\s*{([^}]*)}/)?.[1],
      css.match(/\[data-motion="reduced"\]\s*{([^}]*)}/)?.[1],
    ];
    for (const body of blocks) {
      expect(body).toBeDefined();
      for (const k of zeroed) expect(body).toContain(`--duration-${k}: 0ms;`);
      expect(body).not.toContain('meter');
    }
    expect(blocks[0]!.replace(/\s+/g, '')).toBe(blocks[1]!.replace(/\s+/g, ''));
  });
  it('emits the new durations, shadow and tracking', () => {
    expect(css).toContain('--duration-press: 120ms;');
    expect(css).toContain('--duration-enter: 380ms;');
    expect(css).toContain('--duration-badge: 450ms;');
    expect(css).toContain('--duration-meter: 120ms;');
    // Task 6 (Ruling K): the sidebar's own durations, read off the prototype.
    expect(css).toContain('--duration-hover: 140ms;');
    expect(css).toContain('--duration-tab: 160ms;');
    expect(css).toContain('--duration-spin: 180ms;');
    expect(css).toContain('--duration-dock: 340ms;');
    expect(css).toContain('--duration-rail-badge: 500ms;');
    expect(css).toContain('--text-meta: 0.65625rem;');
    expect(css).toContain('--text-title: 1.1875rem;');
    expect(css).toContain('--text-mini: 0.46875rem;');
    expect(css).toContain('--r-row: 5px;');
    expect(css).toContain('--r-tag: 3px;'); // every prototype tag (BOT, LIVE, playback only): task 6b
    expect(css).toContain('--hover-hi: #1A1E25;');
    expect(css).toContain('--shadow-3: 0 30px 80px rgba(0,0,0,0.65);');
    expect(css).toContain('--label-tracking-wide: 0.12em;');
    expect(css).toContain('--accent: #5AA8E6;');
    expect(css).toContain('--ok-text: #9EF5AC;');
  });
  it('emits structural tokens', () => {
    expect(css).toContain('--r-md: 6px;');
    expect(css).toContain('--r-xl: 10px;');
    expect(css).toContain('--r-bot: 6px;');
    expect(css).toContain('--rail-w: 56px;');
    expect(css).toContain('--panel-h: 4rem;');
    expect(css).toContain('--focus-ring: 2px solid var(--accent);');
    expect(css).toContain('--font-mono: "JetBrains Mono Variable"');
  });
  it('emits the type scale in rem, never px', () => {
    expect(css).toContain('--text-micro: 0.625rem;');
    expect(css).toContain('--text-base: 0.84375rem;');
    expect(css).toContain('--text-xl: 1.375rem;');
    expect(css).not.toMatch(/--text-[a-z]+:\s*[\d.]+px;/);
  });
  it('emits the boxes around that text in rem, and the resizable panes in px', () => {
    expect(css).toContain('--topbar-h: 2.75rem;');
    expect(css).toContain('--bottombar-h: 1.625rem;');
    expect(css).toContain('--channel-header-h: 2.75rem;');
    expect(css).toContain('--bar-h: 2.75rem;');
    expect(css).toContain('--row-h: 1.75rem;');
    expect(css).toContain('--row-h-sub: 1.625rem;');
    expect(css).toContain('--row-h-2: 2.75rem;');
    expect(css).toContain('--list-gap: 0.0625rem;');
    expect(css).toContain('--rail-tile: 38px;');
    expect(css).toContain('--slot-w: 32px;');
    expect(css).toMatch(/\[data-density="cozy"\]\s*{[^}]*--avatar-size:\s*2\.25rem;/);
    expect(css).toContain('--sidebar-w: 244px;');
    expect(css).toContain('--thread-w: 340px;');
    expect(css).not.toMatch(/--(topbar-h|bottombar-h|channel-header-h|row-pad-[xy]|row-gap|group-gap|avatar-size):\s*[\d.]+px;/);
  });
});

import { readFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
it('committed dist/tokens.css equals renderCss()', () => {
  const dist = join(dirname(fileURLToPath(import.meta.url)), '..', 'dist', 'tokens.css');
  expect(readFileSync(dist, 'utf8')).toBe(renderCss());
});
