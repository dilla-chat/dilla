#!/usr/bin/env node
// Shoots one screen fixture from the web client (packages/web, the code the shipping app loads) into the reference PNG
// the GPUI side is compared with: clients/desktop/parity/references/<os>/<area>/<state>[-b]-2x.png, and the sidecar
// <state>[-b].shot.json beside it (the fixture's digest, the clock shift and the ids the real dillad handed out).
//
// Usage (from the repository root, after `npm run build -w @dilla/web`, `go build -o target/dilla-testhost
// ./cmd/dilla-testhost` and the WASI core build, see references/linux/README.md):
//   node clients/desktop/parity/shoot.mjs clients/desktop/parity/fixtures/<area>/<state>.json [--out <dir>]
//   node clients/desktop/parity/shoot.mjs <fixture.json> --check      # no browser, no host: prints the fixture line
//
// The fixture is the schema of dilla-client's testing/spec.rs (clients/desktop/dilla-client/src/testing/spec.rs). The
// run is: `cargo run -p dilla-desktop --example parity_world` seeds a real dillad with the fixture's world; a fresh
// Chromium signs the viewer in on a new device the way a person does (password, authenticator code, recovery key),
// visits the channels that hold history so it can read them, and takes the world's history; then the fixture's steps
// are replayed with their `web` selectors (`-` is a step with no web counterpart; a selector starting `if:` clicks only
// when the element is there) and one viewport frame is taken at deviceScaleFactor 2, reduced motion, UTC, en-US,
// with the browser's clock frozen at the fixture's `clock`. On Linux Chromium runs with `--font-render-hinting=none
// --enable-font-subpixel-positioning` (Jonas's 2026-11-12 decision for the Linux reference, controller ruling R8).
//
// The line `fixture <name> <sha256 of the file> <kind:local=hex;...>` is printed by this script and by the GPUI driver's
// `fixture` command; the parity world prints it as `FIXTURE ...` and this script refuses to shoot if they differ.
import { createHash, createHmac } from 'node:crypto';
import { spawn } from 'node:child_process';
import { copyFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, '../../..');
const WAIT = 60_000;

// ---- The fixture line (shared with spec.rs: `derive_id`, `ScreenFixture::ids`, `ids_line`, `digest`) ----------------

export function deriveId(kind, local) {
  return createHash('sha256').update(`dilla-parity/1\0${kind}\0${local}`).digest().subarray(0, 16).toString('hex');
}

/** Every entity of the world by `kind:local`, in byte order of the key (a Rust BTreeMap's order). */
export function worldIds(fixture) {
  const w = fixture.world ?? {};
  const ids = new Map();
  const put = (kind, local) => ids.set(`${kind}:${local}`, deriveId(kind, local));
  for (const p of w.people ?? []) put('person', p.id);
  for (const s of w.servers ?? []) {
    put('server', s.id);
    for (const c of s.channels ?? []) put('channel', `${s.id}/${c.id}`);
    for (const r of s.roles ?? []) put('role', `${s.id}/${r.id}`);
  }
  for (const d of w.dms ?? []) put('dm', d.id);
  for (const m of w.messages ?? []) put('message', m.key);
  return [...ids.entries()].sort(([a], [b]) => Buffer.compare(Buffer.from(a), Buffer.from(b)));
}

/** The ids both sides use: derived, replaced by the ones a sidecar recorded (strict hex, keys that are in the world). */
export function effectiveIds(fixture, shotIds = {}) {
  const ids = new Map(worldIds(fixture));
  for (const [key, value] of Object.entries(shotIds)) {
    if (!ids.has(key)) throw new Error(`sidecar id \`${key}\` is not in the fixture's world`);
    if (!/^[0-9a-f]{32}$/.test(value)) throw new Error(`sidecar id \`${key}\`: \`${value}\` is not 32 lower-case hex digits`);
    ids.set(key, value);
  }
  return [...ids.entries()].sort(([a], [b]) => Buffer.compare(Buffer.from(a), Buffer.from(b)));
}

export function fixtureLine(fixture, text, shotIds = {}) {
  const digest = createHash('sha256').update(text).digest('hex');
  const ids = effectiveIds(fixture, shotIds).map(([k, v]) => `${k}=${v}`).join(';');
  return `fixture ${fixture.name} ${digest} ${ids}`;
}

/** `{server}`, `{server/channel}`, `{dm/<id>}` in a route, replaced by `lookup(kind, local)`. */
export function resolveRoute(route, lookup) {
  return route.replace(/\{([^}]*)\}/g, (_, name) => {
    if (name.startsWith('dm/')) return lookup('dm', name.slice(3));
    return name.includes('/') ? lookup('channel', name) : lookup('server', name);
  });
}

// ---- Second factor ----------------------------------------------------------------------------------------------

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

// ---- Arguments ----------------------------------------------------------------------------------------------------

function parseArgs(argv) {
  const args = { fixture: null, out: null, check: false };
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--check') args.check = true;
    else if (argv[i] === '--out') args.out = resolve(argv[++i] ?? '');
    else if (argv[i].startsWith('-')) throw new Error(`unknown option ${argv[i]}`);
    else if (args.fixture === null) args.fixture = resolve(argv[i]);
    else throw new Error('one fixture at a time');
  }
  if (args.fixture === null) throw new Error('usage: shoot.mjs <fixtures/<area>/<state>.json> [--out <dir>] [--check]');
  return args;
}

/** Where the PNG and its sidecar go: the committed references, or `<out>/shots/<area>` when `--out` is given (so a trial run never overwrites them). */
export function shotDir(parity, os, name, out) {
  const area = name.split('/').slice(0, -1);
  return out ? join(out, 'shots', ...area) : join(parity, 'references', os, ...area);
}

function osDir() {
  return { linux: 'linux', darwin: 'macos', win32: 'windows' }[process.platform] ?? process.platform;
}

// ---- The run ------------------------------------------------------------------------------------------------------

function startWorld(fixturePath, out) {
  const child = spawn('cargo', ['run', '-q', '-p', 'dilla-desktop', '--example', 'parity_world', '--', fixturePath, out], {
    cwd: REPO, stdio: ['pipe', 'pipe', 'inherit'],
  });
  const waiters = {};
  // Every phase, the fixture line included, fails when the world exits before it prints.
  const phase = (name) => new Promise((resolvePhase, reject) => {
    waiters[name] = resolvePhase;
    child.on('exit', (code) => reject(new Error(`parity_world exited (${code}) before ${name}`)));
  });
  const fixtureLine = phase('FIXTURE');
  const signin = phase('SIGNIN');
  const ready = phase('READY');
  createInterface({ input: child.stdout }).on('line', (line) => {
    const name = line.split(' ')[0];
    waiters[name]?.(line);
  });
  return { child, signin, ready, fixtureLine };
}

/** Opens a server's channel the way a person does, so the browser joins its group before the history is posted. */
async function visit(page, server, channel) {
  await page.locator('.d-community-rail__item').filter({ hasText: server }).first().click();
  await page.locator('.d-chrow').filter({ hasText: channel }).first().click();
  await page.getByPlaceholder(`Message #${channel}`).waitFor();
}

/** A chord of the fixture grammar as Playwright's key: `ctrl-shift-m` -> `Control+Shift+m`; `secondary` is Control off
 * macOS; a function key `f1`..`f12` is Playwright's `F1`..`F12`; a bare letter or digit stays as written (lower case:
 * `shift` is its own modifier). */
export function playwrightChord(chord) {
  const names = { ctrl: 'Control', alt: 'Alt', shift: 'Shift', fn: 'Fn', secondary: 'Control', enter: 'Enter', tab: 'Tab', escape: 'Escape', backspace: 'Backspace', delete: 'Delete', space: 'Space', up: 'ArrowUp', down: 'ArrowDown', left: 'ArrowLeft', right: 'ArrowRight', home: 'Home', end: 'End', pageup: 'PageUp', pagedown: 'PageDown', minus: 'Minus', comma: 'Comma', period: 'Period', slash: 'Slash', semicolon: 'Semicolon', quote: 'Quote', backslash: 'Backslash', equals: 'Equal', backtick: 'Backquote', insert: 'Insert' };
  return chord.split('-').map((p) => names[p] ?? (/^f([1-9]|1[0-2])$/.test(p) ? p.toUpperCase() : p)).join('+');
}

/** One step of the fixture, on the web side. */
async function replayStep(page, step) {
  const pick = (sel) => {
    if (sel === '-') return null;
    const optional = sel.startsWith('if:');
    return { optional, locator: page.locator(optional ? sel.slice(3) : sel).first() };
  };
  switch (step.do) {
    case 'click': case 'hover': {
      const t = pick(step.web);
      if (!t) return;
      if (t.optional && (await t.locator.count()) === 0) return;
      await (step.do === 'click' ? t.locator.click() : t.locator.hover());
      return;
    }
    case 'type': {
      const t = pick(step.web);
      if (!t) return;
      await t.locator.click();
      await page.keyboard.type(step.text);
      return;
    }
    case 'key': {
      await page.keyboard.press(playwrightChord(step.chord));
      return;
    }
    case 'settle':
      await page.evaluate(() => document.fonts.ready);
      await page.waitForTimeout(500);
      return;
    default:
      throw new Error(`unknown step ${JSON.stringify(step)}`);
  }
}

async function shoot(args, fixture, text) {
  if (process.platform !== 'linux') {
    throw new Error(`references are shot on Linux only for now (${process.platform}): macOS and Windows tolerances are PENDING-CI`);
  }
  const OS = osDir();
  const parity = resolve(args.fixture, '../../..');
  const out = args.out ?? join(REPO, 'target', `parity-world-${fixture.name.replace('/', '-')}-${process.pid}`);
  const dest = shotDir(parity, OS, fixture.name, args.out);
  const leaf = fixture.name.split('/').at(-1);
  const world = startWorld(args.fixture, out);
  let browser = null;
  try {
    const worldLine = await world.fixtureLine;
    const mine = fixtureLine(fixture, text);
    if (worldLine.replace(/^FIXTURE /, 'fixture ') !== mine) {
      throw new Error(`the web shooter and the parity world read different fixtures:\n  ${mine}\n  ${worldLine}`);
    }
    await world.signin;
    const state = JSON.parse(readFileSync(join(out, 'state.json'), 'utf8'));
    const viewport = { width: state.window[0], height: state.window[1] };
    const { chromium } = await import('playwright');
    // A fake capture device that the browser grants without asking: the Voice page's mic test runs as on a machine with a
    // microphone (a shooter without one shows "The microphone test could not start."). It plays silence (`silence.wav`,
    // one second of zeros) where Chromium's default fake device beeps, as the GPUI side's test hears no input.
    const media = ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream',
      `--use-file-for-fake-audio-capture=${join(HERE, 'silence.wav')}`];
    const args2 = process.platform === 'linux' ? ['--font-render-hinting=none', '--enable-font-subpixel-positioning', ...media] : media;
    browser = await chromium.launch({ args: args2 });
    const context = await browser.newContext({ viewport, deviceScaleFactor: 2, reducedMotion: 'reduce', timezoneId: 'UTC', locale: 'en-US' });
    const page = await context.newPage();
    page.setDefaultTimeout(WAIT);
    await page.clock.setFixedTime(state.now * 1000);
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
    await page.locator('.d-channel-header').waitFor();
    for (const v of state.visit) await visit(page, v.server, v.channel);
    await page.waitForTimeout(3000);
    world.child.stdin.write('post\n');
    await world.ready;
    // The route: in-app, with the real ids.
    const route = resolveRoute(fixture.route, (kind, local) => state.ids[`${kind}:${local}`]);
    if (new URL(page.url()).pathname !== route.split('?')[0]) {
      await page.evaluate((p) => { history.pushState({}, '', p); dispatchEvent(new PopStateEvent('popstate')); }, route);
    }
    // The last message of the route's own channel is on screen before the frame.
    const target = [...fixture.route.matchAll(/\{([^}/]+\/[^}]+)\}/g)].map((m) => m[1]).at(-1);
    const last = (fixture.world.messages ?? []).filter((m) => m.channel === target).at(-1);
    if (last) await page.getByText(last.body, { exact: true }).first().waitFor();
    for (const step of fixture.steps ?? []) await replayStep(page, step);
    if (!(fixture.steps ?? []).some((s) => s.do === 'hover')) {
      await page.mouse.move(viewport.width - 2, viewport.height - 2);
      await page.evaluate(() => document.activeElement instanceof HTMLElement && document.activeElement.blur());
    }
    await page.evaluate(() => document.fonts.ready);
    await page.waitForTimeout(1000);
    mkdirSync(dest, { recursive: true });
    const file = join(dest, `${leaf}-2x.png`);
    await page.screenshot({ path: file });
    // The geometry behind the frame, for porting: every visible box and text run with its computed style (written beside
    // the world's state, never into the repository).
    const layout = await page.evaluate(() => {
      const outRows = [];
      const visible = (r) => r.width > 0 && r.height > 0 && r.bottom > 0 && r.top < innerHeight;
      for (const el of document.querySelectorAll('body *')) {
        const r = el.getBoundingClientRect();
        if (!visible(r)) continue;
        const cs = getComputedStyle(el);
        if (cs.visibility === 'hidden' || cs.display === 'none') continue;
        const own = [...el.childNodes].filter((n) => n.nodeType === 3 && n.textContent.trim() !== '');
        const texts = own.map((n) => { const range = document.createRange(); range.selectNodeContents(n);
          return { text: n.textContent, rects: [...range.getClientRects()].map((q) => [q.x, q.y, q.width, q.height]) }; });
        outRows.push({ tag: el.tagName.toLowerCase(), cls: el.getAttribute('class') ?? '', rect: [r.x, r.y, r.width, r.height],
          bg: cs.backgroundColor, color: cs.color, font: `${cs.fontStyle} ${cs.fontWeight} ${cs.fontSize}/${cs.lineHeight} ${cs.fontFamily}`,
          border: `${cs.borderTopWidth} ${cs.borderTopColor} | ${cs.borderRightWidth} ${cs.borderRightColor} | ${cs.borderBottomWidth} ${cs.borderBottomColor} | ${cs.borderLeftWidth} ${cs.borderLeftColor}`,
          radius: cs.borderRadius, shadow: cs.boxShadow, outline: `${cs.outlineWidth} ${cs.outlineStyle} ${cs.outlineColor}`, ls: cs.letterSpacing,
          tt: cs.textTransform, texts });
      }
      return outRows;
    });
    writeFileSync(join(out, `${leaf}.layout.json`), JSON.stringify(layout, null, 1));
    copyFileSync(join(out, 'world.json'), join(dest, `${leaf}.shot.json`));
    console.log('wrote', file);
  } finally {
    await browser?.close();
    world.child.stdin.end();
  }
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const args = parseArgs(process.argv.slice(2));
  const text = readFileSync(args.fixture);
  const fixture = JSON.parse(text.toString('utf8'));
  // The sidecar beside the reference, when there is one, gives the ids both sides log.
  const sidecar = join(resolve(args.fixture, '../../..'), 'references', osDir(), `${fixture.name}.shot.json`);
  const shotIds = existsSync(sidecar) ? JSON.parse(readFileSync(sidecar, 'utf8')).ids ?? {} : {};
  console.log(fixtureLine(fixture, text, shotIds));
  if (!args.check) await shoot(args, fixture, text);
}
