// @vitest-environment node
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

const read = (rel: string) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), 'utf8');
const pane = read('./ThreadPane.css');
const row = read('../MessageRow/MessageRow.css');

describe('thread pane CSS (web3.7)', () => {
  it('pads the sheet\'s composer by the home indicator', () => {
    expect(pane).toMatch(/\.d-thread-pane\[data-phone\] \.d-thread-pane__composer \{[^}]*padding-bottom: env\(safe-area-inset-bottom\)/);
  });
  it('makes the sheet\'s buttons, attach control and thread chip 44px', () => {
    expect(pane).toMatch(/\[data-phone\] \.d-btn\[data-size\][^{]*\{[^}]*min-height: 2\.75rem/s);
    expect(pane).toMatch(/\[data-phone\] \.d-message-row__thread \{[^}]*min-height: 2\.75rem/s);
  });
  it('lets the column shrink to the aside', () => {
    expect(pane).toMatch(/\.d-thread-pane \{[^}]*min-width: 0/);
  });
  // Task 7 (rule 0, Jonas 2026-10-27: phone sizes follow the prototype): the row's thread chip is the prototype's 24px everywhere.
  it('draws the row\'s thread chip at the prototype\'s 24px, with no narrow-shell enlargement', () => {
    expect(row).toMatch(/\.d-message-row__thread \{[^}]*height: 1\.5rem/);
    expect(row).not.toMatch(/@container d-shell/);
  });
});
