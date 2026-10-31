// The UX audit world: an empty test host with the Bot runtime, then a realistic server built through the client only. It keeps
// every browser open (each with a CDP port) so ux:shots, the parity shooter and the step runner can attach, writes state.json when
// ready, then holds until it is stopped.
//   npm run ux:world -w @dilla/e2e -- [--base <n>] [--out <dir>]
// (needs packages/web/dist, target/dilla-testhost and target/dillabot, and the core wasm built).
// Ports from --base n (default 8611): host n, control n+1, SFU n+2 (UDP n+4), browsers' CDP from n+10, Ada's phone n+19.
// Output: --out (default target/ui-screens/signal-b), state.json inside it.
import { mkdirSync, writeFileSync, rmSync } from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { spawnSync } from 'node:child_process';
import { waitForVoiceJoin } from './voice-fixture.mjs';
import { fileURLToPath, pathToFileURL } from 'node:url';

const REPO = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const argv = process.argv.slice(2);
const flag = (name, dflt) => { const i = argv.indexOf(name); return i >= 0 && argv[i + 1] !== undefined ? argv[i + 1] : dflt; };
const BASE = Number(flag('--base', '8611'));
const OUT = path.resolve(flag('--out', path.join(REPO, 'target', 'ui-screens', 'signal-b')));
const require = createRequire(path.join(REPO, 'e2e', 'package.json'));
const { chromium } = require('@playwright/test');
const { build } = require('esbuild');
const { t } = await import(path.join(REPO, 'packages/web/src/strings/index.ts'));
const { chromiumMediaArgs, writeFixtures } = await import(path.join(REPO, 'e2e/media/support/fixtures.ts'));
const { nextCode } = await import(path.join(REPO, 'e2e/web/support/totp.ts'));
const { addAuthenticator } = await import(path.join(REPO, 'e2e/web/support/webauthn.ts'));

// The world never builds: it needs the web client, the testhost and bot binaries and the core wasm already built (see README).
import { existsSync } from 'node:fs';
{
  const need = [
    ['packages/web/dist/index.html', 'npm run build -w @dilla/web'],
    ['target/dilla-testhost', 'go build -o target/dilla-testhost ./cmd/dilla-testhost'],
    ['target/dillabot', 'go build -o target/dillabot ./cmd/dillabot'],
    ['internal/mlswasi/testdata/dilla_core_wasi.wasm', 'cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked (then copy to internal/mlswasi/testdata)'],
  ].filter(([f]) => !existsSync(path.join(REPO, f)));
  if (need.length > 0) { console.error('ux:world needs built artifacts:\n' + need.map(([f, c]) => `  ${f}: ${c}`).join('\n')); process.exit(2); }
}
// host.ts (startTestHost: the host, its fake-clock ticker and the Bot runtime) is TypeScript with extensionless imports, which
// plain node cannot load: bundle it once into target/ and import that. import.meta.url is pinned so REPO_ROOT stays right.
const hostSrc = path.join(REPO, 'e2e/web/support/host.ts');
const hostBundle = path.join(REPO, 'target', 'tmp', `ux-host-${BASE}.bundle.mjs`);
await build({ entryPoints: [hostSrc], outfile: hostBundle, bundle: true, platform: 'node', format: 'esm', packages: 'external', logLevel: 'error',
  define: { 'import.meta.url': JSON.stringify(pathToFileURL(hostSrc).href) } });
const { startTestHost } = await import(pathToFileURL(hostBundle).href);

const PORT = BASE, CONTROL = BASE + 1, SFU = BASE + 2, SFU_UDP = BASE + 4;
// Connected RTC profiles match the testhost's numeric signalling origin and CSP.
// Only fresh auth captures use localhost, a valid WebAuthn RP hostname.
const publicUrl = `http://127.0.0.1:${PORT}`;
const authUrl = `http://localhost:${PORT}`;
const controlUrl = `http://127.0.0.1:${CONTROL}`;
const WAIT = 60_000;
mkdirSync(OUT, { recursive: true });
const DATA = path.join(OUT, 'world');
rmSync(DATA, { recursive: true, force: true });
mkdirSync(DATA, { recursive: true });
const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);

const host = await startTestHost({ port: PORT, control: CONTROL, webRoot: 'packages/web/dist', webauthnOrigin: authUrl, botRuntime: true, advanceClockWithWall: true, wallClockSweep: false, prebuilt: true,
  sfu: { port: SFU, udpPort: SFU_UDP } });
const invite0 = host.invite.split(',')[0];
log('host up');
const crash = (e) => { console.error(e); host.stop().finally(() => process.exit(1)); };
process.on('uncaughtException', crash);
process.on('unhandledRejection', crash);
const fixtures = writeFixtures(path.join(DATA, 'media'));
const state = { publicUrl, authUrl, controlUrl, people: {}, cdp: {} };
const authenticators = {};
let cdpPort = BASE + 10;
const save = () => writeFileSync(path.join(OUT, 'state.json'), JSON.stringify(state, null, 2));

async function open(who, extra = {}) {
  const port = cdpPort++;
  const ctx = await chromium.launchPersistentContext(path.join(DATA, `profile-${who}`), {
    headless: true, viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1, colorScheme: 'dark', reducedMotion: 'reduce',
    locale: 'en-GB', timezoneId: 'Europe/Stockholm', baseURL: publicUrl,
    args: [...chromiumMediaArgs(fixtures), '--auto-select-desktop-capture-source=Entire screen', `--remote-debugging-port=${port}`],
    permissions: ['microphone', 'camera', 'clipboard-read', 'clipboard-write'], ...extra,
  });
  state.cdp[who] = port;
  const page = ctx.pages()[0] ?? await ctx.newPage();
  page.on('pageerror', (e) => log(who, 'pageerror', String(e).slice(0, 200)));
  return page;
}
const byText = (page, role, key, vars) => page.getByRole(role, { name: t(key, vars), exact: true });
const composer = (page, channel) => page.getByRole('textbox', { name: t('shell.composer.label', { channel }), exact: true });
const row = (page, channel, text) => page.getByRole('log', { name: t('shell.log.label', { channel }), exact: true })
  .locator('.d-message-row').filter({ hasText: text });
const rail = (page) => byText(page, 'navigation', 'shell.rail.label');
const escape = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

/** Settings → Profile, as a person sets the display name after sign-up (wave B task 3b: sign-up asks for none). */
async function setDisplay(page, display) {
  if (!display) return;
  await page.goto('/settings/profile');
  const field = page.getByRole('textbox', { name: t('profile.display.label'), exact: true });
  await field.waitFor({ timeout: WAIT });
  await field.fill(display);
  await page.getByRole('status').filter({ hasText: t('profile.saved') }).first().waitFor({ timeout: WAIT });
  await page.keyboard.press('Escape');
  await rail(page).waitFor({ timeout: WAIT });
}

async function onboard(page, person, invite) {
  await page.goto(`/welcome?invite=${encodeURIComponent(invite)}`);
  await page.locator('.d-onboarding-frame h1').waitFor({ timeout: WAIT });
  await byText(page, 'button', 'onboarding.next').click();
  // Wave B task 3b: step 2 has the username and the password; Create account makes the keys, and the key step's
  // I saved it creates the account. There is no display name at sign-up: Settings → Profile sets it below.
  await byText(page, 'heading', 'onboarding.identity.title').waitFor({ timeout: WAIT });
  await page.getByLabel(t('onboarding.identity.username'), { exact: true }).fill(person.username);
  person.password = `ux-${person.username}-password`;
  const pw = page.getByLabel(t('onboarding.identity.password'), { exact: true });
  if (await pw.count() > 0) await pw.fill(person.password);
  await byText(page, 'button', 'onboarding.identity.submit').click();
  await byText(page, 'heading', 'onboarding.keys.title').waitFor({ timeout: WAIT });
  await page.locator('.d-recovery-key li').first().waitFor({ timeout: WAIT });
  person.recoveryKey = (await page.locator('.d-recovery-key li').allTextContents()).join(' ');
  await byText(page, 'checkbox', 'onboarding.keys.acknowledge').check();
  await byText(page, 'button', 'onboarding.keys.submit').click();
  await byText(page, 'heading', 'factor.choose.title').waitFor({ timeout: WAIT });
  await page.getByRole('radio', { name: new RegExp(t('factor.choose.app')) }).check();
  await byText(page, 'button', 'factor.choose.next').click();
  await byText(page, 'heading', 'factor.totp.titleSignup').waitFor({ timeout: WAIT });
  await page.waitForFunction(() => /^[A-Z2-7]{16,}$/.test(document.querySelector('#factor-totp-key')?.value ?? ''), null, { timeout: WAIT });
  const a = { totpSecret: await page.getByLabel(t('factor.totp.key'), { exact: true }).inputValue(), lastCounter: -1 };
  authenticators[person.username] = a;
  // Wave B task 3: the sixth digit in the code boxes sends the code; there is no button to press.
  await page.getByLabel(t('factor.totp.code'), { exact: true }).fill(await nextCode(controlUrl, a, { wallClock: true }));
  // Step 6: the backup codes, whose box gates Open dilla.
  await byText(page, 'heading', 'onboarding.done.title').waitFor({ timeout: WAIT });
  await byText(page, 'checkbox', 'onboarding.done.codesAck').check();
  await byText(page, 'button', 'onboarding.done.next').click();
  await rail(page).waitFor({ timeout: WAIT });
  await setDisplay(page, person.display);
  person.totp = a;
  state.people[person.username] = person;
  log('onboarded', person.username);
}

const openChannel = async (page, name, remaining = () => WAIT) => {
  await page.getByRole('navigation', { name: t('shell.channels.label'), exact: true }).getByRole('button', { name: new RegExp(`^${escape(name)}(,|$)`) }).first().click({ timeout: remaining() });
  // Reading an announcement channel does not grant write permission. Its
  // disabled composer still confirms navigation; send() waits for write-ready.
  await composer(page, name).waitFor({ timeout: remaining() });
};
async function send(page, channel, text, budget) {
  // Optional unread/announcement decoration shares one deadline across every
  // operation and retry. Required conversation seeding keeps the full WAIT.
  const deadline = budget === undefined ? null : Date.now() + budget;
  const remaining = () => deadline === null ? WAIT : Math.max(1, deadline - Date.now());
  if (!(await composer(page, channel).isVisible().catch(() => false))) await openChannel(page, channel, remaining);
  const box = composer(page, channel);
  await box.and(page.locator(':enabled:not([readonly])')).waitFor({ timeout: remaining() });
  await box.fill(text, { timeout: remaining() });
  await box.press('Enter', { timeout: remaining() });
  const target = row(page, channel, text.split('\n')[0]);
  for (let i = 0; i < 3; i++) {
    await target.and(page.locator('[data-state="ok"], [data-state="failed"]')).last().waitFor({ timeout: remaining() });
    if (await target.last().getAttribute('data-state', { timeout: remaining() }) !== 'failed') break;
    await target.last().getByRole('button', { name: t('shell.message.retry'), exact: true }).click({ timeout: remaining() });
    await page.waitForTimeout(Math.min(1500, remaining()));
  }
}
const trySend = async (page, channel, text) => { try { await send(page, channel, text, 10_000); } catch (e) { log('optional send failed', channel, String(e).slice(0, 200)); } };

const SERVER = 'Midgard Crew';
const USER_AGENT = 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1';
const ada = await open('ada');
const P = { ada: { username: 'ada', display: 'Ada' }, bjorn: { username: 'bjorn', display: 'Björn' }, mira: { username: 'mira', display: 'Mira' },
  liv: { username: 'liv', display: 'Liv Halvorsen-Strand, Navigator of the Northern Fjords' } };
await onboard(ada, P.ada, invite0);
await rail(ada).getByRole('button', { name: t('layout.rail.add'), exact: true }).click();
await byText(ada, 'dialog', 'layout.add.title').getByRole('button', { name: t('layout.add.create'), exact: true }).click();
const sd = byText(ada, 'dialog', 'create.server.title');
await sd.getByRole('textbox', { name: t('create.server.name'), exact: true }).fill(SERVER);
await sd.getByRole('textbox', { name: t('create.server.name'), exact: true }).press('Enter');
await sd.waitFor({ state: 'hidden', timeout: WAIT });
await composer(ada, 'general').and(ada.locator(':enabled')).waitFor({ timeout: WAIT });
log('server made');

const closeSettings = async () => {
  for (let i = 0; i < 6; i++) {
    if (!(await byText(ada, 'dialog', 'admin.title').isVisible().catch(() => false))) return;
    await ada.keyboard.press('Escape');
    await ada.waitForTimeout(500);
  }
};
// The Bot: installed through Server settings -> Bot with Polls, Reminders and Music on, as e2e/web/bot-runtime.spec.ts does. Its display
// name goes to state.botName.
{
  await byText(ada, 'button', 'admin.open').click().then(() => byText(ada, 'menuitem', 'signalSettings.menu.serverSettings').click());
  const bs = byText(ada, 'dialog', 'admin.title');
  await bs.getByRole('navigation', { name: t('admin.nav'), exact: true }).getByRole('button', { name: t('workspace.bot'), exact: true }).click();
  for (const cap of ['polls', 'reminders', 'music']) {
    const sw = bs.getByRole('switch', { name: t(`serverSettings.bot.cap.${cap}`), exact: true });
    await sw.waitFor({ timeout: WAIT });
    if ((await sw.getAttribute('aria-checked')) !== 'true') await sw.click();
  }
  await bs.getByRole('button', { name: t('serverSettings.bot.enable'), exact: true }).click();
  const stepUp = byText(ada, 'dialog', 'factorSettings.stepUp.title');
  await stepUp.getByLabel(t('factorSettings.stepUp.password'), { exact: true }).fill(state.people.ada.password);
  await stepUp.getByRole('button', { name: t('factorSettings.stepUp.confirm'), exact: true }).click();
  await ada.waitForTimeout(2500); await ada.screenshot({ path: path.join(OUT, 'bot-after-enable.png') });
  const deadline = Date.now() + 120_000;
  for (;;) {
    await closeSettings();
    await byText(ada, 'button', 'admin.open').click().then(() => byText(ada, 'menuitem', 'signalSettings.menu.serverSettings').click());
    const again = byText(ada, 'dialog', 'admin.title');
    await again.getByRole('navigation', { name: t('admin.nav'), exact: true }).getByRole('button', { name: t('workspace.bot'), exact: true }).click();
    try { const tok = (await import('node:fs')).readFileSync(host.runtimeCredentialFile, 'utf8').trim(); const r = await fetch(host.url + '/v1/bot-runtime/installations', { headers: { authorization: 'Bearer ' + tok, accept: 'application/cbor' } }); log('installations', r.status, Buffer.from(await r.arrayBuffer()).toString('hex').slice(0, 80)); } catch (e) { log('inst probe', String(e).slice(0, 100)); }
    await ada.waitForTimeout(5000);
    const status = await again.locator('.dw-bot-settings').innerText({ timeout: 4000 }).catch(() => ''); log('bot card', JSON.stringify(status.split('\n').slice(0, 6).join(' | ')));
    if (status.includes(t('serverSettings.bot.card', { presence: t('serverSettings.bot.online') }).replace(/ · v.*/, ''))) {
      const lines = status.split('\n').map((l) => l.trim()).filter(Boolean); const tag = lines.findIndex((l) => l.toLowerCase() === t('serverSettings.bot.tag')); state.botName = tag > 0 ? lines[tag - 1] : t('serverSettings.bot.fallbackName');
      break;
    }
    if (Date.now() > deadline) throw new Error('the Bot did not reach ready: ' + status);
    await ada.waitForTimeout(2000);
  }
  await closeSettings();
  log('bot ready', state.botName);
  save();
}



const addChannel = async (kind, name) => {
  await ada.getByRole('navigation', { name: t('shell.channels.label'), exact: true }).getByRole('button', { name: t('create.channel.open'), exact: true }).click();
  const dialog = byText(ada, 'dialog', 'create.channel.title', { server: SERVER });
  const field = dialog.getByRole('textbox', { name: t('create.channel.name'), exact: true });
  await field.waitFor({ timeout: WAIT });
  if (kind === 'voice') await dialog.getByRole('radio', { name: t('create.channel.voice'), exact: true }).click();
  if (kind === 'announce') await dialog.getByRole('radio', { name: t('announce.kind'), exact: true }).click();
  await field.fill(name);
  await field.press('Enter');
  await dialog.waitFor({ state: 'hidden', timeout: WAIT });
  log('channel', kind, name);
};
const TEXT = ['loot', 'raid-planning-for-the-bridge-troll-and-the-long-winter-campaign', 'memes'];
for (const n of TEXT) await addChannel('text', n);
await addChannel('announce', 'news');
// The AFK channel is built in (web3.9): the server makes it, so the world adds no voice channel for it and picks none.
for (const n of ['harbour', 'Late night lounge for people who never sleep']) await addChannel('voice', n);

// Invite link.
await byText(ada, 'button', 'admin.open').click().then(() => byText(ada, 'menuitem', 'signalSettings.menu.serverSettings').click());
const settings = byText(ada, 'dialog', 'admin.title');
await settings.waitFor({ timeout: WAIT });
await settings.getByRole('navigation', { name: t('admin.nav'), exact: true }).getByRole('button', { name: t('workspace.invites'), exact: true }).click();
// Task 3b review I1: unlimited uses, so every parity pair that signs up through it (task 3b's six steps, desktop and
// phone) also joins the server, and the done step's panel can name it.
await settings.getByRole('radiogroup', { name: t('serverSettings.invites.uses'), exact: true })
  .getByRole('radio', { name: t('serverSettings.invites.uses.unlimited'), exact: true }).click();
// Signal task 10c: the link shows once in the Invites card (no dialog).
await settings.getByRole('button', { name: t('serverSettings.invites.make'), exact: true }).click();
const inv = settings;
const link = inv.getByRole('textbox', { name: t('serverSettings.invites.link'), exact: true });
await link.waitFor({ timeout: WAIT });
await ada.waitForFunction(() => [...document.querySelectorAll('input')].some((i) => i.value.includes('/i/')), null, { timeout: WAIT });
const inviteLink = await link.inputValue();
const invitePath = new URL(inviteLink).pathname;
const invite = decodeURIComponent(invitePath.slice(3));
state.invitePath = invitePath;
// Give whole-screen captures the prototype's channel topic and a real hoisted group.
await settings.getByRole('navigation', { name: t('admin.nav'), exact: true }).getByRole('button', { name: t('workspace.channels'), exact: true }).click();
await settings.getByRole('listitem').filter({ hasText: '#general' }).getByRole('button', { name: t('admin.channels.editLabel', { name: 'general' }), exact: true }).click();
const channelEdit = byText(ada, 'dialog', 'admin.channel.titleText', { name: 'general' });
await channelEdit.getByRole('textbox', { name: t('admin.channel.topic'), exact: true }).fill('raids, plans and the bridge troll');
await channelEdit.getByRole('button', { name: t('admin.channel.save'), exact: true }).click();
await channelEdit.waitFor({ state: 'hidden', timeout: WAIT });
await settings.getByRole('navigation', { name: t('admin.nav'), exact: true }).getByRole('button', { name: t('workspace.roles'), exact: true }).click();
await settings.getByRole('button', { name: t('serverSettings.roles.new'), exact: true }).click();
const roleName = settings.getByRole('textbox', { name: t('roles.editor.name'), exact: true });
await roleName.waitFor({ timeout: WAIT });
const roleField = await roleName.elementHandle();
await ada.waitForFunction(({ field, value }) => field.value === value, { field: roleField, value: t('serverSettings.roles.newName') }, { timeout: WAIT });
await roleField.dispose();
await roleName.fill('Raid leaders');
await roleName.press('Enter');
const raidRole = settings.getByRole('list', { name: t('serverSettings.roles.list'), exact: true }).getByRole('button', { name: /^Raid leaders, \d+ members?/ });
await raidRole.waitFor({ timeout: WAIT });
// Mira owns playback and, like the prototype's Raid lead, may pin messages. The
// latter gives her message toolbar its real More action instead of an empty menu.
for (const key of ['perm.music', 'perm.pin']) {
  const permission = byText(settings, 'switch', key);
  if (await permission.getAttribute('aria-checked') !== 'true') await permission.click();
  await permission.and(ada.locator('[aria-checked="true"]')).waitFor({ timeout: WAIT });
}
await raidRole.focus();
await ada.keyboard.press('Shift+F10');
const hoist = byText(ada, 'menuitemcheckbox', 'serverSettings.roles.menu.hoist');
await hoist.click();
await ada.waitForFunction(() => document.querySelector('[role="menuitemcheckbox"][aria-checked="true"]') !== null, null, { timeout: WAIT });
await ada.keyboard.press('Escape');
await ada.keyboard.press('Escape');
await settings.waitFor({ state: 'hidden', timeout: WAIT });
log('invite', invitePath);
save();

const pages = { ada };
for (const who of ['bjorn', 'mira', 'liv']) {
  pages[who] = await open(who);
  await onboard(pages[who], P[who], invite);
  await composer(pages[who], 'general').and(pages[who].locator(':enabled')).waitFor({ timeout: WAIT });
}
// Assign the hoisted role once members have joined.
// Channel creation leaves Ada on the last voice page, whose header has no
// Members action. Enter the text channel before opening the member fixture.
await openChannel(ada, 'general');
const members = byText(ada, 'complementary', 'members.region', { server: SERVER });
if (!(await members.isVisible().catch(() => false))) await ada.getByRole('button', { name: /^Members/ }).first().click();
for (const name of ['Björn', 'Mira']) {
  await members.getByRole('listitem').filter({ hasText: name }).locator('.d-member-list__open').click();
  await ada.getByRole('dialog', { name, exact: true }).getByRole('button', { name: t('layout.profile.manageRoles'), exact: true }).click();
  const assignRole = byText(ada, 'dialog', 'roles.assign.title', { name });
  // This is server-controlled: wait for the authoritative role slice after clicking.
  await assignRole.getByRole('checkbox', { name: 'Raid leaders', exact: true }).click();
  await assignRole.getByRole('checkbox', { name: 'Raid leaders', exact: true, checked: true }).waitFor({ timeout: WAIT });
  await assignRole.getByRole('button', { name: t('roles.assign.close'), exact: true }).click();
}
await openChannel(ada, 'general');
state.serverPath = new URL(ada.url()).pathname;
// Ada's phone: a mobile-emulated Chromium profile (390x844, touch, coarse pointer), signed in as Ada's second device through the
// real sign-in flow. Its sign-in screens are shot on the way; ux:shots drives it afterwards (who = "phone").
try {
  const ada1 = state.people.ada;
  const ctx = await chromium.launchPersistentContext(path.join(DATA, 'profile-phone'), {
    headless: true, viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true,
    userAgent: USER_AGENT,
    colorScheme: 'dark', reducedMotion: 'reduce', locale: 'en-GB', timezoneId: 'Europe/Stockholm', baseURL: publicUrl,
    args: [...chromiumMediaArgs(fixtures), `--remote-debugging-port=${BASE + 19}`], permissions: ['microphone', 'camera'],
  });
  const page = ctx.pages()[0] ?? await ctx.newPage();
  await addAuthenticator(page);
  const shot = async (n) => { await page.waitForTimeout(300); await page.screenshot({ path: path.join(OUT, `${n}.png`) }); log('shot', n); };
  await page.goto('/welcome');
  await byText(page, 'button', 'onboarding.connect.signIn').waitFor({ timeout: WAIT });
  await shot('p390-signal-signin-0-entry');
  await byText(page, 'button', 'onboarding.connect.signIn').click();
  await page.getByRole('heading', { name: /^Sign in to / }).waitFor({ timeout: WAIT });
  await page.getByLabel(t('signin.login.username'), { exact: true }).fill('ada');
  await page.getByLabel(t('signin.login.password'), { exact: true }).fill(ada1.password);
  await shot('p390-signal-signin-1-login');
  await page.getByLabel(t('signin.login.password'), { exact: true }).focus();
  await page.setViewportSize({ width: 390, height: 844 - 336 });
  await shot('p390-signal-signin-1-login-keyboard');
  await page.setViewportSize({ width: 390, height: 844 });
  await byText(page, 'button', 'signin.login.submit').click();
  await byText(page, 'heading', 'signin.totp.title').waitFor({ timeout: WAIT });
  await page.getByLabel(t('signin.totp.code'), { exact: true }).fill(await nextCode(controlUrl, ada1.totp, { wallClock: true }));
  await shot('p390-signal-signin-2-code');
  await byText(page, 'heading', 'signin.key.title').waitFor({ timeout: WAIT });
  await page.getByRole('textbox', { name: t('signin.key.label'), exact: true }).fill(ada1.recoveryKey);
  await shot('p390-signal-signin-3-recovery-key');
  await byText(page, 'button', 'signin.key.submit').click();
  await byText(page, 'heading', 'signin.done.title').waitFor({ timeout: WAIT });
  await shot('p390-signal-signin-4-done');
  await byText(page, 'button', 'signin.done.next').click();
  await page.waitForTimeout(3000);
  // Join the channel's MLS group before any seeded history is encrypted.
  await page.goto(state.serverPath);
  await composer(page, 'general').and(page.locator(':enabled')).waitFor({ timeout: WAIT });
  pages.phone = page;
  state.cdp.phone = BASE + 19;
  log('phone ready');
} catch (e) {
  state.ready = false; state.notReady = 'phone: ' + String(e).slice(0, 300); save();
  throw new Error('the phone browser did not sign in: ' + String(e).slice(0, 300));
}
await openChannel(ada, 'general');
await ada.waitForTimeout(3000);
const CONV = [
  ['ada', 'raid tonight at 20:00? bring the good shields'],
  ['bjorn', 'in. last time the bridge troll took everything we had'],
  ['mira', 'I’m in too. Packing list:\nrope\ntorches\nsnacks for Björn'],
  ['liv', 'I can navigate if someone lends me a boat. the fjord route is shorter but the weather has been awful all week, so plan for the long way round'],
  ['ada', 'the map from saturday is in #loot, the one with the coffee stain'],
  ['bjorn', 'see you at the bridge'],
];
for (const [who, body] of CONV) await send(pages[who], 'general', body);
log('conversation done');
// Seed one real reply so Q9's Threads button can reopen it directly. Visit the
// thread in both Ada profiles: the last-opened thread is local browser state.
const threadRoot = row(ada, 'general', CONV[1][1]);
// Keyboard focus keeps the toolbar present while arriving history can move the
// row away from the mouse. Use the normal visible action after focus.
await threadRoot.focus();
await threadRoot.getByRole('button', { name: t('thread.open'), exact: true }).click();
const threadReply = ada.getByRole('textbox', { name: t('thread.composer'), exact: true });
await threadReply.fill('two ropes. final answer.');
await threadReply.press('Enter');
await byText(ada, 'complementary', 'thread.title').getByText('two ropes. final answer.', { exact: true }).waitFor({ timeout: WAIT });
await byText(ada, 'button', 'thread.close').click();
const phoneRoot = row(pages.phone, 'general', CONV[1][1]);
await phoneRoot.getByRole('button', { name: t('thread.summary.one'), exact: true }).click();
await byText(pages.phone, 'complementary', 'thread.title').getByText('two ropes. final answer.', { exact: true }).waitFor({ timeout: WAIT });
await byText(pages.phone, 'button', 'thread.close').click();
log('thread done');

// Every member enters every text channel once (joins its group), then returns to #general.
for (const who of ['bjorn', 'mira']) {
  for (const n of [...TEXT, 'news']) { try { await openChannel(pages[who], n); } catch (e) { log(who, 'open', n, String(e).slice(0, 120)); } }
  await openChannel(pages[who], 'general');
}
for (const n of [...TEXT, 'news']) { try { await openChannel(ada, n); } catch (e) { log('ada open', n, String(e).slice(0, 120)); } }
await trySend(ada, 'news', 'Patch notes: the bridge is open again. Tolls are back to two coins.');
await openChannel(ada, 'general');
// Unread: Björn and Mira post where Ada is not reading.
await trySend(pages.bjorn, 'loot', 'the map is in the chest');
await trySend(pages.bjorn, 'loot', 'and the good rope');
await trySend(pages.mira, 'memes', 'when the troll asks for the toll twice');
await trySend(pages.mira, 'raid-planning-for-the-bridge-troll-and-the-long-winter-campaign', '@ada can you take the left flank?');
log('unreads done');
// A DM: Mira writes to Ada, through the `@` tile (web3.9 has no sidebar tabs).
try {
  const mira = pages.mira;
  await rail(mira).getByRole('button', { name: new RegExp(`^${escape(t('home.tile'))}(,|$)`) }).click();
  await byText(mira, 'button', 'shell.dms.new').click();
  const picker = byText(mira, 'dialog', 'shell.dms.newTitle');
  await picker.getByRole('listitem').filter({ hasText: 'Ada' }).getByRole('button', { name: t('shell.dms.open'), exact: true }).click();
  const dmBox = mira.getByRole('textbox', { name: t('shell.dm.composer', { name: 'Ada' }), exact: true });
  await dmBox.waitFor({ state: 'visible', timeout: WAIT });
  await dmBox.fill('see you at the bridge, Ada');
  await dmBox.press('Enter');
  await mira.waitForTimeout(2000);
  await rail(mira).getByRole('button', { name: new RegExp(`^${escape(SERVER)}`) }).click();
  log('dm done');
} catch (e) { log('dm failed', String(e).slice(0, 300)); }
// Liv goes offline (closes her browser).
await pages.liv.context().close();
delete state.cdp.liv;

// Voice: Björn and Mira join harbour (a click on a voice row joins on a desktop).
const joinVoice = async (page, name) => {
  await page.getByRole('button', { name: t('voice.open', { name }), exact: true }).first().click();
  await waitForVoiceJoin(page, { timeout: WAIT, joinLabel: t('callStage.preview.join') });
};
for (const who of ['bjorn', 'mira']) {
  try { await joinVoice(pages[who], 'harbour'); log(who, 'in voice'); } catch (e) {
    const diagnostics = { url: pages[who].url(), error: String(e),
      stage: await pages[who].locator('.dw-voice-stage').innerText({ timeout: 1000 }).catch(() => '(no stage)'),
      body: (await pages[who].locator('body').innerText({ timeout: 1000 }).catch(() => '(no body)')).slice(0, 8000) };
    writeFileSync(path.join(OUT, `voice-${who}-failure.json`), JSON.stringify(diagnostics, null, 2));
    await pages[who].screenshot({ path: path.join(OUT, `voice-${who}-failure.png`) }).catch(() => {});
    state.ready = false; state.notReady = `${who} voice: ${String(e).slice(0, 500)}`; save();
    throw new Error(state.notReady);
  }
}
// Mira shares her screen (the fake UI picks the screen), so the stage opens in the share focus layout, as in the prototype.
try {
  const controls = pages.mira.getByRole('toolbar', { name: t('voice.controls.voice'), exact: true });
  await controls.locator('[data-control="share"]').click();
  const picker = byText(pages.mira, 'dialog', 'callStage.share.title');
  await picker.waitFor({ timeout: WAIT });
  await picker.getByRole('button', { name: t('callStage.share.go'), exact: true }).click();
  await picker.waitFor({ state: 'hidden', timeout: WAIT });
  await pages.mira.locator('.d-call-tile[data-source="screen"]').first().waitFor({ timeout: WAIT });
  await pages.bjorn.locator('.d-call-tile[data-source="screen"]').first().waitFor({ timeout: WAIT });
  state.miraShares = true;
  log('mira shares her screen');
} catch (e) {
  state.ready = false; state.notReady = 'Mira screen share: ' + String(e).slice(0, 300); save();
  throw new Error(state.notReady);
}
// Optional real Music playback gives the voice Bot menu a real tile. A world
// still becomes ready when external provider tools/network are unavailable;
// record that boundary instead of silently capturing the member-list menu.
state.botPlays = false;
const missingMusicTools = [['yt-dlp', '--version'], ['ffmpeg', '-version'], ['bwrap', '--version']]
  .filter(([tool, versionFlag]) => spawnSync(tool, [versionFlag], { timeout: 5000, stdio: 'ignore' }).status !== 0).map(([tool]) => tool);
if (missingMusicTools.length > 0) {
  state.botPlayError = 'Missing Music provider tools: ' + missingMusicTools.join(', ');
  log('music fixture unavailable', state.botPlayError);
} else {
  const requester = pages.mira;
  try {
    // Channel-row navigation preserves Mira's joined call and live screen share.
    // Ada's capture reloads must never own the Music requester's voice lifecycle.
    await openChannel(requester, 'general');
    await composer(requester, 'general').fill('/');
    const command = requester.getByRole('textbox', { name: 'Command', exact: true });
    await command.waitFor({ timeout: WAIT });
    await command.fill('/play');
    await requester.getByRole('option').filter({ has: requester.getByText('/play', { exact: true }) }).waitFor({ timeout: WAIT });
    await command.press('Enter');
    await requester.getByRole('combobox', { name: 'query-or-url', exact: true }).fill('https://www.youtube.com/watch?v=aqz-KE-bpKQ');
    await requester.locator('button.d-composer__send').click();
    const botTile = pages.bjorn.locator('.d-call-tile[data-bot="true"]').first();
    await botTile.waitFor({ timeout: 90_000 });
    // Exercise the same independent Ada reload a parity pair performs. Mira
    // remains in voice and sharing throughout; the Bot must survive that reload.
    await ada.reload({ waitUntil: 'load' });
    await rail(ada).waitFor({ timeout: WAIT });
    await botTile.waitFor({ timeout: 10_000 });
    state.botPlays = true;
    log('Bot is in harbour after Ada reload');
  } catch (e) {
    state.botPlayError = String(e).slice(0, 300);
    log('music fixture unavailable', state.botPlayError);
  } finally {
    await requester.keyboard.press('Escape');
    // Keep the requester in the call. Only its visible text channel changes.
    await openChannel(requester, 'general');
  }
}
// The voice view's path, so the shots can open it without a click that joins.
state.voicePath = new URL(pages.bjorn.url()).pathname;
for (const who of ['bjorn', 'mira']) { try { await openChannel(pages[who], 'general'); } catch { /* the voice view stays */ } }
// The built-in AFK channel: Ada opens it once to learn its path and is back in #general. Since web3.11 the desktop click
// opens its page without marking her away; Go AFK is the explicit action.
try {
  await ada.getByRole('button', { name: t('voice.open', { name: 'AFK' }), exact: true }).first().click();
  await ada.waitForURL((u) => u.pathname !== new URL(state.voicePath, publicUrl).pathname && /\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/.test(u.pathname), { timeout: 10_000 });
  state.afkPath = new URL(ada.url()).pathname;
  const leave = ada.getByRole('button', { name: t('voice.leave'), exact: true }).first();
  if (await leave.isVisible().catch(() => false)) await leave.click();
  log('afk path', state.afkPath);
} catch (e) { log('no AFK row yet:', String(e).slice(0, 160)); }
await openChannel(ada, 'general');
state.serverPath = new URL(ada.url()).pathname;
save();

// A dummy Giphy key (8 to 64 letters and digits) so the composer's GIF button shows. The pickers then sit in their "searching"
// state: the world cannot reach Giphy. Nothing else reads the key. Set last: setting it earlier stalled the first message send.
{
  await byText(ada, 'button', 'admin.open').click().then(() => byText(ada, 'menuitem', 'signalSettings.menu.serverSettings').click());
  const gs = byText(ada, 'dialog', 'admin.title');
  await gs.getByRole('navigation', { name: t('admin.nav'), exact: true }).getByRole('button', { name: t('workspace.gifs'), exact: true }).click();
  await gs.getByLabel(t('gif.settings.key'), { exact: true }).fill('abcdEFGH12345678');
  await gs.getByRole('button', { name: t('gif.settings.save'), exact: true }).click();
  await gs.getByText(t('gif.settings.maskedLabel'), { exact: true }).waitFor({ timeout: WAIT });
  await closeSettings();
  log('dummy giphy key set');
}
state.ready = true;
save();
log('READY');
// Keep the voice participants alive.
setInterval(() => {}, 1 << 30);
const stopAll = () => { host.stop().finally(() => process.exit(0)); };
process.on('SIGTERM', stopAll);
process.on('SIGINT', stopAll);
