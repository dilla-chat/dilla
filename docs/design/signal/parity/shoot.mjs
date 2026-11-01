#!/usr/bin/env node
// Design parity: shoots the Signal prototype and a Storybook story at the same viewport and writes the two frames
// and a side-by-side pair. Usage and the matching rule: see README.md.
import { chromium } from 'playwright';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
export const PROTOTYPE = path.resolve(HERE, '../prototype/Dilla Signal Prototype.dc.html');
export const DEFAULT_GLOBALS = 'theme:signal;motion:reduced';
const SETTLE_MS = 600;
export const CLIP_PAD = 8;
export const SCALE = 2;
const REPO = path.resolve(HERE, '../../../..');

/** A clip box grown by `pad` on every side, kept inside the page (x/y >= 0). Pure. */
export function padClip(box, pad = CLIP_PAD) {
  const x = Math.max(0, box.x - pad), y = Math.max(0, box.y - pad);
  return { x, y, width: box.width + (box.x - x) + pad, height: box.height + (box.y - y) + pad };
}

/**
 * CSS that stands in for the prototype's Google Fonts stylesheet: the same two families, from the repo's own font
 * files (inlined as data: URIs, since a https-served sheet may not point at file: URLs). `read` is injectable for tests.
 */
export function localFontCss(read = f => fs.readFileSync(path.join(REPO, 'node_modules', f)).toString('base64')) {
  const face = (family, style, weight, file) => `@font-face{font-family:'${family}';font-style:${style};font-weight:${weight};font-display:block;src:url(data:font/woff2;base64,${read(file)}) format('woff2')}`;
  return [
    face('JetBrains Mono', 'normal', '400 700', '@fontsource-variable/jetbrains-mono/files/jetbrains-mono-latin-wght-normal.woff2'),
    face('DM Serif Display', 'italic', '400', '@fontsource/dm-serif-display/files/dm-serif-display-latin-400-italic.woff2'),
  ].join('\n');
}

async function routeFonts(page) {
  const css = localFontCss();
  await page.route(/^https:\/\/fonts\.googleapis\.com\//, r => r.fulfill({ status: 200, contentType: 'text/css', body: css }));
  await page.route(/^https:\/\/fonts\.gstatic\.com\//, r => r.abort());
}

const isObject = v => typeof v === 'object' && v !== null && !Array.isArray(v);

/** Validates a parity spec (an array of pairs). Returns the pairs, or throws naming the first problem. */
export function validateSpec(spec) {
  if (!Array.isArray(spec) || spec.length === 0) throw new Error('spec must be a non-empty array of pairs');
  const names = new Set();
  spec.forEach((pair, i) => {
    const at = `pair ${i}`;
    if (!isObject(pair)) throw new Error(`${at}: must be an object`);
    if (typeof pair.name !== 'string' || !/^[A-Za-z0-9][\w.-]*$/.test(pair.name)) throw new Error(`${at}: name is required and must be a file stem (letters, digits, - _ .)`);
    if (names.has(pair.name)) throw new Error(`${at}: duplicate name ${pair.name}`);
    names.add(pair.name);
    const v = pair.viewport;
    if (!isObject(v) || !Number.isInteger(v.width) || !Number.isInteger(v.height) || v.width <= 0 || v.height <= 0) throw new Error(`${at} (${pair.name}): viewport needs integer width and height`);
    if (!isObject(pair.story) || typeof pair.story.id !== 'string' || pair.story.id === '') throw new Error(`${at} (${pair.name}): story.id is required`);
    if (!isObject(pair.prototype)) throw new Error(`${at} (${pair.name}): prototype is required (use {} for the defaults)`);
    const { props, steps } = pair.prototype;
    if (props !== undefined && !isObject(props)) throw new Error(`${at} (${pair.name}): prototype.props must be an object`);
    if (steps !== undefined && !Array.isArray(steps)) throw new Error(`${at} (${pair.name}): prototype.steps must be an array`);
    // Task 8b: a story may take the same steps (a press that shows a toast), run after it settles.
    if (pair.story.steps !== undefined && !Array.isArray(pair.story.steps)) throw new Error(`${at} (${pair.name}): story.steps must be an array`);
    [...(steps ?? []), ...(pair.story.steps ?? [])].forEach((s, j) => {
      const keys = Object.keys(s ?? {});
      const ok = keys.length === 1 && ['click', 'rightClick', 'hover', 'press', 'type', 'wait', 'waitFor'].includes(keys[0]);
      if (!ok) throw new Error(`${at} (${pair.name}): step ${j} needs exactly one of click, rightClick, hover, press, type, wait, waitFor`);
      if (keys[0] === 'type' && !(Array.isArray(s.type) && s.type.length === 2 && s.type.every(x => typeof x === 'string'))) throw new Error(`${at} (${pair.name}): step ${j} type is [selector, text]`);
      if (keys[0] === 'wait' && typeof s.wait !== 'number') throw new Error(`${at} (${pair.name}): step ${j} wait is milliseconds`);
    });
  });
  return spec;
}

/** The iframe URL of a story; globals use Storybook's `key:value;key:value` form and are URL-encoded. */
export function storyUrl(base, story) {
  const u = new URL('iframe.html', base.endsWith('/') ? base : `${base}/`);
  u.searchParams.set('id', story.id);
  u.searchParams.set('viewMode', 'story');
  u.searchParams.set('globals', story.globals ?? DEFAULT_GLOBALS);
  return u.href;
}

/**
 * Merges props into the prototype's `data-props` JSON (prop metadata: {name: {editor, default, ...}}) by setting each
 * prop's `default`, which is what support.js hands the component as this.props. Unknown props get a bare entry.
 * (Pure twin of the in-page patch in installPropsInjection, which cannot import.)
 */
export function patchDataProps(raw, props) {
  let meta = {};
  try { const parsed = raw ? JSON.parse(raw) : {}; if (isObject(parsed)) meta = parsed; } catch { /* start from empty */ }
  for (const [k, v] of Object.entries(props ?? {})) meta[k] = { ...(isObject(meta[k]) ? meta[k] : {}), default: v };
  return JSON.stringify(meta);
}

/** Runs in the page before the prototype's scripts: rewrites script[data-dc-script][data-props] as the parser adds it. */
function installPropsInjection(props) {
  const patch = el => {
    if (el.nodeType !== 1 || !el.matches || !el.matches('script[data-dc-script]') || el.dataset.parityPatched) return;
    let meta = {};
    try { const p = JSON.parse(el.getAttribute('data-props') || '{}'); if (p && typeof p === 'object' && !Array.isArray(p)) meta = p; } catch { /* empty */ }
    for (const [k, v] of Object.entries(props)) meta[k] = { ...(meta[k] && typeof meta[k] === 'object' ? meta[k] : {}), default: v };
    el.setAttribute('data-props', JSON.stringify(meta));
    el.dataset.parityPatched = '1';
  };
  const obs = new MutationObserver(muts => { for (const m of muts) m.addedNodes.forEach(patch); });
  obs.observe(document, { childList: true, subtree: true });
  document.addEventListener('DOMContentLoaded', () => document.querySelectorAll('script[data-dc-script]').forEach(patch), { capture: true });
}

async function settle(page) {
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(SETTLE_MS);
}

async function runSteps(page, steps) {
  for (const s of steps ?? []) {
    if (s.click) await page.click(s.click);
    else if (s.rightClick) await page.click(s.rightClick, { button: 'right' });
    else if (s.waitFor) await page.waitForSelector(s.waitFor, { timeout: 30_000 });
    else if (s.hover) await page.hover(s.hover);
    else if (s.press) await page.keyboard.press(s.press);
    else if (s.type) await page.fill(s.type[0], s.type[1]);
    else if (typeof s.wait === 'number') await page.waitForTimeout(s.wait);
  }
  if ((steps ?? []).length > 0) await settle(page);
}

async function capture(page, out, clip) {
  if (clip) {
    const loc = page.locator(clip).first();
    await loc.scrollIntoViewIfNeeded();
    const box = await loc.boundingBox();
    if (!box) throw new Error(`clip ${clip}: no box`);
    await page.screenshot({ path: out, clip: padClip(box), fullPage: true });
  } else await page.screenshot({ path: out });
}

async function shootPrototype(browser, pair, out) {
  const context = await browser.newContext({ viewport: pair.viewport, reducedMotion: 'reduce', deviceScaleFactor: SCALE });
  try {
    const page = await context.newPage();
    await routeFonts(page);
    await page.addInitScript(installPropsInjection, pair.prototype.props ?? {});
    await page.goto(pathToFileURL(PROTOTYPE).href, { waitUntil: 'load' });
    await settle(page);
    await runSteps(page, pair.prototype.steps);
    await capture(page, out, pair.prototype.clip);
  } finally { await context.close(); }
}

async function shootStory(browser, pair, base, out) {
  const context = await browser.newContext({ viewport: pair.viewport, reducedMotion: 'reduce', deviceScaleFactor: SCALE });
  try {
    const page = await context.newPage();
    const response = await page.goto(storyUrl(base, pair.story), { waitUntil: 'load' });
    if (!response?.ok()) throw new Error(`story ${pair.story.id}: HTTP ${response?.status()}`);
    await page.waitForSelector('#storybook-root > *', { state: 'attached', timeout: 30_000 });
    await settle(page);
    await runSteps(page, pair.story.steps);
    await capture(page, out, pair.story.clip);
  } finally { await context.close(); }
}

async function composePair(browser, left, right, out) {
  const b64 = f => `data:image/png;base64,${fs.readFileSync(f).toString('base64')}`;
  const page = await browser.newPage();
  try {
    await page.setContent(`<body style="margin:0;background:#888"><div style="display:flex;align-items:flex-start;gap:1px;width:max-content"><img src="${b64(left)}"><img src="${b64(right)}"></div></body>`);
    await page.waitForFunction(() => [...document.images].every(i => i.complete && i.naturalWidth > 0));
    await page.locator('body > div').screenshot({ path: out });
  } finally { await page.close(); }
}

export async function main(argv) {
  const args = argv.slice(2);
  let storybook = 'http://127.0.0.1:6006';
  const i = args.indexOf('--storybook');
  if (i >= 0) { storybook = args[i + 1]; args.splice(i, 2); }
  const [specFile, outDir] = args;
  if (!specFile || !outDir || !storybook) { console.error('usage: shoot.mjs <spec.json> <outDir> [--storybook http://127.0.0.1:6006]'); return 2; }
  const spec = validateSpec(JSON.parse(fs.readFileSync(specFile, 'utf8')));
  fs.mkdirSync(outDir, { recursive: true });
  const browser = await chromium.launch({ headless: true });
  let failed = 0;
  try {
    for (const pair of spec) {
      const p = n => path.join(outDir, `${pair.name}.${n}.png`);
      try {
        await shootPrototype(browser, pair, p('prototype'));
        await shootStory(browser, pair, storybook, p('story'));
        await composePair(browser, p('prototype'), p('story'), p('pair'));
        console.log(`ok   ${pair.name}  ${p('pair')}`);
      } catch (e) {
        failed += 1;
        console.log(`FAIL ${pair.name}  ${e?.message ?? e}`);
      }
    }
  } finally { await browser.close(); }
  return failed > 0 ? 1 : 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = await main(process.argv);
}
