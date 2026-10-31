#!/usr/bin/env node
// Design parity: shoots the Signal prototype and either a Storybook story or the running web app (the ux world over CDP, or a
// signed-out browser) at the same viewport and writes the two frames and a side-by-side pair. Usage and the rule: see README.md.
import { chromium } from 'playwright';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
export const PROTOTYPE = path.resolve(HERE, '../prototype/Dilla Signal Prototype.dc.html');
export const DEFAULT_GLOBALS = 'theme:signal;motion:reduced';
const SETTLE_MS = 600;
export const CLIP_PAD = 8;
export const APP_WHO = ['ada', 'bjorn', 'mira', 'phone', 'fresh', 'freshPhone'];
export const PEOPLE = ['ada', 'bjorn', 'mira', 'liv'];
export const DEFAULT_STATE = 'target/ui-screens/signal-b/state.json';
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
    if (pair.mutates !== undefined && typeof pair.mutates !== 'boolean') throw new Error(`${at} (${pair.name}): mutates must be true or false`);
    if (names.has(pair.name)) throw new Error(`${at}: duplicate name ${pair.name}`);
    names.add(pair.name);
    const v = pair.viewport;
    if (!isObject(v) || !Number.isInteger(v.width) || !Number.isInteger(v.height) || v.width <= 0 || v.height <= 0) throw new Error(`${at} (${pair.name}): viewport needs integer width and height`);
    if ((pair.story === undefined) === (pair.app === undefined)) throw new Error(`${at} (${pair.name}): exactly one of story or app is required`);
    if (pair.story !== undefined && (!isObject(pair.story) || typeof pair.story.id !== 'string' || pair.story.id === '')) throw new Error(`${at} (${pair.name}): story.id is required`);
    if (!isObject(pair.prototype)) throw new Error(`${at} (${pair.name}): prototype is required (use {} for the defaults)`);
    const { props, steps } = pair.prototype;
    if (props !== undefined && !isObject(props)) throw new Error(`${at} (${pair.name}): prototype.props must be an object`);
    if (steps !== undefined && !Array.isArray(steps)) throw new Error(`${at} (${pair.name}): prototype.steps must be an array`);
    const other = pair.story ?? pair.app;
    for (const side of [pair.prototype, other]) {
      if (side?.settleMs !== undefined && !(typeof side.settleMs === 'number' && Number.isFinite(side.settleMs) && side.settleMs >= 0)) throw new Error(`${at} (${pair.name}): settleMs must be finite non-negative milliseconds`);
    }
    if (pair.app !== undefined) {
      const a = pair.app;
      if (!isObject(a) || !APP_WHO.includes(a.who)) throw new Error(`${at} (${pair.name}): app.who must be one of ${APP_WHO.join(', ')}`);
      if (a.holdUploads !== undefined && typeof a.holdUploads !== 'boolean') throw new Error(`${at} (${pair.name}): app.holdUploads must be a boolean`);
      for (const k of ['path', 'clip', 'reset']) if (a[k] !== undefined && typeof a[k] !== 'string') throw new Error(`${at} (${pair.name}): app.${k} must be a string`);
    }
    // Task 8b: a story may take the same steps (a press that shows a toast), run after it settles. An app side takes them too.
    if (other.steps !== undefined && !Array.isArray(other.steps)) throw new Error(`${at} (${pair.name}): ${pair.story ? 'story' : 'app'}.steps must be an array`);
    [...(steps ?? []), ...(other.steps ?? [])].forEach((s, j) => {
      const keys = Object.keys(s ?? {});
      const ok = keys.length === 1 && ['click', 'rightClick', 'hover', 'wheel', 'press', 'type', 'wait', 'waitFor', 'goto', 'totp', 'totpFrom', 'upload', 'dispatch'].includes(keys[0]);
      if (!ok) throw new Error(`${at} (${pair.name}): step ${j} needs exactly one of click, rightClick, hover, wheel, press, type, wait, waitFor, goto, totp, totpFrom, upload, dispatch`);
      if (keys[0] === 'wheel' && !(Array.isArray(s.wheel) && s.wheel.length === 3 && typeof s.wheel[0] === 'string' && s.wheel[0] !== '' && s.wheel.slice(1).every(n => typeof n === 'number' && Number.isFinite(n)))) throw new Error(`${at} (${pair.name}): step ${j} wheel is [selector, finite deltaX, finite deltaY]`);
      if (keys[0] === 'type' && !(Array.isArray(s.type) && s.type.length === 2 && s.type.every(x => typeof x === 'string'))) throw new Error(`${at} (${pair.name}): step ${j} type is [selector, text]`);
      if (keys[0] === 'wait' && typeof s.wait !== 'number') throw new Error(`${at} (${pair.name}): step ${j} wait is milliseconds`);
      if (keys[0] === 'waitFor' && !(typeof s.waitFor === 'string' && s.waitFor !== '' || isObject(s.waitFor) && typeof s.waitFor.selector === 'string' && s.waitFor.selector !== '' && Number.isFinite(s.waitFor.timeout) && s.waitFor.timeout > 0)) throw new Error(`${at} (${pair.name}): step ${j} waitFor is a selector or { selector, timeout } with a positive timeout`);
      if (keys[0] === 'goto' && typeof s.goto !== 'string') throw new Error(`${at} (${pair.name}): step ${j} goto is a path`);
      if (keys[0] === 'totp' && !(Array.isArray(s.totp) && s.totp.length === 2 && typeof s.totp[0] === 'string' && PEOPLE.includes(s.totp[1]))) throw new Error(`${at} (${pair.name}): step ${j} totp is [selector, who] with who one of ${PEOPLE.join(', ')}`);
      // Wave B task 3b: an account the pair itself signs up is not in state.people; its code comes from the setup key on the page.
      if (keys[0] === 'totpFrom' && !(Array.isArray(s.totpFrom) && s.totpFrom.length === 2 && s.totpFrom.every(x => typeof x === 'string' && x !== ''))) throw new Error(`${at} (${pair.name}): step ${j} totpFrom is [setup key selector, code selector]`);
      if (keys[0] === 'upload' && !(Array.isArray(s.upload) && s.upload.length >= 2 && s.upload.every(x => typeof x === 'string'))) throw new Error(`${at} (${pair.name}): step ${j} upload is [selector, file, ...] (files relative to the repo)`);
    });
  });
  return spec;
}

/**
 * Standing screens see the fresh world before any browser reloads can end Mira's share. Then read-only pairs run, followed by
 * `mutates: true` pairs (send messages, add a device or role, switch a setting). Order is stable inside each group; input is unchanged.
 */
export const orderPairs = spec => [
  ...spec.filter(p => p.name.startsWith('standing-')),
  ...spec.filter(p => !p.name.startsWith('standing-') && p.mutates !== true),
  ...spec.filter(p => !p.name.startsWith('standing-') && p.mutates === true),
];

/** The URL a path has in the running app; `{invite}` expands to state.invitePath (the world's invite link path) and `{server}` to state.serverPath (#general of the world's server), `{community}` to the server's own path (the phone's server list). Pure. */
export function appUrl(state, p) {
  if (p.includes('{invite}') && typeof state.invitePath !== 'string') throw new Error('state has no invitePath for {invite}');
  const root = state.publicUrl.replace(/\/+$/, '');
  if (p.includes('{server}') && typeof state.serverPath !== 'string') throw new Error('state has no serverPath for {server}');
  const rel = p.replace(/\{invite\}/g, state.invitePath ?? '').replace(/\{server\}/g, state.serverPath ?? '').replace(/\{community\}/g, (state.serverPath ?? '').split('/').slice(0, 3).join('/'));
  return root + (rel.startsWith('/') ? rel : `/${rel}`);
}

/** Fresh auth pages need a valid WebAuthn RP hostname; existing RTC profiles keep the world's numeric origin. */
export function stateForApp(state, who) {
  return (who === 'fresh' || who === 'freshPhone') && state.authUrl !== undefined
    ? { ...state, publicUrl: state.authUrl } : state;
}

/** Typed text in an app step: `{password:who}` and `{recoveryKey:who}` expand from state.people. Pure. */
export function expandText(state, text) {
  return text.replace(/\{(password|recoveryKey):(\w+)\}/g, (_, k, who) => {
    const v = state.people?.[who]?.[k];
    if (typeof v !== 'string') throw new Error(`state has no ${k} for ${who}`);
    return v;
  });
}

/** Which second side a pair has: its output stem is `<name>.app.png` or `<name>.story.png`. */
export const sideOf = pair => (pair.app !== undefined ? 'app' : 'story');
/** Output path of one frame of a pair: kind is prototype, app, story or pair. */
export const pairFile = (outDir, pair, kind) => path.join(outDir, `${pair.name}.${kind}.png`);

/**
 * Command line: `<spec> <outDir> [--storybook url] [--app [state.json]] [--judge]`. `app` is the state file path, or null
 * without --app. `--judge` adds `judge: true` (run judge.mjs on the pairs that shot ok).
 */
export function parseArgs(argv) {
  const args = [...argv];
  let storybook = 'http://127.0.0.1:6006';
  let app = null;
  const k = args.indexOf('--judge');
  const judge = k >= 0;
  if (judge) args.splice(k, 1);
  const i = args.indexOf('--storybook');
  if (i >= 0) { storybook = args[i + 1]; args.splice(i, 2); }
  const j = args.indexOf('--app');
  if (j >= 0) {
    const next = args[j + 1];
    if (next !== undefined && !next.startsWith('--')) { app = next; args.splice(j, 2); } else { app = DEFAULT_STATE; args.splice(j, 1); }
  }
  const [specFile, outDir] = args;
  return { specFile, outDir, storybook, app, ...(judge ? { judge: true } : {}) };
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

async function settle(page, milliseconds = SETTLE_MS) {
  await page.evaluate(() => document.fonts.ready);
  if (milliseconds > 0) await page.waitForTimeout(milliseconds);
}

/** On-demand upload bytes for cap-boundary captures; ordinary fixture paths stay unchanged. */
export function uploadFixture(file) {
  if (!file.startsWith('@bytes/')) return path.resolve(REPO, file);
  const match = /^@bytes\/(\d+)\/([^/]+)$/.exec(file);
  const size = Number(match?.[1]);
  if (!match || !Number.isSafeInteger(size) || size <= 0 || size > 26 * 1024 * 1024) throw new Error('Invalid generated upload fixture');
  return { name: match[2], mimeType: 'application/octet-stream', buffer: Buffer.alloc(size) };
}

export async function runSteps(page, steps, ctx = {}) {
  for (const s of steps ?? []) {
    if (s.goto !== undefined) { await page.goto(appUrl(ctx.state, s.goto), { waitUntil: 'load' }); await page.waitForTimeout(300); }
    else if (s.totp) {
      const { nextCode } = await import(path.join(REPO, 'e2e/web/support/totp.ts'));
      await page.fill(s.totp[0], await nextCode(ctx.state.controlUrl, ctx.state.people[s.totp[1]].totp, { wallClock: true }));
    }
    else if (s.totpFrom) {
      // The setup key the page shows (a new account's, never stored here), then the code for it from the host's clock.
      const { nextCode } = await import(path.join(REPO, 'e2e/web/support/totp.ts'));
      const totpSecret = await page.inputValue(s.totpFrom[0]);
      await page.fill(s.totpFrom[1], await nextCode(ctx.state.controlUrl, { totpSecret, lastCounter: -1 }, { wallClock: true }));
    }
    else if (s.upload) await page.setInputFiles(s.upload[0], s.upload.slice(1).map(uploadFixture));
    else if (s.dispatch) await page.locator(s.dispatch).first().dispatchEvent('click');
    else if (s.click) await page.click(s.click);
    else if (s.rightClick) await page.click(s.rightClick, { button: 'right' });
    else if (s.waitFor) await page.waitForSelector(typeof s.waitFor === 'string' ? s.waitFor : s.waitFor.selector, { timeout: typeof s.waitFor === 'string' ? 30_000 : s.waitFor.timeout });
    else if (s.hover) await page.hover(s.hover);
    else if (s.wheel) {
      await page.hover(s.wheel[0]);
      await page.mouse.wheel(s.wheel[1], s.wheel[2]);
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    }
    else if (s.press) await page.keyboard.press(s.press);
    else if (s.type) await page.fill(s.type[0], ctx.state ? expandText(ctx.state, s.type[1]) : s.type[1]);
    else if (typeof s.wait === 'number') await page.waitForTimeout(s.wait);
  }
  // A click leaves the pointer over its target (notably the signup password
  // field). Preserve hover only when a pair explicitly asks to compare it.
  if (!(steps ?? []).some(s => s.hover)) await page.mouse.move(0, 0);
  if ((steps ?? []).length > 0) await settle(page, ctx.settleMs);
}

/** Wait for transient UI in the captured region, leaving unrelated panels alone. */
export async function waitForCaptureReady(page, clip) {
  const root = await page.locator(clip ?? 'body').first().elementHandle();
  if (!root) throw new Error(`capture region ${clip ?? 'body'}: not found`);
  try {
    await page.waitForFunction(el => {
      const visible = node => {
        const style = getComputedStyle(node);
        return node.getClientRects().length > 0 && style.visibility !== 'hidden' && style.display !== 'none';
      };
      if ([el, ...el.querySelectorAll('[aria-busy="true"]')].some(node => node.getAttribute('aria-busy') === 'true' && visible(node))) return false;
      const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
      while (walker.nextNode()) {
        if (/^Loading(?:\s|…|\.)/i.test(walker.currentNode.textContent.trim()) && visible(walker.currentNode.parentElement)) return false;
      }
      return true;
    }, root, { timeout: 30_000 });
  } finally { await root.dispose(); }
}

export function validateWorldForPair(state, pair) {
  if (['standing-voice-focus', 'gap75-focused-tile-fullscreen'].includes(pair.name) && state.miraShares !== true) {
    throw new Error(`${pair.name}: the world has no confirmed Mira screen share; restart the world before capturing voice focus`);
  }
}

/** A world page can finish its document load while the worker still shows the startup splash. */
export async function waitForAppReady(page, who) {
  if (who !== 'fresh' && who !== 'freshPhone') await page.waitForSelector('.d-app-shell', { state: 'visible', timeout: 60_000 });
  // All app parity pairs compare Signal. A restored preference may arrive after
  // the shell; observe it rather than capturing a transient light frame.
  await page.waitForSelector('html[data-theme="signal"]', { state: 'attached', timeout: 60_000 });
}

/** Another CDP observer can reset target-wide media during a long Bot wait. Restore the runner's OS contract, then observe the app. */
export async function prepareAppCapture(page, who, clip) {
  await page.emulateMedia({ colorScheme: 'dark', reducedMotion: 'reduce' });
  await waitForAppReady(page, who);
  await waitForAppMetadata(page, who, clip);
}

/** Shell mounting precedes account/community slices. Check only metadata actually in this frame. */
export async function waitForAppMetadata(page, who, clip) {
  if (who === 'fresh' || who === 'freshPhone') return;
  const root = await page.locator(clip ?? 'body').first().elementHandle();
  if (!root) throw new Error(`capture region ${clip ?? 'body'}: not found`);
  try {
    await page.waitForFunction(el => {
      const bounds = el.getBoundingClientRect();
      const visible = node => {
        const style = getComputedStyle(node);
        return style.visibility !== 'hidden' && style.display !== 'none' && [...node.getClientRects()].some(r =>
          r.height > 0 && r.bottom > Math.max(0, bounds.top) && r.top < Math.min(innerHeight, bounds.bottom)
          && r.right >= Math.max(0, bounds.left) && r.left <= Math.min(innerWidth, bounds.right));
      };
      const nodes = selector => [...(el.matches(selector) ? [el] : []), ...el.querySelectorAll(selector)].filter(visible);
      // Shell's unknown identity is an en dash; blank custom status is legitimate.
      if (nodes('.d-channel-list__title, .d-user-panel__name').some(node => !node.textContent.trim() || node.textContent.trim() === '–')) return false;
      // Shell deliberately omits the live online part until members AND presence
      // arrive. Zero online still has this part. No stat line is also valid (e.g. no membership).
      return nodes('.d-channel-list__stats').every(node => [...node.querySelectorAll('[data-live="true"]')].some(part => part.textContent.trim()));
    }, root, { timeout: 60_000 });
  } finally { await root.dispose(); }
}

async function capture(page, out, clip, cdp) {
  await waitForCaptureReady(page, clip);
  // With `cdp` (a world browser, emulated at scale 2 through CDP) the frame comes from Page.captureScreenshot: Playwright's own
  // screenshot would ignore an emulation it did not set.
  const grab = async region => {
    if (cdp === undefined) { await page.screenshot({ path: out, ...(region ? { clip: region, fullPage: true } : {}) }); return; }
    const { data } = await cdp.send('Page.captureScreenshot', { format: 'png', ...(region ? { clip: { ...region, scale: 1 } } : {}) });
    fs.writeFileSync(out, Buffer.from(data, 'base64'));
  };
  if (clip) {
    const loc = page.locator(clip).first();
    await loc.scrollIntoViewIfNeeded();
    const box = await loc.boundingBox();
    if (!box) throw new Error(`clip ${clip}: no box`);
    const at = await page.evaluate(() => ({ x: window.scrollX, y: window.scrollY }));
    await grab(padClip(cdp === undefined ? box : { ...box, x: box.x + at.x, y: box.y + at.y }));
  } else await grab(null);
}

export async function shootPrototype(browser, pair, out) {
  const context = await browser.newContext({ viewport: pair.viewport, reducedMotion: 'reduce', deviceScaleFactor: SCALE });
  try {
    const page = await context.newPage();
    await routeFonts(page);
    await page.addInitScript(installPropsInjection, pair.prototype.props ?? {});
    await page.goto(pathToFileURL(PROTOTYPE).href, { waitUntil: 'load' });
    await settle(page);
    await runSteps(page, pair.prototype.steps, { settleMs: pair.prototype.settleMs });
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
    await runSteps(page, pair.story.steps, { settleMs: pair.story.settleMs });
    await capture(page, out, pair.story.clip);
  } finally { await context.close(); }
}

/** A real platform authenticator seam, matching the passkey e2e fixture; no WebAuthn API overrides. */
export async function prepareFreshPage(page) {
  const { addAuthenticator } = await import(path.join(REPO, 'e2e/web/support/webauthn.ts'));
  await addAuthenticator(page);
}

/** The page of one of the world's browsers (CDP), or a new signed-out context. Returns { page, done } */
async function openAppPage(browser, state, pair) {
  const { who } = pair.app;
  const mobile = who === 'phone' || who === 'freshPhone';
  if (who === 'fresh' || who === 'freshPhone') {
    const context = await browser.newContext({
      viewport: pair.viewport, reducedMotion: 'reduce', deviceScaleFactor: SCALE, colorScheme: 'dark', locale: 'en-GB', timezoneId: 'Europe/Stockholm',
      ...(mobile ? { isMobile: true, hasTouch: true } : {}),
    });
    const page = await context.newPage();
    await prepareFreshPage(page);
    await routeFonts(page);
    return { page, mobile, cdp: undefined, restore: async () => {}, done: () => context.close() };
  }
  const port = state.cdp?.[who];
  if (port === undefined) throw new Error(`state has no browser "${who}": is the world up (npm run ux:world -w @dilla/e2e)?`);
  const remote = await chromium.connectOverCDP(`http://127.0.0.1:${port}`);
  let page, cdp, orig;
  try {
    const ctx = remote.contexts()[0];
    page = ctx.pages().find(p => p.url().startsWith('http')) ?? ctx.pages()[0];
    if (page === undefined) throw new Error(`browser "${who}" has no page`);
    orig = page.viewportSize();
    // The world's windows are 1x; the prototype is shot at 2x, so the app is emulated at the pair's viewport and scale 2.
    await page.emulateMedia({ colorScheme: 'dark', reducedMotion: 'reduce' });
    cdp = await ctx.newCDPSession(page);
    await cdp.send('Emulation.setDeviceMetricsOverride', { width: pair.viewport.width, height: pair.viewport.height, deviceScaleFactor: SCALE, mobile });
  } catch (e) { await remote.close().catch(() => {}); throw e; }
  const restore = async () => { await cdp.send('Emulation.clearDeviceMetricsOverride').catch(() => {}); if (orig) await page.setViewportSize(orig).catch(() => {}); };
  return { page, mobile, cdp, restore, done: async () => { await restore(); await remote.close().catch(() => {}); } };
}

async function shootApp(browser, state, pair, out) {
  validateWorldForPair(state, pair);
  const a = pair.app;
  const appState = stateForApp(state, a.who);
  const { page, cdp, done } = await openAppPage(browser, state, pair);
  const fresh = a.who === 'fresh' || a.who === 'freshPhone';
  let back = a.reset ?? a.path;
  let releaseUpload;
  const uploadGate = new Promise(resolve => { releaseUpload = resolve; });
  const holdUpload = async route => { await uploadGate; await route.continue().catch(() => {}); };
  const uploadPattern = /\/v1\/channels\/[0-9a-f]{32}\/blobs\/[0-9a-f]+$/;
  try {
    if (a.holdUploads) await page.route(uploadPattern, holdUpload);
    if (!fresh) back = back ?? new URL(page.url()).pathname + new URL(page.url()).search;
    if (a.path !== undefined) await page.goto(appUrl(appState, a.path), { waitUntil: 'load' });
    else if (fresh) await page.goto(appUrl(appState, '/'), { waitUntil: 'load' });
    await waitForAppReady(page, a.who);
    await page.mouse.move(-10, -10).catch(() => {});
    await settle(page);
    await runSteps(page, a.steps, { state: appState, settleMs: a.settleMs });
    await prepareAppCapture(page, a.who, a.clip);
    await capture(page, out, a.clip, cdp);
  } finally {
    releaseUpload();
    if (a.holdUploads) await page.unroute(uploadPattern, holdUpload);
    if (!fresh) {
      await page.keyboard.press('Escape').catch(() => {});
      await page.keyboard.press('Escape').catch(() => {});
      if (back !== undefined) await page.goto(appUrl(state, back), { waitUntil: 'load' }).catch(() => {});
    }
    await done();
  }
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
  const { specFile, outDir, storybook, app, judge } = parseArgs(argv.slice(2));
  if (!specFile || !outDir || !storybook) { console.error(`usage: shoot.mjs <spec.json> <outDir> [--storybook http://127.0.0.1:6006] [--app [${DEFAULT_STATE}]] [--judge]`); return 2; }
  const spec = validateSpec(JSON.parse(fs.readFileSync(specFile, 'utf8')));
  const state = app !== null && spec.some(p => p.app !== undefined) ? JSON.parse(fs.readFileSync(app ?? DEFAULT_STATE, 'utf8')) : null;
  if (spec.some(p => p.app !== undefined) && state === null) { console.error('the spec has app pairs: pass --app [state.json]'); return 2; }
  fs.mkdirSync(outDir, { recursive: true });
  const browser = await chromium.launch({ headless: true });
  let failed = 0;
  const shot = [];
  try {
    for (const pair of orderPairs(spec)) {
      const p = kind => pairFile(outDir, pair, kind);
      const side = sideOf(pair);
      try {
        await shootPrototype(browser, pair, p('prototype'));
        if (side === 'app') await shootApp(browser, state, pair, p('app'));
        else await shootStory(browser, pair, storybook, p('story'));
        await composePair(browser, p('prototype'), p(side), p('pair'));
        console.log(`ok   ${pair.name}  ${p('pair')}`);
        shot.push(pair.name);
      } catch (e) {
        failed += 1;
        console.log(`FAIL ${pair.name}  ${e?.message ?? e}`);
      }
    }
  } finally { await browser.close(); }
  if (state !== null && spec.some(p => p.mutates === true)) console.log('note: pairs marked mutates changed the world; restart it before shooting again');
  if (judge && shot.length > 0) {
    // Advisory TypeSafe judge over the frames just shot (judge.mjs; README "Judge a pair"). Only pairs that printed ok.
    const { judgeSpec } = await import('./judge.mjs');
    const { exit } = await judgeSpec(spec, outDir, { only: shot });
    if (exit !== 0) failed += 1;
  }
  return failed > 0 ? 1 : 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = await main(process.argv);
}
