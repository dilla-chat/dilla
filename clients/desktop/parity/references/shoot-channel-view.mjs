#!/usr/bin/env node
// Shoots the channel-view parity reference from the current web client (packages/web, the beta.5 code the Electron app
// loads) and writes it, with the fixture the GPUI side renders, under clients/desktop/parity/references/linux/.
//
// Usage (from the repository root, after `npm run build -w @dilla/web`, `go build -o target/dilla-testhost
// ./cmd/dilla-testhost` and the WASI core build, see README.md):
//   node clients/desktop/parity/references/shoot-channel-view.mjs <basic|wide> [outDir]
// `basic` writes linux/channel-view-2x.png and linux/channel-view.fixture.json; `wide` the channel-view-wide pair.
//
// It runs `cargo run -p dilla-desktop --example parity_world` (a real dillad with the fixture in it), signs Ada in on a
// fresh Chromium the way a new browser signs in (password, authenticator code, recovery key), opens #general and takes
// one 1280x800 frame at deviceScaleFactor 2 with Chromium's glyph hinting off (`--font-render-hinting=none
// --enable-font-subpixel-positioning`, the Linux reference Jonas chose on 2026-11-12, controller ruling R8).
import { chromium } from 'playwright';
import { spawn } from 'node:child_process';
import { createHmac } from 'node:crypto';
import { copyFileSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, '../../../..');
const SCENARIO = process.argv[2];
if (SCENARIO !== 'basic' && SCENARIO !== 'wide') throw new Error('usage: shoot-channel-view.mjs <basic|wide> [outDir]');
const NAME = SCENARIO === 'basic' ? 'channel-view' : 'channel-view-wide';
const OUT = resolve(process.argv[3] ?? join(REPO, 'target', `parity-world-${SCENARIO}-${process.pid}`));
const DEST = join(HERE, 'linux');
const WAIT = 60_000;

function base32(secret) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = 0, value = 0;
  const out = [];
  for (const ch of secret.replace(/=+$/, '').toUpperCase()) {
    value = (value << 5) | alphabet.indexOf(ch);
    bits += 5;
    if (bits >= 8) { bits -= 8; out.push((value >>> bits) & 0xff); }
  }
  return Buffer.from(out);
}

function totp(secret, counter) {
  const msg = Buffer.alloc(8);
  msg.writeBigUInt64BE(BigInt(counter));
  const d = createHmac('sha1', base32(secret)).update(msg).digest();
  const o = d[d.length - 1] & 0x0f;
  const word = ((d[o] & 0x7f) << 24) | (d[o + 1] << 16) | (d[o + 2] << 8) | d[o + 3];
  return String(word % 1_000_000).padStart(6, '0');
}

/** The next code the host takes: a counter past the last one it accepted, within its skew (moving its clock if needed). */
async function nextCode(state) {
  for (;;) {
    const now = (await (await fetch(`${state.control}/debug/clock`)).json()).now;
    const c = Math.floor(now / 30);
    const want = Math.max(c - 1, state.totpLast + 1);
    if (want <= c + 1) { state.totpLast = want; return totp(state.totpSecret, want); }
    await fetch(`${state.control}/debug/clock`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ seconds: 30 }) });
  }
}

function startWorld() {
  const child = spawn('cargo', ['run', '-q', '-p', 'dilla-desktop', '--example', 'parity_world', '--', SCENARIO, OUT], {
    cwd: REPO, stdio: ['pipe', 'pipe', 'inherit'],
  });
  const waiters = {};
  const phase = (name) => new Promise((resolvePhase, reject) => {
    waiters[name] = resolvePhase;
    child.on('exit', (code) => reject(new Error(`parity_world exited (${code})`)));
  });
  const signin = phase('SIGNIN');
  const ready = phase('READY');
  createInterface({ input: child.stdout }).on('line', (line) => {
    const name = line.split(' ')[0];
    waiters[name]?.();
  });
  return { child, signin, ready };
}

const world = startWorld();
try {
  await world.signin;
  const state = JSON.parse(readFileSync(join(OUT, 'state.json'), 'utf8'));
  const VIEWPORT = { width: state.window[0], height: state.window[1] };
  const browser = await chromium.launch({ args: ['--font-render-hinting=none', '--enable-font-subpixel-positioning'] });
  const context = await browser.newContext({
    viewport: VIEWPORT, deviceScaleFactor: 2, reducedMotion: 'reduce', timezoneId: 'UTC', locale: 'en-US',
  });
  const page = await context.newPage();
  page.setDefaultTimeout(WAIT);
  await page.goto(`${state.publicUrl}/welcome`);
  await page.getByRole('button', { name: 'Already have an account? Sign in', exact: true }).click();
  await page.getByLabel('Username', { exact: true }).fill(state.username);
  await page.getByLabel('Password', { exact: true }).fill(state.password);
  await page.getByRole('button', { name: 'Continue', exact: true }).click();
  await page.getByRole('heading', { name: 'Enter your code' }).waitFor();
  await page.getByLabel('Code', { exact: true }).fill(await nextCode(state));
  await page.getByRole('heading', { name: 'Add this browser' }).waitFor();
  await page.getByLabel('Recovery key', { exact: true }).fill(state.recoveryKey);
  await page.getByRole('button', { name: 'Add browser', exact: true }).click();
  await page.getByRole('button', { name: 'Open dilla', exact: true }).click();
  // The shell; then the scenario's server and channel (the browser joins its group as it opens it), then the history,
  // posted by the world's clients.
  await page.locator('.d-channel-header').waitFor();
  await page.locator('.d-community-rail__item').filter({ hasText: state.server }).click();
  await page.locator('.d-chrow').filter({ hasText: state.channel }).first().click();
  await page.getByPlaceholder(`Message #${state.channel}`).waitFor();
  await page.waitForTimeout(3000);
  world.child.stdin.write('post\n');
  await world.ready;
  await page.getByText(state.lastMessage, { exact: true }).waitFor();
  // The member pane stays closed: the slice has no presence, so the view is compared without it.
  const members = page.locator('.d-channel-header [data-members-toggle]');
  if ((await members.getAttribute('aria-pressed')) === 'true' || (await members.getAttribute('aria-expanded')) === 'true') {
    await members.click();
  }
  await page.mouse.move(VIEWPORT.width - 2, VIEWPORT.height - 2);
  await page.evaluate(() => document.activeElement instanceof HTMLElement && document.activeElement.blur());
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(1000);
  mkdirSync(DEST, { recursive: true });
  const file = join(DEST, `${NAME}-2x.png`);
  await page.screenshot({ path: file });
  // The geometry behind the frame, for porting: every visible box and text run with its computed type and colour
  // (written beside the world's state, never into the repository).
  const layout = await page.evaluate(() => {
    const out = [];
    const visible = (r) => r.width > 0 && r.height > 0 && r.bottom > 0 && r.top < innerHeight;
    for (const el of document.querySelectorAll('body *')) {
      const r = el.getBoundingClientRect();
      if (!visible(r)) continue;
      const cs = getComputedStyle(el);
      if (cs.visibility === 'hidden' || cs.display === 'none') continue;
      const own = [...el.childNodes].filter((n) => n.nodeType === 3 && n.textContent.trim() !== '');
      const texts = own.map((n) => { const range = document.createRange(); range.selectNodeContents(n);
        return { text: n.textContent, rects: [...range.getClientRects()].map((q) => [q.x, q.y, q.width, q.height]) }; });
      out.push({ tag: el.tagName.toLowerCase(), cls: el.getAttribute('class') ?? '', rect: [r.x, r.y, r.width, r.height],
        bg: cs.backgroundColor, color: cs.color, font: `${cs.fontStyle} ${cs.fontWeight} ${cs.fontSize}/${cs.lineHeight} ${cs.fontFamily}`,
        border: `${cs.borderTopWidth} ${cs.borderTopColor} | ${cs.borderRightWidth} ${cs.borderRightColor} | ${cs.borderBottomWidth} ${cs.borderBottomColor} | ${cs.borderLeftWidth} ${cs.borderLeftColor}`,
        radius: cs.borderRadius, shadow: cs.boxShadow, outline: `${cs.outlineWidth} ${cs.outlineStyle} ${cs.outlineColor}`, ls: cs.letterSpacing,
        tt: cs.textTransform, texts });
    }
    return out;
  });
  writeFileSync(join(OUT, `${NAME}.layout.json`), JSON.stringify(layout, null, 1));
  copyFileSync(join(OUT, 'fixture.json'), join(DEST, `${NAME}.fixture.json`));
  console.log('wrote', file);
  await browser.close();
} finally {
  world.child.stdin.end();
}
