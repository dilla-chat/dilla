// @vitest-environment node
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

// R9 (web3.9): tap targets in two tiers. Primary controls are 44 x 44 on a phone layout (a finger), secondary ones are
// at least 24 x 24 (WCAG 2.5.8). A CSS-text pin: the sizes live in stylesheets, and jsdom has no layout.
const SRC = fileURLToPath(new URL('..', import.meta.url));
const TOKENS = fileURLToPath(new URL('../../../design-tokens/dist/tokens.css', import.meta.url));
const strip = (css: string) => css.replace(/\/\*[\s\S]*?\*\//g, '');

function walk(dir: string, out: string[] = []): string[] {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) walk(p, out);
    else if (e.name.endsWith('.css')) out.push(p);
  }
  return out;
}
const base = strip(fs.readFileSync(path.join(SRC, 'styles', 'base.css'), 'utf8'));
const iconButton = strip(fs.readFileSync(path.join(SRC, 'IconButton', 'IconButton.css'), 'utf8'));
const tokens = strip(fs.readFileSync(TOKENS, 'utf8'));
const PHONE = '.d-app-shell[data-layout="phone"]';

/** A length in px: `2.75rem` -> 44, `24px` -> 24. */
function px(len: string): number {
  const m = /^([\d.]+)(px|rem)$/.exec(len.trim());
  if (!m) throw new Error(`not a px/rem length: ${len}`);
  return m[2] === 'rem' ? parseFloat(m[1]) * 16 : parseFloat(m[1]);
}
function decl(rule: string, prop: string): string {
  const m = new RegExp(`(?:^|[;{\\s])${prop}:\\s*([^;}]+)`).exec(rule);
  if (!m) throw new Error(`no ${prop} in: ${rule}`);
  return m[1];
}
function ruleFor(css: string, selector: string): string {
  const i = css.indexOf(`${selector} {`);
  if (i < 0) throw new Error(`no rule for ${selector}`);
  return css.slice(i, css.indexOf('}', i) + 1);
}

describe('R9: the primary tier is 44 x 44 on a phone layout', () => {
  it('gives .d-btn, .d-segmented__option and .d-icon-button a 44px minimum, both ways', () => {
    // Not inside the phone header's actions: those are the prototype's 28 high (Jonas 2026-10-27, PhoneHeader.css).
    const rule = ruleFor(base, `${PHONE} :is(.d-btn, .d-segmented__option, .d-icon-button):not(.d-phone-header__actions *, .d-channel-list__head-btn, .d-user-panel *)`);
    expect(px(decl(rule, 'min-height'))).toBeGreaterThanOrEqual(44);
    expect(px(decl(rule, 'min-width'))).toBeGreaterThanOrEqual(44);
  });

  it('the phone --row-h is at least 44px, and every row that takes its height from var(--row-h) inherits it', () => {
    const rule = ruleFor(base, `${PHONE}, [data-phone]`);
    expect(px(decl(rule, '--row-h'))).toBeGreaterThanOrEqual(44);
    expect(px(decl(rule, '--row-h-2'))).toBeGreaterThanOrEqual(44);
    // A row's height is its token: no stylesheet sets var(--row-h) on a height smaller than the token (a max-height
    // or a fixed px height on such a row would undo the phone value), and the desktop value is a token, not a literal.
    for (const file of walk(SRC)) {
      const css = strip(fs.readFileSync(file, 'utf8'));
      for (const m of css.matchAll(/([^{}]+)\{([^{}]*var\(--row-h\)[^{}]*)\}/g)) {
        expect(m[2], `${path.relative(SRC, file)}: ${m[1].trim()} caps a --row-h row`).not.toMatch(/(?:^|[;\s])max-height\s*:/);
      }
    }
  });

  it('the phone --row-h-sub is 36px, which is secondary (at least 24px)', () => {
    const rule = ruleFor(base, `${PHONE}, [data-phone]`);
    expect(px(decl(rule, '--row-h-sub'))).toBe(36);
    expect(px(decl(rule, '--row-h-sub'))).toBeGreaterThanOrEqual(24);
  });
});

describe('R9: the secondary tier is at least 24 x 24', () => {
  it('the smallest desktop icon button, size 28, is at least 24px each way', () => {
    const rule = ruleFor(iconButton, '.d-icon-button[data-size="28"]');
    expect(px(decl(rule, 'width'))).toBeGreaterThanOrEqual(24);
    expect(px(decl(rule, 'height'))).toBeGreaterThanOrEqual(24);
  });

  it('the desktop row tokens are at least 24px', () => {
    for (const name of ['--row-h', '--row-h-sub', '--row-h-2']) {
      expect(px(decl(tokens, name)), name).toBeGreaterThanOrEqual(24);
    }
    expect(px(decl(tokens, '--slot-w'))).toBeGreaterThanOrEqual(24);
  });
});
