import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import type { Page } from '@playwright/test';
import { CONVERSATION_PROTOTYPE_CONTRAST } from '../../../packages/ui/scripts/prototype-contrast.ts';

export interface AxeFinding { id: string; impact: 'serious' | 'critical'; help: string; targets: string[] }

let source: string | null = null;
function axeSource(): string {
  source ??= readFileSync(createRequire(import.meta.url).resolve('axe-core/axe.min.js'), 'utf8');
  return source;
}

type AxeWindow = Window & { axe?: { version: string; run(context: unknown, options: unknown): Promise<{ violations: { id: string; impact: string | null; help: string; nodes: { target: string[] }[] }[] }> } };

/** Not page.addScriptTag: an inline <script> is refused by the served CSP (L-HTTP-10) and reported as a
 *  securitypolicyviolation. A protocol-level evaluate is not subject to the page's CSP. */
export async function injectAxe(page: Page): Promise<void> {
  if (!(await page.evaluate(() => typeof (window as AxeWindow).axe !== 'undefined'))) {
    await page.evaluate(axeSource());
  }
}

/** Ruling Y (task 7): the conversation's prototype-exact colours under 4.5:1 leave colour-contrast in the signal theme only. */
const PROTOTYPE_CONTRAST = CONVERSATION_PROTOTYPE_CONTRAST.map(e => e.selector);

export async function runAxe(page: Page): Promise<AxeFinding[]> {
  await injectAxe(page);
  return page.evaluate(async (exempt) => {
    const result = await (window as AxeWindow).axe!.run(document, {
      resultTypes: ['violations'],
      rules: { 'target-size': { enabled: true } },
    });
    // The theme actually drawn: the attribute when set; without it (the system choice) the resolved --bg says whether it is signal's.
    const set = document.documentElement.dataset.theme;
    const theme = set ?? (getComputedStyle(document.documentElement).getPropertyValue('--bg').trim().toUpperCase() === '#08090B' ? 'signal' : 'other');
    const excepted = (v: { id: string }, n: { target: string[] }) => {
      if (v.id !== 'color-contrast' || theme !== 'signal' || n.target.length !== 1) return false;
      const el = document.querySelector(n.target[0]!);
      return el !== null && exempt.some(sel => el.matches(sel));
    };
    return result.violations
      .filter((v) => v.impact === 'serious' || v.impact === 'critical')
      .map((v) => ({ ...v, nodes: v.nodes.filter(n => !excepted(v, n)) }))
      .filter((v) => v.nodes.length > 0)
      .map((v) => ({ id: v.id, impact: v.impact as 'serious' | 'critical', help: v.help, targets: v.nodes.map((n) => n.target.join(' ')) }));
  }, PROTOTYPE_CONTRAST);
}
