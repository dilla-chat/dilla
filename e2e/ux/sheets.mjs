// Contact sheets: one PNG per width and theme, a grid of that width's shots with each one's screen name under it.
//   node e2e/ux/sheets.mjs [--only d1440-signal,p390-light] [--cols 4]
// Reads target/ui-screens/web3.9/<d|p><width>-<theme>-<screen>.png, writes target/ui-screens/web3.9/sheets/<d|p><width>-<theme>.png.
import { mkdirSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';

const REPO = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const OUT = path.join(REPO, 'target', 'ui-screens', 'web3.9');
const SHEETS = path.join(OUT, 'sheets');
const require = createRequire(path.join(REPO, 'e2e', 'package.json'));
const { chromium } = require('@playwright/test');

const arg = (name) => { const i = process.argv.indexOf(`--${name}`); return i === -1 ? undefined : process.argv[i + 1]; };
const only = arg('only')?.split(',');
const NAME = /^([dp]\d+-(?:signal|light|high-contrast))-(.+)\.png$/;

const groups = new Map();
for (const f of readdirSync(OUT).sort()) {
  const m = NAME.exec(f);
  if (m === null) continue;
  if (only !== undefined && !only.includes(m[1])) continue;
  groups.set(m[1], [...(groups.get(m[1]) ?? []), { file: f, screen: m[2] }]);
}
if (groups.size === 0) { console.error(`no shots in ${OUT}: run npm run ux:shots -w @dilla/e2e first`); process.exit(1); }

mkdirSync(SHEETS, { recursive: true });
const browser = await chromium.launch({ headless: true });
try {
  for (const [tag, shots] of groups) {
    const phone = tag.startsWith('p');
    const cols = Number(arg('cols') ?? (phone ? 6 : 3));
    const cell = phone ? 230 : 560;
    const html = `<!doctype html><meta charset="utf-8"><title>${tag}</title>
<style>
  body { margin: 0; padding: 16px; background: #111; color: #ddd; font: 12px/1.3 ui-monospace, monospace; }
  h1 { font-size: 14px; margin: 0 0 12px; }
  .grid { display: grid; grid-template-columns: repeat(${cols}, ${cell}px); gap: 16px 12px; align-items: start; }
  figure { margin: 0; } img { display: block; width: ${cell}px; height: auto; outline: 1px solid #333; }
  figcaption { margin-top: 4px; overflow-wrap: anywhere; }
</style>
<h1>${tag} (${shots.length} shots)</h1>
<div class="grid">${shots.map((s) => `<figure><img src="${pathToFileURL(path.join(OUT, s.file)).href}" alt=""><figcaption>${s.screen}</figcaption></figure>`).join('')}</div>`;
    const page = await browser.newPage({ viewport: { width: cols * (cell + 12) + 32, height: 900 } });
    const html1 = path.join(SHEETS, `${tag}.html`);
    writeFileSync(html1, html);
    await page.goto(pathToFileURL(html1).href);
    await page.evaluate(() => Promise.all([...document.images].map((i) => i.decode().catch(() => {}))));
    await page.screenshot({ path: path.join(SHEETS, `${tag}.png`), fullPage: true });
    await page.close();
    rmSync(html1);
    console.log('sheet', tag, shots.length);
  }
} finally { await browser.close(); }
