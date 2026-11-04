// @vitest-environment node
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

const read = (name: string) => readFileSync(fileURLToPath(new URL(name, import.meta.url)), 'utf8');
const PHONE = '@media (max-width: 47.99rem), (pointer: coarse) and (max-height: 31.99rem)';
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
const rule = (file: string, selector: string) => {
  const css = read(file).replace(/\/\*[\s\S]*?\*\//g, '');
  return [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)]
    .filter(match => match[1].trim() === selector).map(match => match[2]).join('\n');
};

// Jonas 2026-10-31: use each prototype context's own size, including its border, on phones.
// Native regressions live in Button.stories.tsx DesktopPrototypeHeights/PhonePrototypeHeights;
// UI unit CI runs before Chromium is installed, so these pins do not launch a browser.
describe('the prototype phone button heights (CSS pins)', () => {
  it('does not impose a global phone Button minimum', () => {
    expect(phoneBlock(read('./Button.css'))).not.toMatch(/\.d-btn\s*\{[^}]*min-height/);
    expect(read('../styles/base.css')).not.toMatch(/\.d-app-shell\[data-layout="phone"\]\s*:is\(\.d-btn[,)]/);
  });
  it.each([
    ['generic', './Button.css', '.d-btn', /height:\s*2rem\s*;/],
    ['small', './Button.css', '.d-btn[data-size="sm"]', /height:\s*1\.375rem\s*;/],
    ['confirm and poll actions', '../Dialog/Dialog.css', '.d-dialog__footer .d-btn', /height:\s*34px\s*;/],
    ['sign-in Continue', '../OnboardingFrame/OnboardingFrame.css', '.d-onboarding-frame__footer .d-btn[data-variant="accent"]', /height:\s*2\.5rem\s*;\s*min-height:\s*2\.5rem/],
    ['settings action', '../SettingsRow/SettingsRow.css', '.d-settings-row__control .d-btn', /min-height:\s*2rem\s*;/],
    ['invite Copy', '../InviteLink/InviteLink.css', '.d-invite-link__row .d-btn[data-variant="accent"]', /height:\s*2\.25rem\s*;/],
    ['recovery Save as file', '../RecoveryKey/RecoveryKey.css', '.d-recovery-key .d-recovery-key__print', /height:\s*34px\s*;\s*min-height:\s*34px/],
    ['invite Revoke', '../ManageList/ManageList.css', '.d-manage-list__trail > .d-btn', /height:\s*1\.875rem\s*;/],
  ])('%s retains its prototype height', (_name, file, selector, height) => {
    expect(rule(file, selector)).toMatch(height);
  });
  it('does not erase the settings action minimum with a phone wrapper', () => {
    expect(rule('../SettingsFrame/SettingsFrame.css', '.d-settings-frame[data-layout="phone"] .d-btn')).not.toMatch(/min-height:\s*0/);
  });
  it('retains the switch’s separate 2.75rem phone minimum', () => {
    expect(phoneBlock(read('../Toggle/Toggle.css'))).toMatch(/\.d-toggle__switch[^{]*\{[^}]*min-height: 2\.75rem/);
  });
});
