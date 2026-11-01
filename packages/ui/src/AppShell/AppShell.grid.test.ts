// @vitest-environment node
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

const read = (rel: string) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), 'utf8');
const shell = read('./AppShell.css');
const base = read('../styles/base.css');

// Signal (README §5.1): the top bar's row spans every column; the rail runs the full height and the bottom-left panel (web3.8,
// R23) sits under the sidebar alone, as in the prototype; main spans both rows. The status row is gone with the status bar.
describe('AppShell grid areas', () => {
  it('the desktop grid has the top bar row over every column, then the banner, then rail | sidebar | main over the panel', () => {
    expect(shell).toMatch(/\.d-app-shell__grid \{[^}]*grid-template-columns: var\(--rail-w\) var\(--d-sidebar-w, var\(--sidebar-w\)\) minmax\(0, 1fr\);/);
    expect(shell).toMatch(/\.d-app-shell__grid \{[^}]*grid-template-rows: auto auto minmax\(0, 1fr\) auto;[^}]*grid-template-areas: "topbar topbar topbar" "banner banner banner" "rail sidebar main" "rail panel main";/);
    expect(shell).toMatch(/\.d-app-shell__topbar \{ grid-area: topbar;/);
    expect(shell).not.toMatch(/status/);
  });
  it('the panel wrapper is the panel area', () => {
    expect(shell).toMatch(/\.d-app-shell__panel \{[^}]*grid-area: panel;/);
  });
  it('the aside is the fourth (auto) column and sets its own width: 232 (members) or 340 (thread) unless dragged', () => {
    expect(base).toMatch(/\.d-app-shell__grid--aside \{[^}]*minmax\(0, 1fr\) auto;[^}]*grid-template-areas: "topbar topbar topbar topbar" "banner banner banner banner" "rail sidebar main aside" "rail panel main aside";/);
    expect(base).toMatch(/\.d-app-shell \.d-app-shell__aside \{[^}]*--d-aside-open: var\(--d-aside-w, var\(--members-w\)\);[^}]*width: var\(--d-aside-open\);/);
    expect(base).toMatch(/\[data-aside="thread"\] > \.d-app-shell__aside \{ --d-aside-open: var\(--d-aside-w, var\(--thread-w\)\); \}/);
    expect(base).not.toMatch(/d-app-shell__status/);
  });
  it('the column aside animates its width over --duration-aside with --ease-out; its body keeps the open width', () => {
    expect(base).toMatch(/\.d-app-shell \.d-app-shell__aside \{[^}]*transition: width var\(--duration-aside\) var\(--ease-out\)/);
    expect(base).toMatch(/\.d-app-shell__aside\[data-open="false"\] \{[^}]*width: 0; border-left-width: 0; visibility: hidden;[^}]*visibility 0s linear var\(--duration-aside\);/);
    expect(base).toMatch(/@starting-style \{ \.d-app-shell \.d-app-shell__aside\[data-open="true"\] \{ width: 0; border-left-width: 0; \} \}/);
    expect(base).toMatch(/\.d-app-shell__aside-body > \.d-app-shell__aside-scroll \{[^}]*width: calc\(var\(--d-aside-open\) - 1px\);/);
    // Task 6 fix round 3: nothing in the aside clips by clip-path, which would cut its fixed-position popovers (the profile card).
    expect(base).not.toMatch(/d-app-shell__aside[^{]*\{[^}]*clip-path/);
    expect(base).toMatch(/\.d-app-shell__aside\[data-open\] > \.d-app-shell__aside-body \{[^}]*overflow: hidden;/);
    expect(base).toContain('.d-app-shell[data-resizing] .d-app-shell__aside { transition: none; }');
  });
  it('the aside is drawn on bg-2', () => {
    expect(shell).toMatch(/\.d-app-shell__aside \{[^}]*background: var\(--bg-2\);/);
  });
  it('a phone-width shell hides the top bar, in the container query and its phone-layout twin', () => {
    expect(base).toMatch(/@container d-shell \(max-width: 47\.99rem\) \{[^@]*\.d-app-shell \.d-app-shell__topbar \{ display: none; \}/);
    expect(base).toContain('.d-app-shell[data-layout="phone"] .d-app-shell__topbar { display: none; }');
  });
});
