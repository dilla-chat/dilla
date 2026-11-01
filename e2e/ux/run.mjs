// The audit's runner. As a module it exports `connect(who)`, which attaches to one of the world's browsers over CDP and
// returns the helpers every step uses; as a command it runs one step file against one browser:
//   node e2e/ux/run.mjs <who> <step.mjs> [args...]
// <who> is a key of state.json's `cdp` (ada, bjorn, mira, phone). The step file's default export is called with the helpers.
import path from 'node:path';
import { readFileSync, mkdirSync } from 'node:fs';
import { createRequire } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';

export const REPO = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
export const OUT_ROOT = path.join(REPO, 'target', 'ui-screens', 'web3.9');
export const STATE = path.join(OUT_ROOT, 'state.json');
const require = createRequire(path.join(REPO, 'e2e', 'package.json'));
const { chromium } = require('@playwright/test');
const { t } = await import(path.join(REPO, 'packages/web/src/strings/index.ts'));

export const readState = () => JSON.parse(readFileSync(STATE, 'utf8'));

/** Attaches to <who>'s browser; `out` is where its screenshots go (the audit folder by default). */
export async function connect(who, { out = OUT_ROOT } = {}) {
  mkdirSync(out, { recursive: true });
  const state = readState();
  if (state.cdp[who] === undefined) throw new Error(`no browser "${who}" in ${STATE}: is the world up (npm run ux:world -w @dilla/e2e)?`);
  const browser = await chromium.connectOverCDP(`http://127.0.0.1:${state.cdp[who]}`);
  const ctx = browser.contexts()[0];
  const page = ctx.pages().find((p) => p.url().startsWith('http')) ?? ctx.pages()[0];
  const byText = (role, key, vars) => page.getByRole(role, { name: t(key, vars), exact: true });
  /** Moves the pointer away, drops focus (unless asked to keep it), waits for the fonts and a beat of layout. */
  async function settle({ keepFocus = false } = {}) {
    await page.mouse.move(-10, -10).catch(() => {});
    await page.evaluate(async (keep) => {
      if (!keep && document.activeElement instanceof HTMLElement) document.activeElement.blur();
      await document.fonts.ready;
    }, keepFocus);
    await page.waitForTimeout(250);
  }
  async function shoot(name, opts = {}) { const f = path.join(out, `${name}.png`); await page.screenshot({ path: f, ...opts }); console.log('shot', name); return f; }
  /** signal | light | high-contrast: emulate the system preference the app follows. */
  async function theme(name) {
    await page.emulateMedia({ colorScheme: name === 'light' ? 'light' : 'dark', contrast: name === 'high-contrast' ? 'more' : 'no-preference' });
    await page.evaluate((th) => { document.documentElement.dataset.theme = th; }, name);
  }
  /** Rects (rounded to 0.1) and a few computed styles of every rendered element matching each selector. */
  async function measure(sels, styles = []) {
    return page.evaluate(({ sels, styles }) => {
      const r1 = (n) => Math.round(n * 10) / 10;
      const res = {};
      for (const s of sels) {
        res[s] = [...document.querySelectorAll(s)].filter((e) => e.getClientRects().length).map((e) => {
          const b = e.getBoundingClientRect(); const cs = getComputedStyle(e);
          const o = { x: r1(b.x), y: r1(b.y), w: r1(b.width), h: r1(b.height), cy: r1(b.y + b.height / 2), t: (e.getAttribute('aria-label') || e.textContent || '').trim().slice(0, 40) };
          for (const k of styles) o[k] = cs[k];
          return o;
        });
      }
      return res;
    }, { sels, styles });
  }
  return { page, ctx, browser, t, byText, settle, shoot, theme, measure, state, out, close: () => browser.close().catch(() => {}) /* disconnects only */ };
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [who, step, ...args] = process.argv.slice(2);
  if (who === undefined || step === undefined) { console.error('usage: node e2e/ux/run.mjs <who> <step.mjs> [args...]'); process.exit(2); }
  const h = await connect(who);
  try { await (await import(path.resolve(step))).default({ ...h, args }); } finally { await h.close(); }
}
