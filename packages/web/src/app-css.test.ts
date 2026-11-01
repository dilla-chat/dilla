// The shell's layout rules that jsdom cannot lay out (product walk I4, I5; WB a11y m4 ruling): read from app.css, as
// SettingsFrame.test reads its container query. The voice e2e and the docs shoot check the same in a real browser.
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const css = readFileSync(join(dirname(fileURLToPath(import.meta.url)), 'app.css'), 'utf8');
/** The body of the shell's phone container query (`@container d-shell (max-width: 47.99rem)`). */
const phone = /@container d-shell \(max-width: 47\.99rem\) \{([\s\S]*?)\n\}/.exec(css)?.[1] ?? '';
const rule = (block: string, selector: string) => new RegExp(`(^|\\n)\\s*${selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\s*\\{([^}]*)\\}`).exec(block)?.[2] ?? '';

describe('the phone layout of voice (360 px)', () => {
  it('web3.8 FX m1: no rule for a dock inside the sidebar is left (WS6 moved the dock to the shell\'s bottom area)', () => {
    expect(phone).not.toBe('');
    expect(css).not.toMatch(/\.dw-sidebar[^{,]*\.d[w]?-voice-dock/);
    expect(css).not.toMatch(/:not\(\[data-layout="phone"\]\) \.dw-sidebar/);
  });

  it('web3.8 FX: the compact phone grid is the DM call\'s only; the voice stage keeps its own phone tiles', () => {
    expect(rule(phone, '.dw-voice-stage .d-call-stage__tiles:not([data-tiles="stage"])')).toMatch(/grid-template-columns/);
    expect(rule(phone, '.dw-voice-stage .d-call-tile:not([data-stage])')).toMatch(/padding: 0\.5rem/);
    expect(css).not.toMatch(/\.dw-voice-stage \.d-call-stage__tiles \{/);
    expect(css).not.toMatch(/\.dw-voice-stage \.d-call-tile \{/);
  });

  it('Signal §5.6 (rule 0, the prototype’s sizes): the voice preview\'s join is the prototype\'s 40px on a phone too; the stage keeps its 14px gutter', () => {
    const stage = readFileSync(join(dirname(fileURLToPath(import.meta.url)), '../../ui/src/CallStage/CallStage.css'), 'utf8');
    expect(stage).toMatch(/\.d-call-stage__join \{[^}]*height: 2\.5rem;/);
    expect(stage).toMatch(/\.d-call-stage \{[^}]*--d-stage-pad: 0\.875rem;/);
  });

  it('H1/H2: no footer rule is left for the phone\'s DM list, and no bare tab heading', () => {
    expect(css).not.toMatch(/\.dw-phone-footer/);
    expect(css).not.toMatch(/\.dw-phone-heading/);
  });

  it('I5: no stage toolbar floats over the tiles: the controls are in normal flow', () => {
    expect(rule(phone, '.dw-voice-stage .d-call-stage__controls:not([data-bar])')).toMatch(/position:\s*static/);
    expect(rule(phone, '.dw-dm-call .d-call-stage__controls')).toMatch(/position:\s*static/);
    expect(phone).not.toMatch(/d-call-stage__controls\s*\{[^}]*position:\s*(sticky|fixed|absolute)/);
  });
});

describe('the desktop stage toolbar is pinned (web3.8, VOICE-W)', () => {
  const stage = '.d-app-shell:not([data-layout="phone"]) .dw-voice-stage:not(.dw-dm-call .dw-voice-stage)';
  it('fills the stage and sticks the toolbar to its bottom', () => {
    // web3.8 WS7: the stage is as tall as its pane (not a minimum), so the tiles fill what the toolbar leaves.
    expect(rule(css, `${stage} .d-call-stage`)).toMatch(/height:\s*100%;\s*min-height:\s*0;\s*box-sizing:\s*border-box/);
    expect(rule(css, `${stage}`)).toMatch(/overflow:\s*hidden/);
    expect(rule(css, `${stage} .d-call-stage__tiles[data-tiles="stage"]`)).toMatch(/flex:\s*1 1 auto;\s*min-height:\s*0/);
    expect(rule(css, '.dw-voice-stage')).toMatch(/height:\s*100%/);
    expect(rule(css, `${stage} .d-call-stage__controls:not([data-bar])`)).toMatch(/position:\s*sticky;\s*bottom:\s*0;\s*margin-top:\s*auto;\s*padding-block:\s*0\.5rem;\s*background:\s*var\(--bg\)/);
  });
});

describe('a focused tile control clears the pinned toolbar (WCAG 2.4.11)', () => {
  it('the stage scrolls a focused control above the toolbar', () => {
    expect(rule(css, '.dw-voice-stage')).toMatch(/scroll-padding-bottom:\s*4\.5rem/);
  });
});

describe('the one channel list (web3.8 item 12)', () => {
  it('has no voice-section rules left, and the retired text join and the old "+" have none either (web3.9: both are icon buttons, 44 px by base.css)', () => {
    expect(css).not.toContain('dw-voice-section');
    expect(css).not.toContain('dw-new-channel');
    expect(css).not.toContain('dw-channel-add');
    expect(css).not.toContain('d-channel-list__join');
    expect(css).toMatch(/\[data-layout="phone"\] \.dw-call-strip \.d-btn \{ min-height: 2\.75rem; \}/);
    expect(css).toMatch(/@container d-shell \(max-width: 47\.99rem\) \{\s*\.dw-call-strip \.d-btn \{ min-height: 2\.75rem; \}/);
  });
});

describe('a long chosen name never widens the page (product walk I2)', () => {
  it('the direct-message picker wraps a long name inside its row and keeps the open button whole', () => {
    expect(rule(css, '.dw-dm-picker__name')).toMatch(/min-width:\s*0/);
    expect(rule(css, '.dw-dm-picker__name')).toMatch(/overflow-wrap:\s*anywhere/);
    expect(rule(css, '.dw-dm-picker__row > .d-btn')).toMatch(/flex-shrink:\s*0/);
  });
});

describe('web3.9 WEB-AUTH: the auth column edges (L2, L3)', () => {
  it('L2: a ghost action inside the onboarding frame has no side padding, so its text sits on the column\'s x', () => {
    expect(rule(css, '.d-onboarding-frame__body .d-btn[data-variant="ghost"]')).toMatch(/padding-inline:\s*0/);
    expect(css).not.toMatch(/\.d-onboarding-frame \.d-btn\[data-variant="ghost"\]/);
    expect(css).not.toMatch(/__footer[^{]*data-variant="ghost"/);
  });

  it('L3: the setup key\'s Copy is a 36px suffix attached to the field, on one line', () => {
    const field = rule(css, '.dw-factor-key__field');
    expect(field).toMatch(/flex-wrap:\s*nowrap/);
    expect(field).toMatch(/gap:\s*0/);
    expect(rule(css, '.dw-factor-key__field > .d-btn')).toMatch(/height:\s*2\.25rem/);
    expect(rule(css, '.dw-factor-key__field > .d-btn')).toMatch(/border-radius:\s*0 var\(--r-md\) var\(--r-md\) 0/);
    expect(rule(css, '.dw-factor-key__field .d-text-field__input')).toMatch(/height:\s*2\.25rem/);
  });

  it('L3: the one checkbox is 24px and sits on the column\'s x (no UA margin)', () => {
    const box = rule(css, '.dw-factor-ack > input');
    expect(box).toMatch(/width:\s*24px;\s*height:\s*24px/);
    expect(box).toMatch(/margin:\s*0/);
    expect(rule(css, '.dw-factor-ack')).toMatch(/gap:\s*0\.5rem/);
  });

  it('M4: on a phone Copy wraps under the setup key, which is set tighter so it shows whole', () => {
    expect(css).toMatch(/@media \(max-width: 30rem\) \{\s*\.dw-factor-key__field \{ flex-wrap: wrap; \}/);
    expect(css).toMatch(/\.dw-factor-key__field \.d-text-field__input \{[^}]*letter-spacing: -0\.04em/);
  });
});
