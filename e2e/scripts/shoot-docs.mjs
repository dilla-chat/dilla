// Re-shoots the screenshots of the user documentation (docs/user/screenshots/*.png) against a real
// dilla-testhost serving the built web client, the way the web e2e starts it (e2e/web/support/host.ts).
//
//   npm run build -w @dilla/web
//   GO=/path/to/go npm run docs:shots -w @dilla/e2e            # or: node e2e/scripts/shoot-docs.mjs
//   node e2e/scripts/shoot-docs.mjs --port 8471 --control 8472 --out docs/user/screenshots
//
// What it does, in order:
//   1. builds target/dilla-testhost (`go build`, $GO or `go`) and starts it on 127.0.0.1:<port> and
//      <control>, with -production-acl and -web-root packages/web/dist, in a temporary data directory;
//   2. (web-3.1) starts from an empty instance: ada signs up with the host's bootstrap invite and lands in the
//      empty shell, then, through the client alone, starts the server "Midgard Crew" (which comes with #general and
//      a voice general), adds the text channel #loot and the voice channel "harbour", and makes an invite link;
//      ada's screens are shot on the way, at 1280×800 and at 360×740, and the rail once on its own;
//   3. signs björn and mira up through that invite link in two more Chromium profiles (Signal theme,
//      1280×800), lets the three talk in #general, and shoots mira's screens;
//   3b. replies, reacts, edits, pins, mentions and shares images and files in #general between the three
//      of them and shoots mira's view of each (the images are drawn by canvas in the page at run time);
//   4. signs mira in from a fourth browser (username and password, then the second factor, then the recovery key, then done), then
//      shoots Devices in both browsers, notifications, badges, a DM and a DM's badge;
//   4b. (web-3) has ada call mira in their DM and shoots the ring; shoots Settings → Voice; has björn share an audio
//      file and shoots its player; has ada, björn and mira join the voice channel "harbour" and shoots the stage and,
//      from #general, the dock; then all three leave;
//   4c. (web-2c and web-3) the native peer's server "Bridge Watch": Privacy, the comparison, search, diagnostics, the
//      peer's changed identity in the conversation, in Privacy and as a refused voice join, and a second tab;
//   4d. (web3.9 adds home-dms, categories, the AFK call and its timeout, the dock, members, the header tooltip and the message reveal: WEB39_SHOTS)
//   4d. (web3.7) shoots the phone shell at 390×844 for getting-started.md's "On your phone": mira's channel, the servers list,
//      the activity tab and, while she is in "harbour", the call bar (PHONE_SHOTS);
//   5. removes mira from the server on ada's authority (the owner's DELETE /v1/communities/{id}/members/{user},
//      through the host's POST /debug/kick, since the web client has no kick yet) and shoots the "You are no longer
//      in ..." banner (shell-removed). This is the one step not done through a client; the ids it needs are read from the host's database;
//   6. closes every browser, stops the host and deletes the temporary directories.
//
// The host runs its in-process LiveKit on 127.0.0.1:7888 (signalling, reached through the host's /rtc proxy) and
// 7890 (ICE/UDP); the browsers use Chromium's fake microphone and camera (e2e/media/support/fixtures.ts).
//
// Every name and message is fictional, and the recovery key shown belongs to an account that is
// deleted with the host's data directory. Not part of CI: nothing runs it but a person.
import { execFileSync, spawn } from 'node:child_process';
import { randomBytes, randomInt } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, rmSync, statSync } from 'node:fs';
import { createInterface } from 'node:readline';
import net from 'node:net';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from '@playwright/test';
import { t } from '../../packages/web/src/strings/index.ts';
import { chromiumMediaArgs, writeFixtures } from '../media/support/fixtures.ts';
import { nextCode } from '../web/support/totp.ts';

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const TESTHOST_BIN = path.join(REPO_ROOT, 'target', 'dilla-testhost');
const WASI_CORE = path.join(REPO_ROOT, 'internal', 'mlswasi', 'testdata', 'dilla_core_wasi.wasm');
const WEB_ROOT = path.join(REPO_ROOT, 'packages', 'web', 'dist');
const WAIT = 60_000;

// L-DOC-23: the two real browser viewports of the guide's images, and the ten web-2c images, each waiting for the
// data-doc-state its visible root carries (packages/web/src/screens: Privacy, Search, Diagnostics, OtherTab).
// L-DOC-24 (web-3): the six voice states of docs/user/voice.md (packages/web/src/screens: voice/VoiceView, voice/Ring,
// settings/Voice, conversation/Attachments).
export const DOC_VIEWPORTS = { desktop: { width: 1280, height: 800 }, phone: { width: 360, height: 740 } };
const DESKTOP = DOC_VIEWPORTS.desktop;
const PHONE = DOC_VIEWPORTS.phone;
// web3.7: the phone shell's images of docs/user/getting-started.md ("On your phone") are shot at 390×844, the size the
// phone shell was designed and tested at, from the same browser as the desktop ones.
export const PHONE_APP = { width: 390, height: 844 };
export const PHONE_SHOTS = ['phone-home', 'phone-channel', 'phone-activity', 'phone-callbar'];
export const DOC_SHOTS = ['trust-privacy', 'trust-compare', 'trust-conversation', 'trust-changed', 'search-results', 'settings-diagnostics', 'tabs-handover',
  'voice-stage', 'voice-dock', 'voice-settings', 'voice-blocked', 'call-ring', 'media-player']
  .flatMap((state) => ['desktop', 'phone'].map((viewport) => ({ name: `${state}-${viewport}`, state: `[data-doc-state="${state}"]`, viewport })))
  // web3.7: a phone has no dock; its call controls are the call bar, shot with the phone set (PHONE_SHOTS).
  .filter((shot) => shot.name !== 'voice-dock-phone');

/**
 * Waits for the named state to be visible, captures it, and closes `context` (when the caller hands one over) in
 * `finally`, so a failed capture never leaves it open. The persistent profiles of main() are not handed over: their
 * owner closes them.
 */
export async function shootNamedState({ context = null, page, state, outFile }) {
  try {
    await page.waitForSelector(state, { state: 'visible', timeout: WAIT });
    await page.screenshot({ path: outFile });
  } finally {
    if (context !== null) await context.close();
  }
}

const SERVER = 'Midgard Crew';
// web-3.1: the channel ada adds after the starter #general, and the voice channel she adds after the starter voice
// general (the voice guide's calls happen in it).
const LOOT = 'loot';
const VOICE = 'harbour';
// web-3.1 (flow 07): ada's create-flow images, each at both viewports, and the rail on its own.
export const CREATE_SHOTS = ['create-empty', 'create-server', 'create-channel', 'create-invite']
  .flatMap((state) => ['desktop', 'phone'].map((viewport) => `${state}-${viewport}`)).concat(['create-rail', 'create-rail-phone']);
// web3.8: the sign-in in its new order (username and password, second factor, recovery key, done), and the desktop images of
// the stage (1440×900), the resizable panes and the link line that replaced the status bar.
export const SIGNIN_SHOTS = ['signin-0-entry', 'signin-1-login', 'signin-2-code', 'signin-3-recovery-key', 'signin-4-done'];
export const WEB38_SHOTS = ['invite-landing-light', 'voice-stage-focused-desktop', 'voice-stage-grid-desktop', 'panes-resized-desktop', 'link-offline-desktop'];
// web3.10: members-desktop shows Björn's profile card open beside the list; dock-desktop shows the channel name over the link stats.
// web3.9: the Discord-style layout. The @ tile and the Home list (desktop and phone), a collapsed category, the built-in
// AFK channel's call and its timeout setting, the dock close up, the two-line member rows, a message revealed by a tap on
// a phone and a header icon's tooltip.
export const WEB39_SHOTS = ['home-dms-desktop', 'home-dms-phone', 'categories-desktop', 'afk-call-desktop', 'afk-settings-desktop',
  'dock-desktop', 'members-desktop', 'message-reveal-phone', 'header-tooltip-desktop'];
// The host's in-process LiveKit: signalling and ICE/UDP (the plan's docs:shots ports).
const SFU = { port: 7888, udpPort: 7890 };
const PEOPLE = {
  ada: { username: 'ada', display: 'Ada' },
  bjorn: { username: 'bjorn', display: 'Björn' },
  mira: { username: 'mira', display: 'Mira' },
};
const WORKSPACE_SETTINGS = {
  profile: 'workspace.profile', signin: 'workspace.signin', devices: 'workspace.devices',
  notifications: 'workspace.notifications', appearance: 'workspace.appearance', voice: 'workspace.voice',
  privacy: 'workspace.privacy', keyboard: 'workspace.keyboard', diagnostics: 'workspace.diagnostics',
};
const WORKSPACE_SERVER_SETTINGS = {
  overview: 'workspace.overview', channels: 'workspace.channels', invites: 'workspace.invites',
};
const MIRA_PASSWORD = 'docs-mira-password';
// [who, body]: the conversation in #general, in order.
const CONVERSATION = [
  ['ada', 'raid tonight at 20:00? bring the good shields'],
  ['bjorn', 'in. last time the bridge troll took everything we had'],
  ['mira', 'I’m in too. Packing list:\nrope\ntorches\nsnacks for Björn'],
  ['ada', 'the map from saturday is in #loot, the one with the coffee stain'],
  ['bjorn', 'see you at the bridge'],
];
// web-2b: what the three say more with, in #general, after the conversation above.
const REPLY = 'ok, 20:00 works, I’ll bring the shields';
const EDITED = 'I’m in too. Packing list:\nrope\ntorches\nsnacks for Björn\nthe good shields';
const MENTION_REST = 'bring the map from #loot, the one with the stain';
const ATTACH_TEXT = 'the map from saturday, coffee stain included';
const MIRA_FILES_TEXT = 'packing list as a file, and the bridge as I remember it';
const MAP = { name: 'saturday-map.png', width: 640, height: 400 };
const BRIDGE = { name: 'bridge-sketch.png', width: 480, height: 320 };
const LOOT_LIST = { name: 'loot-list.txt', mimeType: 'text/plain', text: 'rope\ntorches\nshields\nsnacks for Björn\n' };
const PACKING_LIST = { name: 'packing-list.txt', mimeType: 'text/plain', text: 'rope x2\ntorches x6\nthe good shields\n' };
const SHANTY = { name: 'bridge-shanty.wav', text: 'the shanty from saturday, for the long walk' };

// ---------------------------------------------------------------------------------------------------
// Arguments.

export function parseArgs(argv) {
  const out = { port: 8471, control: 8472, outDir: path.join(REPO_ROOT, 'docs', 'user', 'screenshots') };
  for (let i = 0; i < argv.length; i += 2) {
    const [flag, value] = [argv[i], argv[i + 1]];
    if (value === undefined) throw new Error(`${flag} needs a value`);
    if (flag === '--port' || flag === '--control') {
      const n = Number(value);
      if (!Number.isInteger(n) || n < 1024 || n > 65535) throw new Error(`${flag} must be a port from 1024 to 65535`);
      out[flag.slice(2)] = n;
    } else if (flag === '--out') {
      out.outDir = path.resolve(process.cwd(), value);
    } else if (flag === '--ui-out') {
      out.uiOut = path.resolve(process.cwd(), value);
    } else if (flag === '--fullscreen-settings-out') {
      out.fullscreenSettingsOut = path.resolve(process.cwd(), value);
    } else {
      throw new Error(`unknown flag ${flag}`);
    }
  }
  if (out.port === out.control) throw new Error('--port and --control must differ');
  return out;
}

const hex = (b) => Buffer.from(b).toString('hex');

// ---------------------------------------------------------------------------------------------------
// The test host.

function portFree(port) {
  return new Promise((done) => {
    const s = net.createServer();
    s.once('error', () => done(false));
    s.listen(port, '127.0.0.1', () => s.close(() => done(true)));
  });
}

function stop(child) {
  if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve();
  return new Promise((done) => {
    const kill = setTimeout(() => child.kill('SIGKILL'), 10_000);
    child.once('exit', () => { clearTimeout(kill); done(); });
    child.kill('SIGTERM');
  });
}

function startHost(o, dataDir) {
  const child = spawn(TESTHOST_BIN, [
    '-listen', `127.0.0.1:${o.port}`, '-control', `127.0.0.1:${o.control}`, '-core', WASI_CORE,
    '-log-level', 'warn', '-data-dir', dataDir, '-production-acl', '-web-root', WEB_ROOT,
    '-sfu', '-sfu-port', String(SFU.port), '-sfu-udp-port', String(SFU.udpPort),
  ], { stdio: ['ignore', 'pipe', 'pipe'] });
  const ready = new Promise((resolve, reject) => {
    let seen = '';
    const timer = setTimeout(() => reject(new Error(`dilla-testhost did not start within ${WAIT} ms:\n${seen}`)), WAIT);
    child.stdout.on('data', (b) => {
      seen += b.toString();
      const invite = /DILLA_TESTKIT_INVITE=(\S+)/.exec(seen);
      if (invite) { clearTimeout(timer); resolve(invite[1]); }
    });
    child.stderr.on('data', (b) => { seen += b.toString(); });
    child.once('exit', (code) => { clearTimeout(timer); reject(new Error(`dilla-testhost exited with ${code}:\n${seen}`)); });
  });
  return { child, ready };
}

// ---------------------------------------------------------------------------------------------------
// The native peer (web-2c): the testkit's web-driver, one JSON line per request (e2e/web/support/driver.ts). It is
// the one member whose identity key can change, which a browser cannot do; nothing it holds is printed.

function startPeer(publicUrl, controlUrl, invite) {
  const bin = process.env.DILLA_TESTKIT ?? path.join(REPO_ROOT, 'target', 'release', 'dilla-testkit');
  if (!existsSync(bin)) throw new Error(`dilla-testkit not found at ${bin}: cargo build -p dilla-testkit --release --locked, or set DILLA_TESTKIT`);
  const child = spawn(bin, ['web-driver', '--ds', publicUrl, '--seed', String(randomInt(1, 0xff_ffff))], {
    env: { ...process.env, DILLA_TESTKIT_INVITE: invite, DILLA_TESTKIT_CONTROL: controlUrl }, stdio: ['pipe', 'pipe', 'pipe'],
  });
  const pending = new Map();
  let next = 1;
  createInterface({ input: child.stdout }).on('line', (line) => {
    const answer = JSON.parse(line);
    const p = pending.get(answer.id);
    if (!p) return;
    pending.delete(answer.id);
    if (answer.ok) p.resolve(answer); else p.reject(new Error(`web-driver: ${answer.error}`));
  });
  child.stderr.on('data', (d) => process.stderr.write(d));
  child.on('exit', (code) => {
    for (const p of pending.values()) p.reject(new Error(`web-driver exited with ${code}`));
    pending.clear();
  });
  const request = (op, args = {}) => new Promise((resolve, reject) => {
    const id = next++;
    pending.set(id, { resolve, reject });
    child.stdin.write(`${JSON.stringify({ id, op, ...args })}\n`);
  });
  return { child, request };
}

// ---------------------------------------------------------------------------------------------------
// The removal at the end: the owner's kick, which the web client cannot make yet.

/**
 * Removes `target` from the server `name` on `actor`'s authority: POST /debug/kick with a community runs the production
 * route DELETE /v1/communities/{id}/members/{user} as the actor's device (e2e/web/membership.spec.ts). The browsers
 * never show a full device id, so the ids are read from the host's own database, read only.
 */
async function kickFromServer(controlUrl, dataDir, name, actor, target) {
  // Imported here, so that loading this module for its unit tests needs no node:sqlite.
  const { DatabaseSync } = await import('node:sqlite');
  const db = new DatabaseSync(path.join(dataDir, 'dilla.db'), { readOnly: true });
  let ids;
  try {
    const device = (username) => db.prepare('SELECT d.id AS id FROM devices d JOIN users u ON u.id = d.user_id '
      + 'WHERE u.username = ? AND d.revoked_at IS NULL ORDER BY d.rowid LIMIT 1').get(username);
    const community = db.prepare('SELECT id FROM communities WHERE name = ?').all(name);
    if (community.length !== 1) throw new Error(`the host has ${community.length} servers named ${name}`);
    const [a, b] = [device(actor), device(target)];
    if (a === undefined || b === undefined) throw new Error(`no device of ${a === undefined ? actor : target} in the host's database`);
    ids = { community: hex(community[0].id), actor: hex(a.id), target: hex(b.id) };
  } finally {
    db.close();
  }
  const res = await fetch(`${controlUrl}/debug/kick`, {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(ids),
  });
  if (res.status !== 204) throw new Error(`POST /debug/kick: ${res.status} ${await res.text()}`);
}

// ---------------------------------------------------------------------------------------------------
// The browser side: the onboarding and shell, driven by the copy of packages/web/src/strings/en.ts.

const byText = (page, role, key, vars) => page.getByRole(role, { name: t(key, vars), exact: true });
/** The `@` tile (web3.9), the rail's first item: it carries the DM counts in `data-unread` and its name once a count shows. */
const homeTile = (page) => byText(page, 'navigation', 'shell.rail.label').getByRole('button', { name: new RegExp(`^${t('home.tile')}(,|$)`) });
/** The gear in the user panel at the bottom left (web3.8): the rail has no settings button. */
// Signal: settings opens from the top bar's gear ("Open settings"); the user panel has none.
const gear = (page) => page.getByRole('button', { name: t('workspace.open'), exact: true }).first();
const composer = (page, channel) => page.getByRole('textbox', { name: t('shell.composer.label', { channel }), exact: true });
const row = (page, channel, text) => page.getByRole('log', { name: t('shell.log.label', { channel }), exact: true })
  .locator('.d-message-row').filter({ hasText: text });

/** Fonts loaded, animations ended, no hover and no focus ring left over from the pointer or a filled field. */
async function settle(page, { keepFocus = false } = {}) {
  // Park the pointer outside the page: a corner inside it would hover the phone's "you" tab.
  await page.mouse.move(-10, -10);
  await page.evaluate(async (keep) => {
    if (!keep && document.activeElement instanceof HTMLElement) document.activeElement.blur();
    await document.fonts.ready;
    await Promise.all(document.getAnimations()
      .filter((a) => Number.isFinite(a.effect?.getComputedTiming().endTime))
      .map((a) => a.finished.catch(() => undefined)));
  }, keepFocus);
}

// web3.6: every account sets up an authenticator app. The setup key of each person is kept for the sign-in shots;
// the codes are computed for the test host's clock (control URL set by main()).
let controlBase = '';
const authenticators = new Map();

async function onboard(page, person, invite, shots) {
  const shot = async (name) => { if (shots) { await settle(page); await shots(name); } };
  await page.goto(`/welcome?invite=${encodeURIComponent(invite)}`);
  await page.locator('.d-onboarding-frame h1').waitFor({ timeout: WAIT });
  await shot('onboarding-1-invite');
  await byText(page, 'button', 'onboarding.next').click();
  await byText(page, 'heading', 'onboarding.identity.title').waitFor({ timeout: WAIT });
  await page.getByLabel(t('onboarding.identity.username'), { exact: true }).fill(person.username);
  await page.getByLabel(t('onboarding.identity.display'), { exact: true }).fill(person.display);
  const password = page.getByLabel(t('onboarding.identity.password'), { exact: true });
  if (await password.count() > 0) await password.fill(person.username === 'mira' ? MIRA_PASSWORD : `docs-${hex(randomBytes(8))}`);
  await shot('onboarding-2-name');
  await byText(page, 'button', 'onboarding.next').click();
  await byText(page, 'heading', 'onboarding.keys.title').waitFor({ timeout: WAIT });
  await page.locator('.d-recovery-key li').first().waitFor({ timeout: WAIT });
  const recoveryKey = person.username === 'mira' ? (await page.locator('.d-recovery-key li').allTextContents()).join(' ') : null;
  await shot('onboarding-3-recovery-key');
  await byText(page, 'checkbox', 'onboarding.keys.acknowledge').check();
  await byText(page, 'button', 'onboarding.next').click();
  await byText(page, 'heading', 'onboarding.browser.title').waitFor({ timeout: WAIT });
  await shot('onboarding-4-browser');
  await byText(page, 'button', 'onboarding.browser.submit').click();
  await byText(page, 'heading', 'factor.choose.title').waitFor({ timeout: WAIT });
  await shot('onboarding-5-factor-choose');
  await page.getByRole('radio', { name: new RegExp(t('factor.choose.app')) }).check();
  await byText(page, 'button', 'factor.choose.next').click();
  await byText(page, 'heading', 'factor.totp.title').waitFor({ timeout: WAIT });
  const keyField = page.getByLabel(t('factor.totp.key'), { exact: true });
  await page.waitForFunction(() => /^[A-Z2-7]{16,}$/.test(document.querySelector('#factor-totp-key')?.value ?? ''), null, { timeout: WAIT });
  const authenticator = { totpSecret: await keyField.inputValue(), lastCounter: -1 };
  authenticators.set(person.username, authenticator);
  await shot('onboarding-5-factor-app');
  await page.getByLabel(t('factor.totp.code'), { exact: true }).fill(await nextCode(controlBase, authenticator));
  await byText(page, 'button', 'factor.totp.submit').click();
  await byText(page, 'heading', 'factor.codes.title').waitFor({ timeout: WAIT });
  await shot('onboarding-5-factor-codes');
  await byText(page, 'checkbox', 'factor.codes.ack').check();
  await byText(page, 'button', 'factor.codes.next').click();
  await byText(page, 'heading', 'onboarding.done.title').waitFor({ timeout: WAIT });
  await shot('onboarding-5-done');
  await byText(page, 'button', 'onboarding.done.next').click();
  await byText(page, 'navigation', 'shell.rail.label').waitFor({ timeout: WAIT });
  return recoveryKey;
}

async function send(page, channel, text) {
  const box = composer(page, channel);
  // A composer that is joining or catching up is read-only (aria-disabled), not disabled.
  await box.and(page.locator(':not([readonly])')).waitFor({ timeout: WAIT });
  await box.fill(text);
  await box.press('Enter');
  // A send that raced a member's join is refused at its stale epoch and shown "not sent" (by design,
  // L-TS-07); the person retries it, and so does this script, at most three times.
  const target = row(page, channel, text.split('\n')[0]);
  for (let attempt = 0; attempt < 3; attempt++) {
    await target.and(page.locator('[data-state="ok"], [data-state="failed"]')).waitFor({ timeout: WAIT });
    if (await target.getAttribute('data-state') !== 'failed') break;
    process.stdout.write(`shoot-docs: retrying a refused send (${(await target.innerText()).replace(/\s+/g, ' ')})\n`);
    await target.getByRole('button', { name: t('shell.message.retry'), exact: true }).click();
    await target.and(page.locator('[data-state="failed"]')).waitFor({ state: 'detached', timeout: WAIT });
  }
  await confirmed(page, channel, text);
}

/** Waits until the row of `text` is shown confirmed; on a timeout, says what the log showed instead. */
async function confirmed(page, channel, text) {
  const target = row(page, channel, text.split('\n')[0]);
  try {
    await target.and(page.locator('[data-state="ok"]')).waitFor({ timeout: WAIT });
  } catch (e) {
    const rows = await page.locator('.d-message-row').evaluateAll((els) => els.map((el) => `${el.getAttribute('data-state')}: ${el.textContent}`));
    const main = await page.locator('main').innerText().catch(() => '');
    throw new Error(`${JSON.stringify(text)} was not confirmed in #${channel}; rows: ${JSON.stringify(rows)}; main: ${JSON.stringify(main)}`, { cause: e });
  }
}

const byId = (page, msgId) => page.locator(`.d-message-row[data-msg-id="${msgId}"]`);

async function idOf(page, channel, text) {
  const target = row(page, channel, text).and(page.locator('[data-state="ok"]'));
  await target.waitFor({ timeout: WAIT });
  const id = await target.getAttribute('data-msg-id');
  if (!id || !/^[0-9a-f]{32}$/.test(id)) throw new Error(`no msg id on ${JSON.stringify(text)}`);
  return id;
}

async function toolbarAction(page, msgId, key) {
  await byId(page, msgId).focus();
  await byId(page, msgId).hover();
  const toolbar = byId(page, msgId).getByRole('toolbar', { name: t('shell.message.toolbar'), exact: true });
  // Task 7 (Ruling U): edit, pin, unpin and delete are in the "more" menu; react, reply and thread are icon buttons.
  if (['shell.message.edit', 'shell.message.pin', 'shell.message.unpin', 'shell.message.delete'].includes(key)) {
    await toolbar.getByRole('button', { name: t('shell.message.more'), exact: true }).click();
    await toolbar.getByRole('menuitem', { name: t(key), exact: true }).click();
    return;
  }
  await toolbar.getByRole('button', { name: t(key), exact: true }).click();
}

async function pickEmoji(page, nameKey) {
  const dialog = page.getByRole('dialog', { name: t('shell.emoji.label'), exact: true });
  await dialog.getByRole('button', { name: t(nameKey), exact: true }).click();
  await dialog.waitFor({ state: 'hidden', timeout: WAIT });
}

async function drawPng(page, spec) {
  const bytes = await page.evaluate(async ({ kind, width, height }) => {
    const canvas = new OffscreenCanvas(width, height);
    const c = canvas.getContext('2d');
    if (kind === 'map') {
      c.fillStyle = '#e8dcc0'; c.fillRect(0, 0, width, height);
      c.strokeStyle = '#4a7fa8'; c.lineWidth = 18;
      c.beginPath(); c.moveTo(0, 260); c.bezierCurveTo(160, 180, 320, 340, 640, 220); c.stroke();
      c.fillStyle = '#6b4f2a'; c.fillRect(296, 222, 48, 64);
      c.strokeStyle = 'rgba(111, 78, 55, 0.55)'; c.lineWidth = 8;
      c.beginPath(); c.arc(150, 110, 46, 0, 2 * Math.PI); c.stroke();
      c.strokeStyle = '#a8323a'; c.lineWidth = 10;
      c.beginPath(); c.moveTo(470, 90); c.lineTo(510, 130); c.moveTo(510, 90); c.lineTo(470, 130); c.stroke();
    } else if (kind === 'bridge') {
      c.fillStyle = '#f4f1ea'; c.fillRect(0, 0, width, height);
      c.strokeStyle = '#3b3b3b'; c.lineWidth = 6;
      c.beginPath(); c.arc(240, 320, 180, Math.PI, 2 * Math.PI); c.stroke();
      c.beginPath(); c.moveTo(40, 150); c.lineTo(440, 150); c.stroke();
      for (const x of [80, 160, 240, 320, 400]) {
        c.beginPath(); c.moveTo(x, 150); c.lineTo(x, 200); c.stroke();
      }
    } else throw new Error(`unknown drawing ${kind}`);
    const blob = await canvas.convertToBlob({ type: 'image/png' });
    return Array.from(new Uint8Array(await blob.arrayBuffer()));
  }, spec);
  return Buffer.from(bytes);
}

async function attach(page, file) {
  const [chooser] = await Promise.all([
    page.waitForEvent('filechooser', { timeout: WAIT }),
    page.getByRole('button', { name: t('shell.composer.attach'), exact: true }).click(),
  ]);
  await chooser.setFiles(file);
}

async function trayReady(page, n) {
  const list = page.getByRole('list', { name: t('shell.tray.label'), exact: true });
  await list.locator('li[data-phase="ready"]').nth(n - 1).waitFor({ timeout: WAIT });
  if (await list.locator('li[data-phase="failed"]').count()) {
    throw new Error('a tray entry failed: ' + await list.innerText());
  }
}

async function focusRow(page, channel, msgId) {
  const active = page.getByRole('log', { name: t('shell.log.label', { channel }), exact: true })
    .locator('article.d-message-row[tabindex="0"]');
  await active.focus();
  await active.press('End');
  for (let i = 0; i < 30; i++) {
    if (await byId(page, msgId).evaluate((el) => el === document.activeElement)) return;
    await page.keyboard.press('ArrowUp');
  }
  throw new Error('could not move to ' + msgId);
}

/** A 2 s, 48 kHz, 16-bit mono WAV of a short tune, made here (no audio file ships with the script). */
function shantyWav() {
  const rate = 48_000;
  const notes = [392, 440, 494, 523, 494, 440, 392, 330];
  const samples = rate * 2;
  const per = samples / notes.length;
  const b = Buffer.alloc(44 + samples * 2);
  b.write('RIFF', 0); b.writeUInt32LE(36 + samples * 2, 4); b.write('WAVE', 8);
  b.write('fmt ', 12); b.writeUInt32LE(16, 16); b.writeUInt16LE(1, 20); b.writeUInt16LE(1, 22);
  b.writeUInt32LE(rate, 24); b.writeUInt32LE(rate * 2, 28); b.writeUInt16LE(2, 32); b.writeUInt16LE(16, 34);
  b.write('data', 36); b.writeUInt32LE(samples * 2, 40);
  for (let i = 0; i < samples; i++) {
    const at = i % per;
    const fade = Math.min(1, at / 480, (per - at) / 480);
    b.writeInt16LE(Math.round(Math.sin((2 * Math.PI * notes[Math.floor(i / per)] * i) / rate) * 0.3 * fade * 32767), 44 + i * 2);
  }
  return b;
}

// The voice page contract, as e2e/voice/support/voice.ts reads it.
const railServer = (page, name) => byText(page, 'navigation', 'shell.rail.label').getByRole('button', { name: new RegExp(`^${name}(, |$)`) });
const voiceRow = (page, name) => page.getByRole('button', { name: t('voice.open', { name }), exact: true }).first();
const stage = (page) => page.locator('.dw-voice-stage');
const dock = (page) => page.locator('.dw-voice-dock');

/** The call phase is `in` and the dock holds a connected session (Signal: the dock hides while the stage is on screen). */
async function connected(page) {
  await page.locator('[data-call-phase="in"]').first().waitFor({ timeout: WAIT });
  await dock(page).locator('.d-voice-dock[data-phase="connected"]').waitFor({ state: 'attached', timeout: WAIT });
}

/** Clicks the voice channel `name` of the server `server` (one click joins it) and waits for a connected call. */
async function joinVoice(page, server, name) {
  await railServer(page, server).click();
  // web3.9: one click on a voice channel joins it on a computer; the stage has no join button to press first.
  await voiceRow(page, name).click();
  await connected(page);
}

/**
 * Leaves through the one `leave` on screen and waits until the dock is gone. While the call's stage is open the dock shows
 * no controls (web3.7), so the button is the stage's; from another channel it is the dock's.
 */
async function leaveVoice(page) {
  // Signal: the stage's leave is its [data-control="leave"] ("Leave"); without the stage, the dock's "leave".
  const stageLeave = page.locator('.dw-voice-stage [data-control="leave"]');
  if (await stageLeave.isVisible().catch(() => false)) await stageLeave.click();
  else await page.locator('.dw-voice-dock').getByRole('button', { name: t('voice.leave'), exact: true }).click();
  await dock(page).waitFor({ state: 'detached', timeout: WAIT });
}

async function main(argv) {
  const o = parseArgs(argv);
  const publicUrl = `http://127.0.0.1:${o.port}`;
  const controlUrl = `http://127.0.0.1:${o.control}`;
  controlBase = controlUrl;
  if (!existsSync(path.join(WEB_ROOT, 'index.html'))) throw new Error(`${WEB_ROOT} has no index.html: npm run build -w @dilla/web first`);
  if (!existsSync(WASI_CORE)) throw new Error(`${WASI_CORE} is missing: cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked, then copy it there`);
  for (const p of [o.port, o.control, SFU.port]) {
    if (!(await portFree(p))) throw new Error(`127.0.0.1:${p} is in use: pick others with --port and --control`);
  }
  execFileSync(process.env.GO ?? 'go', ['-C', REPO_ROOT, 'build', '-o', TESTHOST_BIN, './cmd/dilla-testhost'], { stdio: 'inherit' });

  const scratch = mkdtempSync(path.join(tmpdir(), 'dilla-docs-shots-'));
  const fixtures = writeFixtures(path.join(scratch, 'media'));
  const host = startHost(o, path.join(scratch, 'host'));
  const contexts = [];
  const problems = [];
  const written = [];
  let peer = null;
  try {
    // The host prints its instance invites comma-separated; the first is the bootstrap invite an operator hands on.
    const instanceInvite = await host.ready;
    const bootstrapInvite = instanceInvite.split(',')[0];
    mkdirSync(o.outDir, { recursive: true });

    const open = async (who) => {
      const context = await chromium.launchPersistentContext(path.join(scratch, `profile-${who}`), {
        headless: true, viewport: DESKTOP, deviceScaleFactor: 1, colorScheme: 'dark', reducedMotion: 'reduce',
        locale: 'en-GB', timezoneId: 'Europe/Stockholm', baseURL: publicUrl,
        // web-3: Chromium's fake microphone and camera (a steady tone, a moving gradient), granted without a prompt.
        // web-3.1: the clipboard too, so the invite dialog's own copy goes through ("Invite link copied.").
        args: chromiumMediaArgs(fixtures), permissions: ['microphone', 'camera', 'clipboard-read', 'clipboard-write'],
      });
      contexts.push(context);
      // Headless Chromium denies notifications in a persistent profile even after CDP sets "prompt".
      // Show the opt-in state of the real Settings screen; no notification is requested or sent here.
      if (who === 'mira') await context.addInitScript(() => {
        if (window.Notification) Object.defineProperty(window.Notification, 'permission', {
          configurable: true, get: () => 'default',
        });
      });
      context.on('weberror', (e) => problems.push(`${who}: ${String(e.error())}`));
      const page = context.pages()[0] ?? (await context.newPage());
      return page;
    };
    const pages = {};
    const shotsOf = (page) => async (name) => {
      const file = path.join(o.outDir, `${name}.png`);
      await page.screenshot({ path: file });
      written.push(file);
    };

    // web-3.1 (flow 07): the instance is empty. Ada signs up with the bootstrap invite and does the rest through the
    // client: the server, its channels and the invite link everyone else signs up with.
    const ada = pages.ada = await open('ada');
    const shootAda = shotsOf(ada);
    // Each create-flow image at 1280×800 and at 360×740, with focus kept where the person left it.
    const shootBoth = async (name) => {
      for (const [viewport, size] of [['desktop', DESKTOP], ['phone', PHONE]]) {
        await ada.setViewportSize(size);
        await settle(ada, { keepFocus: true });
        await shootAda(`${name}-${viewport}`);
      }
      await ada.setViewportSize(DESKTOP);
    };
    await onboard(ada, PEOPLE.ada, bootstrapInvite, null);
    const empty = ada.getByRole('region', { name: t('shell.noServer.title'), exact: true });
    await empty.waitFor({ timeout: WAIT });
    await ada.evaluate(() => { if (document.activeElement instanceof HTMLElement) document.activeElement.blur(); });
    await shootBoth('create-empty');

    // "new server" in the rail: the dialog, with the name typed, then Enter.
    const rail = byText(ada, 'navigation', 'shell.rail.label');
    await rail.getByRole('button', { name: t('layout.rail.add'), exact: true }).click();
    await byText(ada, 'dialog', 'layout.add.title').getByRole('button', { name: t('layout.add.create'), exact: true }).click();
    const startDialog = byText(ada, 'dialog', 'create.server.title');
    const serverName = startDialog.getByRole('textbox', { name: t('create.server.name'), exact: true });
    await serverName.and(ada.locator(':focus')).waitFor({ timeout: WAIT });
    await serverName.pressSequentially(SERVER);
    await shootBoth('create-server');
    await serverName.press('Enter');
    await startDialog.waitFor({ state: 'hidden', timeout: WAIT });
    // #general opens with the focus in its composer; Ada's browser is the first in it and so creates its group.
    await composer(ada, 'general').and(ada.locator(':enabled')).waitFor({ timeout: WAIT });
    await ada.getByRole('button', { name: t('voice.open', { name: 'general' }), exact: true }).waitFor({ timeout: WAIT });
    // The rail up close: the @ tile, the server's tile and the one "+" tile, beside the server's name and its actions;
    // cut off below the "+" tile (the settings gear lives in the user panel since web3.8).
    await settle(ada);
    const railFile = path.join(o.outDir, 'create-rail.png');
    const newBox = await byText(ada, 'button', 'layout.rail.add').boundingBox();
    if (newBox === null) throw new Error('the rail has no add tile on screen');
    await ada.screenshot({ path: railFile, clip: { x: 0, y: 0, width: 300, height: Math.ceil(newBox.y + newBox.height + 12) } });
    written.push(railFile);
    // web3.9: the same top of the channel list at 360 wide, from the phone's home tab (back out of the channel first).
    await ada.setViewportSize(PHONE);
    await ada.getByRole('button', { name: t('nav.back'), exact: true }).click();
    await ada.getByRole('button', { name: t('create.channel.open'), exact: true }).waitFor({ timeout: WAIT });
    await settle(ada);
    const railPhoneFile = path.join(o.outDir, 'create-rail-phone.png');
    await ada.screenshot({ path: railPhoneFile, clip: { x: 0, y: 0, width: PHONE.width, height: 240 } });
    written.push(railPhoneFile);
    await ada.getByRole('button', { name: 'general', exact: true }).first().click();
    await composer(ada, 'general').and(ada.locator(':enabled')).waitFor({ timeout: WAIT });
    await ada.setViewportSize(DESKTOP);

    // "new channel" (the "+" in the channel list's header, for the owner): #loot, then the voice channel "harbour".
    const addChannel = async (kind, name, shotName) => {
      await ada.getByRole('navigation', { name: t('shell.channels.label'), exact: true })
        .getByRole('button', { name: t('create.channel.open'), exact: true }).click();
      const dialog = byText(ada, 'dialog', 'create.channel.title', { server: SERVER });
      const field = dialog.getByRole('textbox', { name: t('create.channel.name'), exact: true });
      await field.and(ada.locator(':focus')).waitFor({ timeout: WAIT });
      if (kind === 'voice') {
        await dialog.getByRole('radio', { name: t('create.channel.voice'), exact: true }).click();
        await field.focus();
      }
      await field.pressSequentially(name);
      if (shotName !== null) await shootBoth(shotName);
      await field.press('Enter');
      await dialog.waitFor({ state: 'hidden', timeout: WAIT });
    };
    await addChannel('text', LOOT, 'create-channel');
    await composer(ada, LOOT).and(ada.locator(':enabled')).waitFor({ timeout: WAIT });
    await addChannel('voice', VOICE, null);
    await ada.getByRole('button', { name: t('voice.open', { name: VOICE }), exact: true }).waitFor({ timeout: WAIT });
    await ada.getByRole('button', { name: 'general', exact: true }).click();
    await composer(ada, 'general').and(ada.locator(':enabled')).waitFor({ timeout: WAIT });

    // web3.9: Server settings → overview holds the inactive timeout of the built-in AFK channel (there is no picker), then
    // channels → new category: "Raids" holds #loot and "Boats" holds the voice channel harbour; Boats is collapsed for the shot.
    await byText(ada, 'button', 'admin.open').click().then(() => byText(ada, 'menuitem', 'signalSettings.menu.serverSettings').click());
    const prepDialog = byText(ada, 'dialog', 'admin.title');
    const prepNav = prepDialog.getByRole('navigation', { name: t('admin.nav'), exact: true });
    await prepNav.getByRole('button', { name: t(WORKSPACE_SERVER_SETTINGS.overview), exact: true }).click();
    await prepDialog.getByRole('radiogroup', { name: t('admin.overview.afk'), exact: true }).waitFor({ timeout: WAIT });
    await settle(ada);
    await shootAda('afk-settings-desktop');
    await prepNav.getByRole('button', { name: t(WORKSPACE_SERVER_SETTINGS.channels), exact: true }).click();
    for (const category of ['Raids', 'Boats']) {
      await prepDialog.getByRole('button', { name: t('admin.channels.newCategory'), exact: true }).click();
      const categoryDialog = byText(ada, 'dialog', 'admin.category.title', { server: SERVER });
      const categoryName = categoryDialog.getByRole('textbox', { name: t('admin.category.name'), exact: true });
      await categoryName.fill(category);
      await categoryName.press('Enter');
      await categoryDialog.waitFor({ state: 'hidden', timeout: WAIT });
    }
    for (const [channel, category, key] of [[LOOT, 'Raids', 'admin.channel.titleText'], [VOICE, 'Boats', 'admin.channel.titleVoice']]) {
      await prepDialog.getByRole('button', { name: t('admin.channels.editLabel', { name: channel }), exact: true }).click();
      const editDialog = byText(ada, 'dialog', key, { name: channel });
      await editDialog.getByLabel(t('admin.channel.category'), { exact: true }).selectOption({ label: category });
      await editDialog.getByRole('button', { name: t('admin.channel.save'), exact: true }).click();
      await editDialog.waitFor({ state: 'hidden', timeout: WAIT });
    }
    await ada.keyboard.press('Escape');
    await prepDialog.waitFor({ state: 'hidden', timeout: WAIT });
    const adaChannels = ada.getByRole('navigation', { name: t('shell.channels.label'), exact: true });
    const boats = adaChannels.getByRole('button', { name: /^Boats/ });
    await boats.click();
    await voiceRow(ada, VOICE).waitFor({ state: 'hidden', timeout: WAIT });
    await settle(ada);
    await shootAda('categories-desktop');
    await boats.click();
    await voiceRow(ada, VOICE).waitFor({ timeout: WAIT });

    // Server settings → Invites → "Make link": the dialog makes the link and copies it; the link is read from its field,
    // never the clipboard.
    await byText(ada, 'button', 'admin.open').click().then(() => byText(ada, 'menuitem', 'signalSettings.menu.serverSettings').click());
    const settingsDialog = byText(ada, 'dialog', 'admin.title');
    await settingsDialog.getByRole('navigation', { name: t('admin.nav'), exact: true })
      .getByRole('button', { name: t(WORKSPACE_SERVER_SETTINGS.invites), exact: true }).click();
    // Signal task 10c: the link shows once in the Invites card, with Copy (no dialog).
    await settingsDialog.getByRole('button', { name: t('serverSettings.invites.make'), exact: true }).click();
    const inviteDialog = settingsDialog;
    const linkField = inviteDialog.getByRole('textbox', { name: t('serverSettings.invites.link'), exact: true });
    await linkField.waitFor({ timeout: WAIT });
    const inviteLink = await linkField.inputValue();
    const invitePath = new URL(inviteLink).pathname;
    if (!/^\/i\/[^/]+$/.test(invitePath)) throw new Error(`the invite dialog showed ${JSON.stringify(inviteLink)}, not an /i/ link`);
    const invite = decodeURIComponent(invitePath.slice(3));
    await shootBoth('create-invite');
    await ada.keyboard.press('Escape');
    await settingsDialog.waitFor({ state: 'hidden', timeout: WAIT });
    const missing = CREATE_SHOTS.filter((name) => !written.includes(path.join(o.outDir, `${name}.png`)));
    if (missing.length > 0) throw new Error(`create-flow images not shot: ${missing.join(', ')}`);

    // Björn signs up through Ada's link and lands in the server.
    pages.bjorn = await open('bjorn');
    await onboard(pages.bjorn, PEOPLE.bjorn, invite, null);
    await composer(pages.bjorn, 'general').and(pages.bjorn.locator(':enabled')).waitFor({ timeout: WAIT });

    // Mira is the camera: the invite landing page (the link's own path: it names the instance's public address, and
    // this host serves it on loopback), the five onboarding steps, then the shell.
    const mira = pages.mira = await open('mira');
    const shoot = shotsOf(mira);
    await mira.goto(invitePath);
    await settle(mira);
    await shoot('invite-landing');
    // web3.8: the styled invite page follows the browser's colour scheme; the light one is shot as well.
    await mira.emulateMedia({ colorScheme: 'light' });
    await settle(mira);
    await shoot('invite-landing-light');
    await mira.emulateMedia({ colorScheme: 'dark' });
    await settle(mira);
    const miraKey = await onboard(mira, PEOPLE.mira, invite, shoot);
    await composer(mira, 'general').and(mira.locator(':enabled')).waitFor({ timeout: WAIT });
    // Let Ada's and Björn's tabs apply Mira's join before the first message (fewer stale-epoch retries).
    await mira.waitForTimeout(3000);

    for (const [who, body] of CONVERSATION) {
      await send(pages[who], 'general', body);
      await confirmed(mira, 'general', body);
    }
    await composer(mira, 'general').fill('');
    await composer(mira, 'general').focus();
    await settle(mira, { keepFocus: true });
    await shoot('shell-conversation');
    // web3.8: the channel list dragged 96 px wider by its border, then reset with a double-click. Done here, from the
    // first desktop shell: a shell that went through the phone layout and back has measured itself again.
    const splitter = mira.getByRole('separator', { name: t('pane.resize.sidebar'), exact: true });
    const splitBox = await splitter.boundingBox();
    if (splitBox === null) throw new Error('the channel list border has no box');
    const splitX = splitBox.x + splitBox.width / 2;
    const splitY = splitBox.y + splitBox.height / 2;
    await mira.mouse.move(splitX, splitY);
    await mira.mouse.down();
    await mira.mouse.move(splitX + 48, splitY, { steps: 5 });
    await mira.mouse.move(splitX + 96, splitY, { steps: 5 });
    await mira.mouse.up();
    await mira.waitForTimeout(300);
    await settle(mira);
    await shoot('panes-resized-desktop');
    await splitter.dblclick();
    await settle(mira);

    await mira.setViewportSize(PHONE);
    await settle(mira, { keepFocus: true });
    await shoot('shell-phone');
    await mira.setViewportSize(DESKTOP);

    // Saying more in #general: the same message ids are used across all three browsers.
    const raidId = await idOf(pages.bjorn, 'general', 'raid tonight');
    const packId = await idOf(mira, 'general', 'Packing list');

    await toolbarAction(pages.bjorn, raidId, 'shell.message.reply');
    await pages.bjorn.getByRole('button', { name: t('shell.composer.replyCancel'), exact: true })
      .waitFor({ timeout: WAIT });
    await send(pages.bjorn, 'general', REPLY);
    const replyId = await idOf(pages.bjorn, 'general', 'I’ll bring the shields');
    await confirmed(mira, 'general', REPLY);
    await byId(mira, replyId).locator('.d-message-row__reply')
      .getByRole('button', { name: 'raid tonight' }).waitFor({ timeout: WAIT });

    await toolbarAction(pages.ada, packId, 'shell.message.react');
    await pickEmoji(pages.ada, 'shell.emoji.01');
    await byId(pages.ada, packId).locator('button[data-emoji="👍"][aria-pressed="true"]')
      .waitFor({ timeout: WAIT });
    await byId(pages.bjorn, packId).focus();
    await byId(pages.bjorn, packId).hover();
    await byId(pages.bjorn, packId).locator('button[data-emoji="👍"]').click();
    await byId(pages.bjorn, packId).locator('button[data-emoji="👍"][aria-pressed="true"]')
      .waitFor({ timeout: WAIT });
    await toolbarAction(pages.bjorn, packId, 'shell.message.react');
    await pickEmoji(pages.bjorn, 'shell.emoji.10');
    await byId(pages.bjorn, packId).locator('button[data-emoji="🔥"][aria-pressed="true"]')
      .waitFor({ timeout: WAIT });
    for (const [key, count] of [['shell.emoji.01', 2], ['shell.emoji.10', 1]]) {
      await byId(mira, packId).getByRole('button', {
        name: t('shell.message.reaction', { name: t(key), count }), exact: true,
      }).waitFor({ timeout: WAIT });
    }

    await focusRow(mira, 'general', replyId);
    await byId(mira, replyId).getByRole('toolbar', { name: t('shell.message.toolbar'), exact: true })
      .waitFor({ timeout: WAIT });
    await settle(mira, { keepFocus: true });
    await shoot('conversation-actions');
    await mira.keyboard.press('Escape');

    await composer(mira, 'general').fill('');
    await composer(mira, 'general').focus();
    await composer(mira, 'general').press('ArrowUp');
    const editor = mira.getByRole('textbox', { name: t('shell.edit.label'), exact: true });
    await editor.and(mira.locator(':focus')).waitFor({ timeout: WAIT });
    await editor.fill(EDITED);
    await settle(mira, { keepFocus: true });
    await shoot('conversation-edit');
    await editor.press('Enter');
    await byId(mira, packId).and(mira.locator('[data-edited="true"][data-state="ok"]'))
      .waitFor({ timeout: WAIT });
    await byId(mira, packId).getByText('the good shields', { exact: false }).waitFor({ timeout: WAIT });
    await byId(pages.ada, packId).and(pages.ada.locator('[data-edited="true"]'))
      .waitFor({ timeout: WAIT });

    await toolbarAction(pages.ada, packId, 'shell.message.pin');
    await byId(mira, packId).and(mira.locator('[data-pinned="true"]')).waitFor({ timeout: WAIT });
    // Signal (Ruling X): pins are in the channel row's context menu, not a header button.
    await mira.getByRole('navigation', { name: t('shell.channels.label'), exact: true })
      .getByRole('button', { name: /^general(,|$)/ }).first().click({ button: 'right' });
    await mira.getByRole('menu', { name: t('nav.channel.menuOf', { name: 'general' }), exact: true })
      .getByRole('menuitem', { name: t('shell.pins.open'), exact: true }).click();
    const pins = mira.getByRole('dialog', { name: t('shell.pins.title', { channel: 'general' }), exact: true });
    await pins.getByText(t('shell.pins.by', { name: PEOPLE.ada.display }), { exact: true })
      .waitFor({ timeout: WAIT });
    await settle(mira, { keepFocus: true });
    await shoot('conversation-pins');
    await mira.keyboard.press('Escape');
    await pins.waitFor({ state: 'hidden', timeout: WAIT });

    const mentionBox = composer(mira, 'general');
    await mentionBox.fill('');
    await mentionBox.pressSequentially('@ad');
    const mentions = mira.getByRole('listbox', { name: t('shell.composer.mentions'), exact: true });
    await mentions.getByRole('option').filter({ hasText: PEOPLE.ada.display }).waitFor({ timeout: WAIT });
    await settle(mira, { keepFocus: true });
    await shoot('conversation-mention');
    await mentionBox.press('Enter');
    await mentions.waitFor({ state: 'hidden', timeout: WAIT });
    const mentionValue = await mentionBox.inputValue();
    if (mentionValue !== '@ada ') throw new Error('the mention was not inserted: ' + JSON.stringify(mentionValue));
    await mentionBox.pressSequentially(MENTION_REST);
    await mentionBox.press('Enter');
    await confirmed(mira, 'general', MENTION_REST);
    await row(pages.ada, 'general', MENTION_REST).and(pages.ada.locator('[data-mention="true"]'))
      .waitFor({ timeout: WAIT });

    const mapPng = await drawPng(pages.bjorn, { kind: 'map', ...MAP });
    await attach(pages.bjorn, { name: MAP.name, mimeType: 'image/png', buffer: mapPng });
    await attach(pages.bjorn, { name: LOOT_LIST.name, mimeType: LOOT_LIST.mimeType,
      buffer: Buffer.from(LOOT_LIST.text, 'utf8') });
    await trayReady(pages.bjorn, 2);
    await send(pages.bjorn, 'general', ATTACH_TEXT);
    await confirmed(mira, 'general', ATTACH_TEXT);
    await mira.locator(`img[alt="${MAP.name}"][data-thumb="ready"]`).waitFor({ timeout: WAIT });
    await mira.getByRole('button', { name: t('shell.attachment.save', { name: LOOT_LIST.name }), exact: true })
      .waitFor({ timeout: WAIT });

    await attach(mira, { name: PACKING_LIST.name, mimeType: PACKING_LIST.mimeType,
      buffer: Buffer.from(PACKING_LIST.text, 'utf8') });
    await attach(mira, { name: BRIDGE.name, mimeType: 'image/png',
      buffer: await drawPng(mira, { kind: 'bridge', ...BRIDGE }) });
    await trayReady(mira, 2);
    await composer(mira, 'general').focus();
    await settle(mira, { keepFocus: true });
    await shoot('conversation-attachments');
    await send(mira, 'general', MIRA_FILES_TEXT);
    await mira.locator(`img[alt="${BRIDGE.name}"][data-thumb="ready"]`).waitFor({ timeout: WAIT });

    // The log is still settling after the second image loads, and a click that lands mid-scroll opens nothing: press
    // again, from a still page, until the lightbox shows.
    const openMap = mira.getByRole('button', { name: t('shell.attachment.open', { name: MAP.name }), exact: true });
    const lightbox = mira.locator('dialog[open]').filter({ has: mira.locator(`img[alt="${MAP.name}"]`) });
    for (let attempt = 0; attempt < 4 && !(await lightbox.isVisible()); attempt++) {
      await openMap.scrollIntoViewIfNeeded();
      await mira.waitForTimeout(1500);
      await openMap.click();
      await lightbox.waitFor({ timeout: 4000 }).catch(() => undefined);
    }
    await lightbox.waitFor({ timeout: WAIT });
    await mira.waitForFunction((alt) => {
      const i = document.querySelector(`dialog[open] img[alt="${alt}"]`);
      return i instanceof HTMLImageElement && i.complete && i.naturalWidth > 0;
    }, MAP.name, { timeout: WAIT });
    await settle(mira, { keepFocus: true });
    await shoot('conversation-lightbox');
    await mira.keyboard.press('Escape');
    await lightbox.waitFor({ state: 'hidden', timeout: WAIT });
    await composer(mira, 'general').fill('');

    // Sign-in ceremony in a clean fourth browser, in the shipped order (web3.8, R6): username and password, then the
    // second factor, then the recovery key, then done. Nothing is registered until the key step.
    const second = await open('mira-second');
    const shootSecond = shotsOf(second);
    await second.goto('/welcome');
    await byText(second, 'button', 'onboarding.connect.signIn').waitFor({ timeout: WAIT });
    await settle(second);
    await shootSecond('signin-0-entry');
    await byText(second, 'button', 'onboarding.connect.signIn').click();
    await second.getByRole('heading', { name: /^Sign in to / }).waitFor({ timeout: WAIT });
    await second.getByLabel(t('signin.login.username'), { exact: true }).fill(PEOPLE.mira.username);
    await second.getByLabel(t('signin.login.password'), { exact: true }).fill(MIRA_PASSWORD);
    await settle(second);
    await shootSecond('signin-1-login');
    await byText(second, 'button', 'signin.login.submit').click();
    await byText(second, 'heading', 'signin.totp.title').waitFor({ timeout: WAIT });
    await second.getByLabel(t('signin.totp.code'), { exact: true }).fill(await nextCode(controlUrl, authenticators.get(PEOPLE.mira.username)));
    await settle(second);
    await shootSecond('signin-2-code');
    await byText(second, 'button', 'signin.login.submit').click();
    await byText(second, 'heading', 'signin.key.title').waitFor({ timeout: WAIT });
    if (miraKey === null) throw new Error('mira recovery key was not captured');
    await second.getByRole('textbox', { name: t('signin.key.label'), exact: true }).fill(miraKey);
    await settle(second);
    await shootSecond('signin-3-recovery-key');
    await byText(second, 'button', 'signin.key.submit').click();
    await byText(second, 'heading', 'signin.done.title').waitFor({ timeout: WAIT });
    await settle(second);
    await shootSecond('signin-4-done');
    await byText(second, 'button', 'signin.done.next').click();
    await byText(second, 'navigation', 'shell.rail.label').waitFor({ timeout: WAIT });

    // Mira's original browser shows both signed-in devices and the notification opt-in.
    await gear(mira).click();
    await byText(mira, 'navigation', 'settings.nav.label').getByRole('button', { name: t(WORKSPACE_SETTINGS.devices), exact: true }).click();
    await byText(mira, 'heading', 'devices.title').waitFor({ timeout: WAIT });
    await mira.locator('.d-device-row').nth(1).waitFor({ timeout: WAIT });
    await settle(mira);
    await shoot('settings-devices');
    // The sign-out dialog of the second browser, opened and closed again without sending anything.
    await gear(second).click();
    await byText(second, 'navigation', 'settings.nav.label').getByRole('button', { name: t(WORKSPACE_SETTINGS.devices), exact: true }).click();
    await byText(second, 'heading', 'devices.title').waitFor({ timeout: WAIT });
    await second.locator('.d-device-row').nth(1).waitFor({ timeout: WAIT });
    // Signal task 10 fix round 1: Sign out is on this browser's own device row (README §5.9).
    await second.locator('.d-settings-frame__body .d-device-row__actions').getByRole('button', { name: t('workspace.signOut'), exact: true }).click();
    await byText(second, 'heading', 'devices.signOutTitle').waitFor({ timeout: WAIT });
    await settle(second, { keepFocus: true });
    await shootSecond('settings-devices-signout');
    await second.keyboard.press('Escape');
    await byText(second, 'heading', 'devices.signOutTitle').waitFor({ state: 'hidden', timeout: WAIT });
    await byText(second, 'button', 'settings.close').click();
    await byText(mira, 'button', WORKSPACE_SETTINGS.notifications).click();
    await byText(mira, 'heading', 'notify.title').waitFor({ timeout: WAIT });
    await mira.getByRole('switch', { name: t('notify.desktop.label'), exact: true }).waitFor({ timeout: WAIT });
    await settle(mira);
    await shoot('settings-notifications');
    // web3.6: Settings -> Sign-in lists the factors mira set up at sign-up.
    await byText(mira, 'navigation', 'settings.nav.label').getByRole('button', { name: t(WORKSPACE_SETTINGS.signin), exact: true }).click();
    await byText(mira, 'heading', 'factorSettings.title').waitFor({ timeout: WAIT });
    await byText(mira, 'button', 'factorSettings.codes.make').waitFor({ timeout: WAIT });
    await settle(mira);
    await shoot('settings-signin');
    await byText(mira, 'button', 'settings.close').click();

    // Enter the second channel once so this browser joins its group, then leave it unopened on screen.
    await mira.getByRole('button', { name: 'loot', exact: true }).click();
    await composer(mira, 'loot').and(mira.locator(':enabled')).waitFor({ timeout: WAIT });
    await mira.getByRole('button', { name: 'general', exact: true }).click();
    await composer(mira, 'general').and(mira.locator(':enabled')).waitFor({ timeout: WAIT });

    // A message in the now-unopened channel produces a row and rail badge.
    await pages.ada.getByRole('button', { name: 'loot', exact: true }).click();
    await composer(pages.ada, 'loot').and(pages.ada.locator(':enabled')).waitFor({ timeout: WAIT });
    await send(pages.ada, 'loot', 'the map is in the chest');
    await mira.getByRole('button', { name: /loot, unread 1, mentions 0/ }).waitFor({ timeout: WAIT });
    await settle(mira);
    await shoot('shell-unread-badge');

    // Open a DM from the member picker, then show its conversation.
    await homeTile(mira).click();
    await byText(mira, 'button', 'shell.dms.new').click();
    const picker = byText(mira, 'dialog', 'shell.dms.newTitle');
    await picker.getByRole('listitem').filter({ hasText: PEOPLE.ada.display })
      .getByRole('button', { name: t('shell.dms.open'), exact: true }).click();
    const dmBox = mira.getByRole('textbox', { name: t('shell.dm.composer', { name: PEOPLE.ada.display }), exact: true });
    await dmBox.waitFor({ state: 'visible', timeout: WAIT });
    await dmBox.fill('see you at the bridge, Ada');
    await dmBox.press('Enter');
    await mira.getByRole('log', { name: t('shell.dm.log', { name: PEOPLE.ada.display }) })
      .getByText('see you at the bridge, Ada').waitFor({ timeout: WAIT });
    await settle(mira);
    await shoot('dm-conversation');

    // Return to the channel before the removal proof below.
    await railServer(mira, SERVER).click();
    await mira.getByRole('button', { name: 'general', exact: true }).click();
    await composer(mira, 'general').waitFor({ timeout: WAIT });

    // Ada answers the DM while Mira reads #general: the @ tile counts it, and #loot (still unread) keeps the
    // server's badge in the rail. The tile's name carries its counts once they show, so it is matched by its start.
    await homeTile(pages.ada).click();
    await pages.ada.getByRole('button', { name: new RegExp(`^${PEOPLE.mira.display}`) }).click();
    const adaDm = pages.ada.getByRole('textbox', { name: t('shell.dm.composer', { name: PEOPLE.mira.display }), exact: true });
    await adaDm.and(pages.ada.locator(':enabled')).waitFor({ timeout: WAIT });
    await adaDm.fill('rope is packed. bring the torches');
    await adaDm.press('Enter');
    await pages.ada.getByRole('log', { name: t('shell.dm.log', { name: PEOPLE.mira.display }) })
      .locator('.d-message-row[data-state="ok"]').filter({ hasText: 'rope is packed' }).waitFor({ timeout: WAIT });
    await homeTile(mira).and(mira.locator('[data-unread="1"]')).waitFor({ timeout: WAIT });
    await settle(mira);
    await shoot('shell-dm-badge');

    // web3.9: the @ tile opens Home, the direct messages with Ada's unread reply; then back to #general.
    await homeTile(mira).click();
    await mira.getByRole('navigation', { name: t('shell.dms.label'), exact: true }).getByRole('button', { name: new RegExp(`^${PEOPLE.ada.display}`) }).click();
    await mira.getByRole('textbox', { name: t('shell.dm.composer', { name: PEOPLE.ada.display }), exact: true }).waitFor({ timeout: WAIT });
    await mira.getByRole('log', { name: t('shell.dm.log', { name: PEOPLE.ada.display }) }).getByText('rope is packed').waitFor({ timeout: WAIT });
    await settle(mira);
    await shoot('home-dms-desktop');
    await railServer(mira, SERVER).click();
    await mira.getByRole('button', { name: 'general', exact: true }).click();
    await composer(mira, 'general').waitFor({ timeout: WAIT });

    // web3.7: the phone shell, at 390×844. Mira's page flips to the phone layout live: the channel she is reading is a
    // pushed view with a back, the lists are behind it, and the tab bar names three tabs. What she received since this
    // tab opened (the mention, the DM from Ada) is in the activity tab.
    const phoneTabs = mira.getByRole('navigation', { name: t('nav.tabs.label'), exact: true });
    const phoneTab = (key) => phoneTabs.getByRole('link', { name: new RegExp(`^${t(key)}`) });
    const phoneBack = mira.getByRole('button', { name: t('nav.back'), exact: true });
    await mira.setViewportSize(PHONE_APP);
    await composer(mira, 'general').waitFor({ state: 'visible', timeout: WAIT });
    await settle(mira, { keepFocus: false });
    await shoot('phone-channel');
    // web3.9: on a phone a tap on a message reveals its actions; a second tap hides them.
    const revealRow = row(mira, 'general', 'raid tonight at').first();
    await revealRow.scrollIntoViewIfNeeded();
    await revealRow.click({ position: { x: 150, y: 8 } });
    await revealRow.and(mira.locator('[data-revealed="true"]')).waitFor({ timeout: WAIT });
    await settle(mira, { keepFocus: true });
    await shoot('message-reveal-phone');
    await revealRow.click({ position: { x: 150, y: 8 } });
    await mira.locator('[data-revealed="true"]').waitFor({ state: 'detached', timeout: WAIT });
    // web3.7.1 (opt in with --ui-out): the tab bar on a channel, hidden while typing, under Settings and under the members
    // sheet, in the Signal, light and high-contrast themes. Mira ends where she began: reading #general, nothing focused.
    if (o.uiOut !== undefined) {
      mkdirSync(o.uiOut, { recursive: true });
      const ui = async (name, theme) => {
        await mira.evaluate((th) => { document.documentElement.dataset.theme = th; }, theme);
        await settle(mira, { keepFocus: name.includes('typing') });
        await mira.screenshot({ path: path.join(o.uiOut, `${name}.${theme}.png`) });
      };
      const channelMenu = mira.getByRole('button', { name: t('nav.channel.menu'), exact: true });
      for (const theme of ['signal', 'light', 'high-contrast']) {
        // Right after opening: the tab bar shows, nothing is focused and there is no keyboard.
        await phoneBack.click();
        await phoneTabs.waitFor({ state: 'visible', timeout: WAIT });
        await mira.getByRole('button', { name: 'general', exact: true }).click();
        await composer(mira, 'general').waitFor({ timeout: WAIT });
        await ui('phone-channel-opened', theme);
        await phoneTabs.waitFor({ state: 'visible', timeout: WAIT });
        await ui('phone-channel-tabbar', theme);
        await composer(mira, 'general').focus();
        await phoneTabs.waitFor({ state: 'hidden', timeout: WAIT });
        await ui('phone-channel-typing', theme);
        await mira.evaluate(() => { if (document.activeElement instanceof HTMLElement) document.activeElement.blur(); });
        await phoneTabs.waitFor({ state: 'visible', timeout: WAIT });
        await phoneTab('nav.tab.you').click();
        await mira.getByRole('button', { name: t('you.settings'), exact: true }).click();
        const settings = mira.getByRole('dialog', { name: t('settings.title'), exact: true });
        await settings.waitFor({ timeout: WAIT });
        await phoneTabs.waitFor({ state: 'hidden', timeout: WAIT });
        await ui('phone-settings-index', theme);
        await settings.getByRole('navigation', { name: t('settings.nav.label'), exact: true })
          .getByRole('button', { name: t(WORKSPACE_SETTINGS.privacy), exact: true }).click();
        await mira.waitForURL(/\/settings\/privacy$/);
        await ui('phone-settings-privacy', theme);
        // Task 10: a phone section has no Back (README §5.10); the browser's back returns to the index.
        await mira.goBack();
        await mira.waitForURL(/\/settings$/);
        await settings.getByRole('button', { name: t('settings.close'), exact: true }).click();
        await settings.waitFor({ state: 'hidden', timeout: WAIT });
        await phoneBack.click();
        await phoneTab('home.tab').click();
        await composer(mira, 'general').waitFor({ timeout: WAIT });
        await channelMenu.click();
        await mira.getByRole('menuitem', { name: t('members.toggle'), exact: true }).click();
        const closeMembers = mira.getByRole('button', { name: t('members.close'), exact: true });
        await closeMembers.waitFor({ timeout: WAIT });
        await ui('phone-members-tabbar', theme);
        await closeMembers.click();
        await closeMembers.waitFor({ state: 'hidden', timeout: WAIT });
      }
      await mira.evaluate(() => { document.documentElement.dataset.theme = 'signal'; });
      await settle(mira, { keepFocus: false });
    }
    await phoneBack.click();
    await phoneTabs.waitFor({ state: 'visible', timeout: WAIT });
    await settle(mira);
    await shoot('phone-home');
    // web3.9: the @ tile on the Home tab, selected, with the direct messages beside the rail.
    await homeTile(mira).click();
    await mira.getByRole('navigation', { name: t('shell.dms.label'), exact: true }).waitFor({ timeout: WAIT });
    await settle(mira);
    await shoot('home-dms-phone');
    await railServer(mira, SERVER).click();
    await mira.getByRole('button', { name: 'general', exact: true }).waitFor({ timeout: WAIT });
    await phoneTab('nav.tab.activity').click();
    await mira.getByRole('heading', { name: t('activity.title'), exact: true }).waitFor({ timeout: WAIT });
    await mira.getByText(t('activity.dm'), { exact: false }).first().waitFor({ timeout: WAIT });
    await settle(mira);
    await shoot('phone-activity');
    await phoneTab('home.tab').click();
    await mira.getByRole('button', { name: 'general', exact: true }).click();
    await composer(mira, 'general').waitFor({ timeout: WAIT });
    await mira.setViewportSize(DESKTOP);
    await settle(mira, { keepFocus: true });

    // Each data-doc-state shot (L-DOC-23, L-DOC-24) at both viewports once its state is visible; `before` brings the
    // state into view after the viewport changes (a phone stacks the channel list above the conversation).
    const shootState = async (page, state, owned = null, before = null, reopen = null) => {
      for (const shot of DOC_SHOTS.filter((s) => s.state === `[data-doc-state="${state}"]`)) {
        await page.setViewportSize(DOC_VIEWPORTS[shot.viewport]);
        // Flipping to the phone layout re-mounts the shell, which drops a screen's transient state (an open comparison): let the
        // flip settle, and open the state again when it was lost.
        if (reopen !== null) {
          await page.waitForTimeout(1500);
          if (!(await page.locator(shot.state).first().isVisible())) await reopen();
        }
        await page.waitForSelector(shot.state, { state: 'visible', timeout: WAIT });
        if (before !== null) await before();
        await settle(page, { keepFocus: true });
        const outFile = path.join(o.outDir, `${shot.name}.png`);
        await shootNamedState({ context: shot.viewport === 'phone' ? owned : null, page, state: shot.state, outFile });
        written.push(outFile);
      }
      if (owned === null) await page.setViewportSize(DESKTOP);
    };
    const openSettings = async (section) => {
      await gear(mira).click();
      const key = WORKSPACE_SETTINGS[section];
      if (key === undefined) throw new Error(`unknown user settings section ${section}`);
      await byText(mira, 'navigation', 'settings.nav.label').getByRole('button', { name: t(key), exact: true }).click();
    };
    const closeSettings = () => byText(mira, 'button', 'settings.close').click();

    // The fullscreen settings acceptance set is opt-in and captures the live, integrated testhost state. It adds screenshots
    // only after the real users, server and channels above exist; no content is mocked for these images.
    if (o.fullscreenSettingsOut !== undefined) {
      mkdirSync(o.fullscreenSettingsOut, { recursive: true });
      const capture = async (page, name) => {
        await settle(page, { keepFocus: true });
        const outFile = path.join(o.fullscreenSettingsOut, `fullscreen-settings-${name}.png`);
        await page.screenshot({ path: outFile });
        written.push(outFile);
      };
      await mira.setViewportSize({ width: 1440, height: 900 });
      await openSettings('profile');
      const userSettings = mira.getByRole('dialog', { name: t('settings.title'), exact: true });
      await userSettings.waitFor({ timeout: WAIT });
      await capture(mira, 'profile-desktop-signal');
      for (const [section, name] of [
        ['signin', 'signin'], ['devices', 'devices'], ['notifications', 'notifications'],
        ['voice', 'voice'], ['privacy', 'privacy'], ['diagnostics', 'diagnostics'],
      ]) {
        await byText(mira, 'navigation', 'settings.nav.label')
          .getByRole('button', { name: t(WORKSPACE_SETTINGS[section]), exact: true }).click();
        await mira.waitForURL(new RegExp(`/settings/${section}$`));
        await capture(mira, `${name}-desktop-signal`);
      }
      // Signal task 10 fix round 1: settings search is the ⌘K palette's; the workspace has no search field to shoot.
      await byText(mira, 'navigation', 'settings.nav.label')
        .getByRole('button', { name: t(WORKSPACE_SETTINGS.profile), exact: true }).click();
      await mira.waitForURL(/\/settings\/profile$/);
      const display = userSettings.getByRole('textbox', { name: t('profile.display.label'), exact: true });
      // Task 11 (Q7): Profile saves as you type; a character it refuses keeps the draft unsaved for this shot.
      await display.fill(`Unsaved${String.fromCodePoint(0x09)}draft`);
      await byText(mira, 'navigation', 'settings.nav.label')
        .getByRole('button', { name: t(WORKSPACE_SETTINGS.signin), exact: true }).click();
      const discard = mira.getByRole('dialog', { name: 'Discard unsaved changes?', exact: true });
      await discard.waitFor({ timeout: WAIT });
      await capture(mira, 'dirty-draft-confirmation');
      await discard.getByRole('button', { name: 'Keep editing', exact: true }).click();
      await byText(mira, 'navigation', 'settings.nav.label')
        .getByRole('button', { name: t(WORKSPACE_SETTINGS.signin), exact: true }).click();
      await discard.waitFor({ timeout: WAIT });
      await discard.getByRole('button', { name: 'Discard changes', exact: true }).click();
      await userSettings.getByRole('navigation', { name: t('settings.nav.label'), exact: true })
        .getByRole('button', { name: t(WORKSPACE_SETTINGS.profile), exact: true }).click();
      await mira.waitForURL(/\/settings\/profile$/);
      for (const [theme, width, height, name] of [
        ['light', 1440, 900, 'profile-desktop-light'], ['high-contrast', 1440, 900, 'profile-desktop-high-contrast'],
        ['signal', 1024, 768, 'profile-tablet-signal'], ['signal', 390, 844, 'profile-phone-signal'],
        ['signal', 320, 740, 'profile-narrow-signal'], ['signal', 844, 390, 'profile-short-wide-signal'],
        ['signal', 720, 450, 'profile-short-viewport-signal'],
      ]) {
        await mira.evaluate(value => { document.documentElement.dataset.theme = value; }, theme);
        await mira.setViewportSize({ width, height });
        await mira.waitForTimeout(250);
        await capture(mira, name);
      }
      await mira.evaluate(() => { document.documentElement.dataset.theme = 'signal'; });
      await mira.setViewportSize(PHONE_APP);
      await mira.waitForTimeout(250);
      // Task 10 fix round 1: a phone has no settings index; Settings opens on Profile with the nav's chips (README §5.10).
      // Task 11: the shot is named for that, and the sections are reached by their chips (no index to go back to).
      await capture(mira, 'phone-settings-entry');
      for (const section of ['privacy', 'diagnostics']) {
        await userSettings.getByRole('navigation', { name: t('settings.nav.label'), exact: true })
          .getByRole('button', { name: t(WORKSPACE_SETTINGS[section]), exact: true }).click();
        await mira.waitForURL(new RegExp(`/settings/${section}$`));
        await capture(mira, `${section}-phone`);
      }
      await userSettings.getByRole('button', { name: t('settings.close'), exact: true }).click();
      await userSettings.waitFor({ state: 'hidden', timeout: WAIT });
      await mira.setViewportSize(DESKTOP);

      const adaPage = pages.ada;
      await adaPage.setViewportSize({ width: 1440, height: 900 });
      await railServer(adaPage, SERVER).click();
      await byText(adaPage, 'button', 'admin.open').click().then(() => byText(adaPage, 'menuitem', 'signalSettings.menu.serverSettings').click());
      const serverSettings = adaPage.getByRole('dialog', { name: t('admin.title'), exact: true });
      await serverSettings.waitFor({ timeout: WAIT });
      for (const [section, name] of [
        ['overview', 'overview'], ['members', 'members'], ['music', 'music'], ['automoderation', 'automod'],
      ]) {
        await serverSettings.getByRole('navigation', { name: t('admin.nav'), exact: true })
          .getByRole('button', { name: t(`workspace.${section}`), exact: true }).click();
        await adaPage.waitForURL(new RegExp(`/c/[0-9a-f]{32}/settings/${section}$`));
        await serverSettings.locator('.d-settings-frame__body > section').waitFor({ state: 'visible', timeout: WAIT });
        if (name === 'music') {
          const closeButton = serverSettings.getByRole('button', { name: t('settings.close'), exact: true });
          await closeButton.waitFor({ state: 'visible', timeout: WAIT });
          const geometry = await serverSettings.evaluate(dialog => {
            const panel = dialog.querySelector('.d-settings-frame__panel');
            const body = dialog.querySelector('.d-settings-frame__body');
            // Task 10: the close button is the viewport's (README §5.9), not the bar's.
            const button = dialog.querySelector('.d-settings-frame__close');
            const bar = button;
            const rect = (element) => {
              if (!(element instanceof HTMLElement)) return null;
              const box = element.getBoundingClientRect();
              return { top: box.top, bottom: box.bottom, left: box.left, right: box.right };
            };
            return { frameScrollTop: dialog.scrollTop, panelScrollTop: panel?.scrollTop ?? null, bodyScrollTop: body?.scrollTop ?? null,
              frame: rect(dialog), bar: rect(bar), closeButton: rect(button), viewportHeight: window.innerHeight };
          });
          console.log(`shoot-docs: music settings geometry ${JSON.stringify(geometry)}`);
          if (geometry.frameScrollTop !== 0 || geometry.closeButton === null || geometry.bar === null
            || geometry.closeButton.top < 0 || geometry.closeButton.bottom > geometry.viewportHeight
            || geometry.bar.top < 0 || geometry.bar.bottom > geometry.viewportHeight) {
            throw new Error(`music settings frame scrolled or close control is outside the viewport: ${JSON.stringify(geometry)}`);
          }
        }
        await capture(adaPage, `${name}-desktop`);
      }
      await serverSettings.getByRole('button', { name: t('settings.close'), exact: true }).click();
      await serverSettings.waitFor({ state: 'hidden', timeout: WAIT });
      await mira.setViewportSize(DESKTOP);
      await mira.emulateMedia({ reducedMotion: 'reduce' });
    }

    // web-3 (L-DOC-24). Ada calls Mira from their DM: Mira's banner rings (30 s from its arrival, time enough for both
    // shots); she declines, and Ada, alone in the call, leaves, which ends it.
    await homeTile(pages.ada).click();
    await pages.ada.getByRole('button', { name: new RegExp(`^${PEOPLE.mira.display}`) }).click();
    await pages.ada.getByRole('textbox', { name: t('shell.dm.composer', { name: PEOPLE.mira.display }), exact: true }).waitFor({ timeout: WAIT });
    // Signal (Ruling Z): a DM's call starts from the DM row's context menu.
    await pages.ada.getByRole('navigation', { name: t('shell.dms.label'), exact: true }).locator('.d-chrow[aria-current="page"]').click({ button: 'right' });
    await pages.ada.getByRole('menuitem', { name: t('call.start'), exact: true }).click();
    await connected(pages.ada);
    const ring = mira.locator('.d-ring-banner__ring');
    await ring.getByRole('button', { name: t('call.ring.answer'), exact: true }).waitFor({ timeout: WAIT });
    await shootState(mira, 'call-ring');
    await ring.getByRole('button', { name: t('call.ring.decline'), exact: true }).click();
    await ring.waitFor({ state: 'detached', timeout: WAIT });
    await leaveVoice(pages.ada);

    await openSettings('voice');
    await shootState(mira, 'voice-settings');
    await closeSettings();

    // Björn shares an audio file in #general; Mira presses its play button, and the browser's own player shows.
    await attach(pages.bjorn, { name: SHANTY.name, mimeType: 'audio/wav', buffer: shantyWav() });
    await trayReady(pages.bjorn, 1);
    await send(pages.bjorn, 'general', SHANTY.text);
    await confirmed(mira, 'general', SHANTY.text);
    const player = mira.locator('[data-doc-state="media-player"]').filter({ hasText: SHANTY.name });
    await player.getByRole('button', { name: t('media.play', { name: SHANTY.name }), exact: true }).click();
    await player.locator('audio').waitFor({ timeout: WAIT });
    // The play button had the focus, which shows the message's toolbar: blur it so the shot shows the player alone.
    await shootState(mira, 'media-player', null, async () => {
      // Flipping to the phone layout re-mounts the conversation, so the player can detach mid-scroll: try again.
      for (let attempt = 0; ; attempt++) {
        try { await player.scrollIntoViewIfNeeded({ timeout: 5000 }); break; } catch (e) { if (attempt >= 5) throw e; await mira.waitForTimeout(1000); }
      }
      await mira.evaluate(() => { if (document.activeElement instanceof HTMLElement) document.activeElement.blur(); });
    });

    // Ada and Björn join "harbour", then Mira; her stage waits until it hears both of them (data-voice-state ok).
    await joinVoice(pages.ada, SERVER, VOICE);
    await joinVoice(pages.bjorn, SERVER, VOICE);
    await joinVoice(mira, SERVER, VOICE);
    await stage(mira).locator('.d-call-tile:not([data-own])[data-voice-state="ok"]').nth(1).waitFor({ timeout: WAIT });
    await shootState(mira, 'voice-stage');
    // web3.8 (item 14): Ada shares her screen. Mira's stage brings it to the front on its own (R26), which is the focused
    // shot; a click on that tile again returns the grid, with the camera or avatar tile and the screen as separate tiles.
    // Both at 1440×900, the size the stage was designed at.
    // Task 8c: Share opens the share picker; Go live starts the share, and Share (now Stop sharing) ends it.
    const adaShare = pages.ada.getByRole('toolbar', { name: t('voice.controls.voice'), exact: true }).locator('[data-control="share"]');
    await adaShare.click();
    const sharePicker = pages.ada.getByRole('dialog', { name: t('callStage.share.title'), exact: true });
    await sharePicker.getByRole('button', { name: t('callStage.share.go'), exact: true }).click();
    await sharePicker.waitFor({ state: 'detached', timeout: WAIT });
    const adaScreen = stage(mira).locator('.d-call-tile[data-source="screen"]:not([data-own])');
    await adaScreen.waitFor({ timeout: WAIT });
    await adaScreen.locator('video').waitFor({ timeout: WAIT });
    await mira.waitForFunction(() => [...document.querySelectorAll('.d-call-tile[data-source="screen"] video')].some((v) => v.videoWidth > 0), null, { timeout: WAIT });
    // The channel list shows Ada's screen glyph once the instance has told the others about the share.
    await mira.getByRole('img', { name: t('voice.state.screen'), exact: true }).first().waitFor({ timeout: 20_000 }).catch(() => undefined);
    await mira.setViewportSize({ width: 1440, height: 900 });
    await settle(mira);
    await shoot('voice-stage-focused-desktop');
    await adaScreen.locator('.d-call-tile__focus').click();
    await settle(mira);
    await shoot('voice-stage-grid-desktop');
    await mira.setViewportSize(DESKTOP);
    await adaShare.click();
    await adaScreen.waitFor({ state: 'detached', timeout: WAIT });
    await settle(mira);
    // Mira reads #general and stays in voice: the dock above her user panel keeps the controls.
    await mira.getByRole('button', { name: 'general', exact: true }).click();
    await composer(mira, 'general').waitFor({ timeout: WAIT });
    // web3.10: the dock's second line shows the ping and the up/down rates once the first link read (every 2 s) has landed.
    await mira.locator('.d-voice-dock__stats:not([data-quality="none"])').waitFor({ timeout: 20_000 });
    // Product walk I4: no scroll helper: at 360 the dock is pinned on screen by itself.
    await shootState(mira, 'voice-dock');
    // web3.9: the dock and the user panel up close: the channel name over the link stats (no meter), camera, share and leave.
    {
      const dockBox = await dock(mira).boundingBox();
      const panelBox = await mira.locator('.d-user-panel').boundingBox();
      if (dockBox === null || panelBox === null) throw new Error('the dock or the user panel has no box');
      const top = Math.max(0, Math.floor(dockBox.y) - 10);
      const dockFile = path.join(o.outDir, 'dock-desktop.png');
      await mira.screenshot({ path: dockFile, clip: { x: 0, y: top, width: 330, height: Math.ceil(panelBox.y + panelBox.height) + 6 - top } });
      written.push(dockFile);
    }
    // web3.7: on a phone the dock is the call bar above the tab bar of the lists (the servers list, here).
    await mira.setViewportSize(PHONE_APP);
    await phoneBack.click();
    await mira.locator('.d-call-bar').waitFor({ state: 'visible', timeout: WAIT });
    await settle(mira);
    await shoot('phone-callbar');
    await mira.getByRole('button', { name: 'general', exact: true }).click();
    await composer(mira, 'general').waitFor({ timeout: WAIT });
    await mira.setViewportSize(DESKTOP);
    await settle(mira, { keepFocus: true });
    for (const who of [mira, pages.bjorn, pages.ada]) await leaveVoice(who);
    // Back in #general, Ada's and Björn's tabs commit what the instance proposes there (Mira's removal, below).
    for (const who of [pages.bjorn, pages.ada]) {
      await who.getByRole('button', { name: 'general', exact: true }).click();
      await composer(who, 'general').waitFor({ timeout: WAIT });
    }

    // web3.9: the member list (two lines a row) and a header icon's tooltip, from #general.
    // The people who just left harbour are gone from the list before the shots.
    await mira.getByRole('navigation', { name: t('shell.channels.label'), exact: true }).getByText('Björn', { exact: true }).waitFor({ state: 'hidden', timeout: WAIT });
    const jumpToPresent = mira.getByRole('button', { name: t('layout.log.jump'), exact: true });
    if (await jumpToPresent.isVisible()) await jumpToPresent.click();
    await jumpToPresent.waitFor({ state: 'hidden', timeout: WAIT });
    const membersButton = mira.locator('.d-channel-header [data-members-toggle]');
    const membersWereOpen = await membersButton.getAttribute('aria-pressed') === 'true';
    if (!membersWereOpen) await membersButton.click();
    await mira.locator('.d-member-list').waitFor({ timeout: WAIT });
    await mira.locator('.d-member-list__row').nth(2).waitFor({ timeout: WAIT });
    await settle(mira);
    // web3.10: Björn's row opens his profile card beside the list (message and compare safety numbers; Mira cannot kick or ban).
    const bjornRow = mira.locator('.d-member-list__row', { hasText: 'Björn' }).first();
    await bjornRow.click();
    await mira.locator('.d-profile-card').waitFor({ timeout: WAIT });
    await settle(mira, { keepFocus: true });
    await shoot('members-desktop');
    await mira.keyboard.press('Escape');
    await mira.locator('.d-profile-card').waitFor({ state: 'detached', timeout: WAIT });
    await membersButton.hover();
    await mira.getByRole('tooltip').waitFor({ timeout: WAIT });
    await shoot('header-tooltip-desktop');
    await mira.mouse.move(-10, -10);
    if (!membersWereOpen) await membersButton.click();

    // AW6: the built-in AFK channel hosts no call. One click marks Mira away; the AFK row lists her with `afk.away`.
    await railServer(mira, SERVER).click();
    await voiceRow(mira, 'AFK').click();
    await mira.locator('.d-voice-row__person[data-away="true"]').first().waitFor({ timeout: WAIT });
    await settle(mira);
    await shoot('afk-call-desktop');
    await mira.getByRole('button', { name: 'general', exact: true }).click();
    await composer(mira, 'general').waitFor({ timeout: WAIT });

    // web3.8: the desktop has no status bar. The link line that replaced it, shot from #general with Mira out of the call
    // (the call would drop with the connection).
    try {
      await mira.context().setOffline(true);
      await mira.getByText(t('nav.link.offline'), { exact: true }).waitFor({ timeout: WAIT });
      await settle(mira);
      await shoot('link-offline-desktop');
    } finally {
      await mira.context().setOffline(false);
    }
    await mira.getByText(t('nav.link.offline'), { exact: true }).waitFor({ state: 'hidden', timeout: WAIT });
    await composer(mira, 'general').waitFor({ timeout: WAIT });

    // web-2c (L-DOC-23): Privacy, a contact's changed identity, search, diagnostics and the tab hand-over, each shot
    // at both viewports once its data-doc-state is visible. The contact whose key changes is the native peer, in a
    // server of its own that Mira joins.
    peer = startPeer(publicUrl, controlUrl, instanceInvite);
    const watch = (await peer.request('setup', { community: 'Bridge Watch', channel: 'watch', voice: true }));
    await peer.request('register');
    await byText(mira, 'button', 'layout.rail.add').click();
    await byText(mira, 'dialog', 'layout.add.title').getByRole('button', { name: t('layout.add.join'), exact: true }).click();
    const join = byText(mira, 'dialog', 'join.title');
    await join.getByLabel(t('join.invite'), { exact: true }).fill(watch.invite_code);
    await join.getByRole('button', { name: t('join.submit'), exact: true }).click();
    await join.waitFor({ state: 'hidden', timeout: WAIT });
    await byText(mira, 'navigation', 'shell.rail.label').getByRole('button', { name: /^Bridge Watch(, |$)/ }).click();
    await byText(mira, 'navigation', 'shell.channels.label').getByRole('button', { name: /^watch(, |$)/ }).click();
    await composer(mira, 'watch').and(mira.locator(':enabled')).waitFor({ timeout: WAIT });
    for (let i = 0; i < 60; i++) {
      await peer.request('sync', { channel_id: watch.channel_id });
      if ((await peer.request('members', { channel_id: watch.channel_id })).devices.length === 2) break;
      await mira.waitForTimeout(500);
    }
    await peer.request('send', { body: 'the bridge is quiet tonight', channel_id: watch.channel_id });
    await row(mira, 'watch', 'the bridge is quiet tonight').and(mira.locator('[data-state="ok"]')).waitFor({ timeout: WAIT });

    await openSettings('privacy');
    await shootState(mira, 'trust-privacy');
    // The comparison itself: the contact, the state, the exact public key and the 12 blocks of five digits.
    const openComparison = async () => {
      await mira.getByRole('button', { name: t('privacy.compare', { name: watch.display }), exact: true }).click();
      await mira.locator('[data-safety-group]').nth(11).waitFor({ timeout: WAIT });
    };
    await openComparison();
    await shootState(mira, 'trust-compare', null, null, openComparison);
    await mira.keyboard.press('Escape');
    await byText(mira, 'dialog', 'privacy.title').waitFor({ state: 'hidden', timeout: WAIT });
    await closeSettings();

    await composer(mira, 'watch').focus();
    await mira.keyboard.press('ControlOrMeta+K');
    await mira.getByRole('combobox', { name: t('search.placeholder'), exact: true }).fill('bridge');
    // The search hits are div[role="option"]: the status select under the channel list has native options too.
    await mira.locator('div[role="option"]').first().waitFor({ timeout: WAIT });
    await shootState(mira, 'search-results');
    await mira.keyboard.press('Escape');

    await openSettings('diagnostics');
    await shootState(mira, 'settings-diagnostics');
    await closeSettings();

    await peer.request('rotate_umk');
    await peer.request('send', { body: 'new key, same watch', channel_id: watch.channel_id });
    await mira.getByRole('alert').filter({ hasText: t('privacy.changed') }).waitFor({ timeout: WAIT });
    // In the conversation: the alert with Compare, and the message field blocked with the same words.
    await mira.getByText(t('privacy.changed'), { exact: true }).last().waitFor({ timeout: WAIT });
    await shootState(mira, 'trust-conversation');
    await openSettings('privacy');
    await shootState(mira, 'trust-changed');
    await closeSettings();

    // A second tab of Mira's browser waits while this one holds the device; the tab is the shot's own and closes.
    const tab = await mira.context().newPage();
    await tab.goto('/');
    await shootState(tab, 'tabs-handover', { close: () => tab.close() });

    // web-3: the peer's new device opens the call of "watch-voice", and Mira's join is refused before anything is
    // posted, with a notice that names the peer. Shot before the removal below, in the order of the guide (the WB fix
    // wave fixed the not-member report a later frame used to clear, which kept that removal from showing).
    await peer.request('call_join', { actor: 'peer', channel: watch.voice_channel_id });
    await railServer(mira, 'Bridge Watch').click();
    await voiceRow(mira, 'watch-voice').click();
    await shootState(mira, 'voice-blocked');

    await byText(mira, 'navigation', 'shell.rail.label').getByRole('button', { name: new RegExp(`^${SERVER}(, |$)`) }).click();
    await mira.getByRole('button', { name: 'general', exact: true }).click();
    await composer(mira, 'general').waitFor({ timeout: WAIT });

    // Mira is removed from the server with her tab open. Since the removal flow (web3.5) she is taken to another
    // server and a banner says "You are no longer in ...", which is what the guide shows.
    await kickFromServer(controlUrl, path.join(scratch, 'host'), SERVER, PEOPLE.ada.username, PEOPLE.mira.username);
    await mira.getByText(t('removed.notice', { server: SERVER }), { exact: true }).waitFor({ timeout: WAIT });
    await settle(mira);
    await shoot('shell-removed');
  } finally {
    if (peer !== null) await stop(peer.child);
    for (const c of contexts) await c.close().catch(() => undefined);
    await stop(host.child);
    rmSync(scratch, { recursive: true, force: true });
  }
  const notShot = [...SIGNIN_SHOTS, ...WEB38_SHOTS, ...WEB39_SHOTS].filter((name) => !written.includes(path.join(o.outDir, `${name}.png`)));
  if (notShot.length > 0) throw new Error(`images not shot: ${notShot.join(', ')}`);
  for (const file of written) process.stdout.write(`shoot-docs: ${path.relative(REPO_ROOT, file)} (${statSync(file).size} bytes)\n`);
  for (const p of problems) process.stderr.write(`shoot-docs: page error ${p}\n`);
  return problems.length > 0 ? 1 : 0;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2)).then(
    (code) => { process.exitCode = code; },
    (err) => { process.stderr.write(`${err && err.stack ? err.stack : err}\n`); process.exitCode = 1; },
  );
}
