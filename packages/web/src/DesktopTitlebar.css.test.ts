import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const css = readFileSync(join(dirname(fileURLToPath(import.meta.url)), 'DesktopTitlebar.css'), 'utf8');
const rules = [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => ({ selector: m[1].trim(), body: m[2] }));
const region = (selector: string) =>
  rules.find((r) => r.selector === selector)?.body.match(/(?<!-webkit-)app-region:\s*([\w-]+)/)?.[1];

describe('desktop top bar app regions', () => {
  it('the empty top bar drags the window', () => {
    expect(region('html[data-desktop] .d-topbar')).toBe('drag');
  });

  it('every interactive element in the top bar stays no-drag', () => {
    const rule = rules.find((r) => r.selector.startsWith('html[data-desktop] .d-topbar :is('));
    expect(rule).toBeDefined();
    for (const sel of ['button', 'input', 'a', '[role="dialog"]', '[role="menu"]', '[role="listbox"]', '.d-topbar__voice', '.d-topbar__search']) {
      expect(rule!.selector).toContain(sel);
    }
    expect(rule!.body).toMatch(/(?<!-webkit-)app-region:\s*no-drag/);
  });
});
