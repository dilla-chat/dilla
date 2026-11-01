// The UX audit world: an empty test host, then a realistic server built through the client only. It keeps every browser open
// (each with a CDP port) so ux:shots and the step runner can attach, writes state.json when ready, then holds until it is stopped.
//   npm run ux:world -w @dilla/e2e        (needs packages/web/dist, target/dilla-testhost and the core wasm built)
// Ports: host 8611/8612, SFU 7988/7990, browsers' CDP from 9611, Ada's phone 9620. Output: target/ui-screens/web3.9/.
import { spawn } from 'node:child_process';
import { mkdirSync, writeFileSync, rmSync } from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

const REPO = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const OUT = path.join(REPO, 'target', 'ui-screens', 'web3.9');
const require = createRequire(path.join(REPO, 'e2e', 'package.json'));
const { chromium } = require('@playwright/test');
const { t } = await import(path.join(REPO, 'packages/web/src/strings/index.ts'));
const { chromiumMediaArgs, writeFixtures } = await import(path.join(REPO, 'e2e/media/support/fixtures.ts'));
const { nextCode } = await import(path.join(REPO, 'e2e/web/support/totp.ts'));

const PORT = 8611, CONTROL = 8612, SFU = 7988, SFU_UDP = 7990;
const publicUrl = `http://127.0.0.1:${PORT}`;
const controlUrl = `http://127.0.0.1:${CONTROL}`;
const WAIT = 60_000;
const DATA = path.join(OUT, 'world');
rmSync(DATA, { recursive: true, force: true });
mkdirSync(DATA, { recursive: true });
const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);

const host = spawn(path.join(REPO, 'target/dilla-testhost'), [
  '-listen', `127.0.0.1:${PORT}`, '-control', `127.0.0.1:${CONTROL}`, '-core', path.join(REPO, 'internal/mlswasi/testdata/dilla_core_wasi.wasm'),
  '-log-level', 'warn', '-data-dir', path.join(DATA, 'host'), '-production-acl', '-web-root', path.join(REPO, 'packages/web/dist'),
  '-sfu', '-sfu-port', String(SFU), '-sfu-udp-port', String(SFU_UDP),
], { stdio: ['ignore', 'pipe', 'pipe'] });
const invite0 = await new Promise((res, rej) => {
  let seen = '';
  host.stdout.on('data', (b) => { seen += b; const m = /DILLA_TESTKIT_INVITE=(\S+)/.exec(seen); if (m) res(m[1].split(',')[0]); });
  host.stderr.on('data', (b) => { seen += b; });
  host.once('exit', (c) => rej(new Error('host exited ' + c + seen)));
});
log('host up');
const fixtures = writeFixtures(path.join(DATA, 'media'));
const state = { publicUrl, controlUrl, people: {}, cdp: {} };
const authenticators = {};
let cdpPort = 9611;
const save = () => writeFileSync(path.join(OUT, 'state.json'), JSON.stringify(state, null, 2));

async function open(who, extra = {}) {
  const port = cdpPort++;
  const ctx = await chromium.launchPersistentContext(path.join(DATA, `profile-${who}`), {
    headless: true, viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1, colorScheme: 'dark', reducedMotion: 'reduce',
    locale: 'en-GB', timezoneId: 'Europe/Stockholm', baseURL: publicUrl,
    args: [...chromiumMediaArgs(fixtures), `--remote-debugging-port=${port}`],
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

async function onboard(page, person, invite) {
  await page.goto(`/welcome?invite=${encodeURIComponent(invite)}`);
  await page.locator('.d-onboarding-frame h1').waitFor({ timeout: WAIT });
  await byText(page, 'button', 'onboarding.next').click();
  await byText(page, 'heading', 'onboarding.identity.title').waitFor({ timeout: WAIT });
  await page.getByLabel(t('onboarding.identity.username'), { exact: true }).fill(person.username);
  await page.getByLabel(t('onboarding.identity.display'), { exact: true }).fill(person.display);
  person.password = `ux-${person.username}-password`;
  const pw = page.getByLabel(t('onboarding.identity.password'), { exact: true });
  if (await pw.count() > 0) await pw.fill(person.password);
  await byText(page, 'button', 'onboarding.next').click();
  await byText(page, 'heading', 'onboarding.keys.title').waitFor({ timeout: WAIT });
  await page.locator('.d-recovery-key li').first().waitFor({ timeout: WAIT });
  person.recoveryKey = (await page.locator('.d-recovery-key li').allTextContents()).join(' ');
  await byText(page, 'checkbox', 'onboarding.keys.acknowledge').check();
  await byText(page, 'button', 'onboarding.next').click();
  await byText(page, 'heading', 'onboarding.browser.title').waitFor({ timeout: WAIT });
  await byText(page, 'button', 'onboarding.browser.submit').click();
  await byText(page, 'heading', 'factor.choose.title').waitFor({ timeout: WAIT });
  await page.getByRole('radio', { name: new RegExp(t('factor.choose.app')) }).check();
  await byText(page, 'button', 'factor.choose.next').click();
  await byText(page, 'heading', 'factor.totp.title').waitFor({ timeout: WAIT });
  await page.waitForFunction(() => /^[A-Z2-7]{16,}$/.test(document.querySelector('#factor-totp-key')?.value ?? ''), null, { timeout: WAIT });
  const a = { totpSecret: await page.getByLabel(t('factor.totp.key'), { exact: true }).inputValue(), lastCounter: -1 };
  authenticators[person.username] = a;
  await page.getByLabel(t('factor.totp.code'), { exact: true }).fill(await nextCode(controlUrl, a));
  await byText(page, 'button', 'factor.totp.submit').click();
  await byText(page, 'heading', 'factor.codes.title').waitFor({ timeout: WAIT });
  await byText(page, 'checkbox', 'factor.codes.ack').check();
  await byText(page, 'button', 'factor.codes.next').click();
  await byText(page, 'heading', 'onboarding.done.title').waitFor({ timeout: WAIT });
  await byText(page, 'button', 'onboarding.done.next').click();
  await rail(page).waitFor({ timeout: WAIT });
  person.totp = a;
  state.people[person.username] = person;
  log('onboarded', person.username);
}

const openChannel = async (page, name) => {
  await page.getByRole('navigation', { name: t('shell.channels.label'), exact: true }).getByRole('button', { name: new RegExp(`^${escape(name)}(,|$)`) }).first().click();
  await composer(page, name).and(page.locator(':enabled')).waitFor({ timeout: WAIT });
};
async function send(page, channel, text) {
  if (!(await composer(page, channel).isVisible().catch(() => false))) await openChannel(page, channel);
  const box = composer(page, channel);
  await box.and(page.locator(':not([readonly])')).waitFor({ timeout: WAIT });
  await box.fill(text);
  await box.press('Enter');
  const target = row(page, channel, text.split('\n')[0]);
  for (let i = 0; i < 3; i++) {
    await target.and(page.locator('[data-state="ok"], [data-state="failed"]')).last().waitFor({ timeout: WAIT });
    if (await target.last().getAttribute('data-state') !== 'failed') break;
    await target.last().getByRole('button', { name: t('shell.message.retry'), exact: true }).click();
    await page.waitForTimeout(1500);
  }
}
const trySend = async (...a) => { try { await send(...a); } catch (e) { log('send failed', a[1], String(e).slice(0, 200)); } };

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
await settings.getByRole('navigation', { name: t('admin.nav'), exact: true }).getByRole('button', { name: t('admin.nav.invites'), exact: true }).click();
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
  const join = page.locator('.dw-voice-stage').getByRole('button', { name: t('voice.join'), exact: true });
  await page.locator('[data-call-phase="in"]').first().waitFor({ timeout: 5000 }).catch(async () => { await join.click(); });
  await page.locator('[data-call-phase="in"]').first().waitFor({ timeout: WAIT });
};
for (const who of ['bjorn', 'mira']) {
  try { await joinVoice(pages[who], 'harbour'); log(who, 'in voice'); } catch (e) { log(who, 'voice failed', String(e).slice(0, 300)); }
}
// The voice view's path, so the shots can open it without a click that joins.
state.voicePath = new URL(pages.bjorn.url()).pathname;
for (const who of ['bjorn', 'mira']) { try { await openChannel(pages[who], 'general'); } catch { /* the voice view stays */ } }
// The built-in AFK channel: Ada opens it once to learn its path and is back in #general. Since web3.11 the desktop click
// marks her away (no call); a later post of hers in this server takes her off the away list.
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

// Ada's phone: a mobile-emulated Chromium profile (390x844, touch, coarse pointer), signed in as Ada's second device through the
// real sign-in flow. Its sign-in screens are shot on the way; ux:shots drives it afterwards (who = "phone").
try {
  const ada1 = state.people.ada;
  const ctx = await chromium.launchPersistentContext(path.join(DATA, 'profile-phone'), {
    headless: true, viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true,
    userAgent: USER_AGENT,
    colorScheme: 'dark', reducedMotion: 'reduce', locale: 'en-GB', timezoneId: 'Europe/Stockholm', baseURL: publicUrl,
    args: [...chromiumMediaArgs(fixtures), '--remote-debugging-port=9620'], permissions: ['microphone', 'camera'],
  });
  const page = ctx.pages()[0] ?? await ctx.newPage();
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
  await page.getByLabel(t('signin.totp.code'), { exact: true }).fill(await nextCode(controlUrl, ada1.totp));
  await shot('p390-signal-signin-2-code');
  await byText(page, 'button', 'signin.login.submit').click();
  await byText(page, 'heading', 'signin.key.title').waitFor({ timeout: WAIT });
  await page.getByRole('textbox', { name: t('signin.key.label'), exact: true }).fill(ada1.recoveryKey);
  await shot('p390-signal-signin-3-recovery-key');
  await byText(page, 'button', 'signin.key.submit').click();
  await byText(page, 'heading', 'signin.done.title').waitFor({ timeout: WAIT });
  await shot('p390-signal-signin-4-done');
  await byText(page, 'button', 'signin.done.next').click();
  await page.waitForTimeout(3000);
  state.cdp.phone = 9620;
  log('phone ready');
} catch (e) { log('phone failed', String(e).slice(0, 300)); }
state.ready = true;
save();
log('READY');
// Keep the voice participants alive.
setInterval(() => {}, 1 << 30);
process.on('SIGTERM', () => { host.kill('SIGTERM'); process.exit(0); });
process.on('SIGINT', () => { host.kill('SIGTERM'); process.exit(0); });
