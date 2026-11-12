// Renders the fixture the way beta.5 (Electron/Chromium) draws it: Playwright Chromium, reduced motion, fonts
// awaited, like docs/design/signal/parity/shoot.mjs. Usage: node render-reference.mjs <outDir> <os>
// Writes <outDir>/<os>-beta5-1x.png and -2x.png (CSS px 1280x800, deviceScaleFactor 1 and 2).
import { chromium } from 'playwright';
import { readFileSync, mkdirSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const fx = JSON.parse(readFileSync(join(here, 'fixture.json'), 'utf8'));
const t = fx.tokens;
const l = fx.layout;
const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

const rows = fx.messages
  .map((m) => {
    const a = fx.authors[m.author];
    const lines = m.lines.map((x) => `<div class="line">${esc(x)}</div>`).join('');
    const code = m.code?.length
      ? `<div class="code">${m.code.map((x) => `<div class="cl">${esc(x)}</div>`).join('')}</div>`
      : '';
    const reacts = m.reactions?.length
      ? `<div class="reacts">${m.reactions.map(([k, c]) => `<div class="pill"><span>${esc(k)} ${esc(c)}</span></div>`).join('')}</div>`
      : '';
    return `<div class="row"><div class="avatar">${esc(a.initials)}</div><div class="col"><div class="head"><div class="name">${esc(a.name)}</div><div class="time">${esc(m.time)}</div></div>${lines}${code}${reacts}</div></div>`;
  })
  .join('');

const fontUrl = 'data:font/ttf;base64,' + readFileSync(join(here, 'fonts', 'JetBrainsMono-Variable-latin.ttf')).toString('base64');
const html = `<!doctype html><html><head><meta charset="utf-8"><style>
@font-face { font-family: "${fx.font.family}"; src: url("${fontUrl}") format("truetype"); font-weight: 100 800; }
* { box-sizing: border-box; margin: 0; padding: 0; }
html, body { width: ${fx.width}px; height: ${fx.height}px; background: ${t.bg}; overflow: hidden; }
body { font-family: "${fx.font.family}"; padding: ${l.outerPad}px; }
.panel { width: 100%; height: 100%; overflow: hidden; background: ${t.surface}; border: 1px solid ${t.hairline2}; border-radius: ${fx.radius}px; padding: ${l.panelPad}px; }
.feed { display: flex; flex-direction: column; }
.row { display: flex; gap: ${l.rowGap}px; padding: ${l.rowPadY}px 0; }
.avatar { flex: none; width: ${l.avatar}px; height: ${l.avatar}px; display: flex; align-items: center; justify-content: center; border-radius: 999px; background: ${t.surfaceHi}; border: 1px solid ${t.hairline2}; font-size: ${fx.font.small}px; color: ${t.fg2}; }
.col { display: flex; flex-direction: column; }
.head { display: flex; align-items: center; gap: ${l.headGap}px; height: ${l.lineH}px; }
.name { height: ${l.lineH}px; line-height: ${l.lineH}px; font-size: ${fx.font.base}px; font-weight: 600; color: ${t.fg}; }
.time { height: ${l.lineH}px; line-height: ${l.lineH}px; font-size: ${fx.font.small}px; color: ${t.fg4}; }
.line { height: ${l.lineH}px; line-height: ${l.lineH}px; font-size: ${fx.font.base}px; color: ${t.fgBody}; white-space: pre; }
.code { margin-top: ${l.codeMarginTop}px; width: ${l.codeW}px; padding: ${l.codePadY}px ${l.codePadX}px; background: ${t.bg}; border: 1px solid ${t.hairline2}; border-radius: ${fx.radius}px; display: flex; flex-direction: column; }
.cl { height: ${l.codeLineH}px; line-height: ${l.codeLineH}px; font-size: ${fx.font.code}px; color: ${t.fgBody}; white-space: pre; }
.reacts { margin-top: ${l.reactMarginTop}px; display: flex; gap: ${l.reactGap}px; }
.pill { height: ${l.pillH}px; padding: 0 ${l.pillPadX}px; display: flex; align-items: center; background: ${t.surface2}; border: 1px solid ${t.controlEdge}; border-radius: 999px; }
.pill span { line-height: ${l.pillH - 2}px; font-size: ${fx.font.pill}px; color: ${t.fg2}; white-space: pre; }
</style></head><body><div class="panel"><div class="feed">${rows}</div></div></body></html>`;

const [outDir, os, variant] = [resolve(process.argv[2] ?? 'out'), process.argv[3] ?? 'linux', process.argv[4] ?? 'default'];
// variant 'nohint': Chromium without glyph hinting (fractional advances), to separate hinting from real layout error.
const launchArgs = variant === 'nohint' ? ['--font-render-hinting=none', '--enable-font-subpixel-positioning'] : [];
mkdirSync(outDir, { recursive: true });
const browser = await chromium.launch({ args: launchArgs });
for (const scale of [1, 2]) {
  const ctx = await browser.newContext({
    viewport: { width: fx.width, height: fx.height },
    deviceScaleFactor: scale,
    reducedMotion: 'reduce',
  });
  const page = await ctx.newPage();
  await page.setContent(html);
  await page.evaluate(() => document.fonts.ready);
  await page.evaluate(() => document.fonts.load('600 13.5px "JetBrains Mono"'));
  await page.waitForTimeout(300);
  const loaded = await page.evaluate(() => [...document.fonts].map((f) => `${f.family}:${f.status}`));
  if (!loaded.some((s) => s.endsWith(':loaded'))) throw new Error(`font not loaded: ${loaded}`);
  const file = join(outDir, `${os}-beta5${variant === 'nohint' ? '-nohint' : ''}-${scale}x.png`);
  await page.screenshot({ path: file });
  console.log('rendered', file, loaded.join(','));
  await ctx.close();
}
await browser.close();
