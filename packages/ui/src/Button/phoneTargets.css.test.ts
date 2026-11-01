// @vitest-environment node
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

const read = (name: string) => readFileSync(fileURLToPath(new URL(name, import.meta.url)), 'utf8');
/** The phone breakpoint of web/src/layout.ts: the page's media query, which reaches onboarding and sign-in outside the shell. */
const PHONE = '@media (max-width: 47.99rem), (pointer: coarse) and (max-height: 31.99rem)';
/** The body of the first phone media block of a stylesheet. */
const phoneBlock = (css: string): string => {
  const at = css.indexOf(PHONE);
  if (at < 0) return '';
  const open = css.indexOf('{', at);
  let depth = 0;
  for (let i = open; i < css.length; i += 1) {
    if (css[i] === '{') depth += 1;
    if (css[i] === '}' && (depth -= 1) === 0) return css.slice(open + 1, i);
  }
  return '';
};

// web3.9 AUDIT-2 FA1 (H4, N3): a primary control is 44 high on a phone page, in the shell or not.
// Task 10 (Jonas, 2026-10-27, pixel parity): Settings rows and the Voice page keep the prototype's sizes on a phone, so
// SettingsRow.css and VoiceSettings.css no longer carry this tier.
describe('the phone target tier of the primary controls (CSS pin)', () => {
  it.each([
    ['Button.css', '../Button/Button.css', /\.d-btn[^{]*\{[^}]*min-height: 2\.75rem/],
    ['Toggle.css (the switch)', '../Toggle/Toggle.css', /\.d-toggle__switch[^{]*\{[^}]*min-height: 2\.75rem/],
  ])('%s has a 2.75rem minimum under the phone media query', (_name, file, want) => {
    expect(phoneBlock(read(file))).toMatch(want);
  });
});
