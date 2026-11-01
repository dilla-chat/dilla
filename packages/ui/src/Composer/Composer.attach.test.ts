import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const css = readFileSync(join(dirname(fileURLToPath(import.meta.url)), 'Composer.css'), 'utf8');

// Task 7 (rule 0, Jonas 2026-10-27: phone sizes follow the prototype): the footer's tools are 26 x 24 at every width.
describe('the composer footer tools', () => {
  it('are the prototype\'s 26 x 24 buttons, with no phone enlargement', () => {
    expect(css).toMatch(/\.d-composer__tool \{[^}]*width: 1\.625rem[^}]*height: 1\.5rem/);
    expect(css).not.toMatch(/\[data-layout="phone"\] \.d-composer__(attach|gif|tool) \{/);
  });
});
