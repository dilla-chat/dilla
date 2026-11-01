// The audit's screens, one named step per screen, shot from the running world (npm run ux:world -w @dilla/e2e).
//   node e2e/ux/shots.mjs                          every step at its default sizes and themes
//   node e2e/ux/shots.mjs --list                   the steps
//   node e2e/ux/shots.mjs shell dm --size 1440     named steps, one width
//   node e2e/ux/shots.mjs phone-servers --theme light
// Files are target/ui-screens/web3.9/<d|p><width>-<theme>-<step>.png. A step that cannot run says so and the rest go on.
// Desktop steps run in Ada's browser at 1440x900 and 1024x768 (signal, light and high-contrast); phone steps run in her phone
// (390x844 and 360x740; signal and light); `auth` runs in browsers of its own (the invite page, a sign-up, a sign-in).
import path from 'node:path';
import { connect, readState, OUT_ROOT, REPO } from './run.mjs';
import { createRequire } from 'node:module';

const require = createRequire(path.join(REPO, 'e2e', 'package.json'));
const esc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const wait = (h, ms = 500) => h.page.waitForTimeout(ms);

// ---------------------------------------------------------------------------------------------------
// Shared moves.

const channelsNav = (h) => h.page.getByRole('navigation', { name: h.t('shell.channels.label'), exact: true });
const railNav = (h) => h.page.getByRole('navigation', { name: h.t('shell.rail.label'), exact: true });
const row = (h, name) => channelsNav(h).getByRole('button', { name: new RegExp(`^${esc(name)}(,|$)`) }).first();
const tabs = (h) => h.page.getByRole('navigation', { name: h.t('nav.tabs.label'), exact: true });
const tab = (h, key) => tabs(h).getByRole('link', { name: new RegExp(`^${esc(h.t(key))}`) });
const back = (h) => h.page.getByRole('button', { name: h.t('nav.back'), exact: true });
const homeTile = (h) => railNav(h).getByRole('button', { name: new RegExp(`^${esc(h.t('home.tile'))}(,|$)`) });
const dmRow = (h) => h.page.getByRole('navigation', { name: h.t('shell.dms.label'), exact: true }).getByRole('button', { name: /^Mira/ }).first();
const composer = (h, channel) => h.page.getByRole('textbox', { name: h.t('shell.composer.label', { channel }), exact: true });
const url = (h, p) => new URL(p, h.state.publicUrl).href;

/** Desktop: the server's #general with nothing open on top of it. */
async function desktopHome(h) {
  await h.page.goto(url(h, h.state.serverPath));
  await h.page.locator('.d-app-shell').waitFor();
  await row(h, 'general').click();
  await composer(h, 'general').waitFor();
}
/** Phone: the home tab's list (rail and channels). */
async function phoneHome(h) {
  await h.page.goto(url(h, h.state.serverPath));
  await h.page.locator('.d-app-shell').waitFor();
  if (await back(h).isVisible().catch(() => false)) await back(h).click();
  await tab(h, 'home.tab').click();
  await row(h, 'general').waitFor();
}
/** A page of the voice stage without joining (a desktop click on a voice row joins). */
async function openPath(h, p, what) {
  if (p === undefined) throw new Error(`${what} is not in state.json (the world did not record it)`);
  await h.page.goto(url(h, p));
  await h.page.locator('.d-app-shell').waitFor();
  await wait(h, 800);
}
/** Joins a voice channel the way a person does and waits for the call. */
async function joinVoice(h, name) {
  if (await h.page.locator('[data-call-phase="in"]').first().isVisible().catch(() => false)) return;
  await h.page.getByRole('button', { name: h.t('voice.open', { name }), exact: true }).first().click();
  const join = h.page.locator('.dw-voice-stage').getByRole('button', { name: h.t('voice.join'), exact: true });
  await h.page.locator('[data-call-phase="in"]').first().waitFor({ timeout: 5000 }).catch(async () => { await join.click(); });
  await h.page.locator('[data-call-phase="in"]').first().waitFor({ timeout: 60_000 });
  await wait(h, 2500);
}

const desktop = { who: 'ada', sizes: [[1440, 900], [1024, 768]], themes: ['signal', 'light', 'high-contrast'], tag: 'd' };
const desktopSignal = { ...desktop, themes: ['signal'] };
const desktop1440 = { ...desktop, sizes: [[1440, 900]], themes: ['signal'] };
const phone = { who: 'phone', sizes: [[390, 844], [360, 740]], themes: ['signal', 'light'], tag: 'p' };
const phoneSignal = { ...phone, themes: ['signal'] };

// ---------------------------------------------------------------------------------------------------
// The steps. Each is `{ ...matrix, run(h, v) }`, `v` being { name, tag, w, h, theme }.

export const steps = {
  // ---- desktop
  shell: { ...desktop, async run(h, v) {
    await desktopHome(h); await h.settle();
    await h.shoot(`${v.tag}-shell`);
  } },
  'composer-focus': { ...desktop, async run(h, v) {
    await desktopHome(h);
    await composer(h, 'general').focus();
    await h.settle({ keepFocus: true });
    await h.shoot(`${v.tag}-composer-focus`);
  } },
  'voice-preview': { ...desktop, async run(h, v) {
    await openPath(h, h.state.voicePath, 'the voice view'); await h.settle();
    await h.shoot(`${v.tag}-voice-preview`);
  } },
  'voice-afk': { ...desktop, async run(h, v) {
    await openPath(h, h.state.afkPath, 'the AFK channel (the world finds no AFK row)'); await h.settle();
    await h.shoot(`${v.tag}-voice-afk`);
  } },
  announcement: { ...desktop, async run(h, v) {
    await desktopHome(h);
    await row(h, 'news').click(); await wait(h, 600); await h.settle();
    await h.shoot(`${v.tag}-announcement`);
  } },
  dm: { ...desktop, async run(h, v) {
    await desktopHome(h);
    await homeTile(h).click(); await wait(h, 400);
    if (await dmRow(h).isVisible().catch(() => false)) { await dmRow(h).click(); await wait(h, 800); }
    await h.settle();
    await h.shoot(`${v.tag}-dm`);
  } },
  'server-settings': { ...desktop, async run(h, v) {
    await desktopHome(h);
    // Signal task 10 fix round 2: the server ⌄ is a menu; its first item opens the server's settings.
    await h.page.locator('[data-focus-key="admin-open"]').first().click();
    await h.page.getByRole('menuitem', { name: h.t('signalSettings.menu.serverSettings'), exact: true }).click();
    const admin = h.page.getByRole('dialog', { name: h.t('admin.title'), exact: true });
    await admin.waitFor();
    const nav = admin.getByRole('navigation', { name: h.t('admin.nav'), exact: true }).getByRole('button');
    const n = await nav.count();
    for (let i = 0; i < n; i++) {
      const label = (await nav.nth(i).innerText()).trim().replace(/\W+/g, '-');
      await nav.nth(i).click(); await wait(h); await h.settle();
      await h.shoot(`${v.tag}-server-settings-${i}-${label}`);
    }
    await h.page.keyboard.press('Escape');
    await admin.waitFor({ state: 'hidden' });
  } },
  'user-settings': { ...desktop, async run(h, v) {
    await desktopHome(h);
    await h.page.getByRole('button', { name: h.t('panel.settings'), exact: true }).click();
    const dialog = h.page.getByRole('dialog', { name: h.t('settings.title'), exact: true });
    await dialog.waitFor();
    const nav = dialog.getByRole('navigation', { name: h.t('settings.nav.label'), exact: true }).getByRole('button');
    const n = await nav.count();
    for (let i = 0; i < n; i++) {
      const label = (await nav.nth(i).innerText()).trim().replace(/\W+/g, '-');
      await nav.nth(i).click(); await wait(h); await h.settle();
      await h.shoot(`${v.tag}-user-settings-${i}-${label}`);
    }
    await h.page.keyboard.press('Escape');
    await dialog.waitFor({ state: 'hidden' });
  } },
  search: { ...desktop, async run(h, v) {
    await desktopHome(h);
    await h.page.keyboard.press('Control+k'); await wait(h);
    await h.page.keyboard.type('bridge'); await wait(h, 1500);
    await h.settle({ keepFocus: true });
    await h.shoot(`${v.tag}-search`);
    await h.page.keyboard.press('Escape'); await wait(h, 300);
  } },
  'list-hover': { ...desktop1440, async run(h, v) {
    await desktopHome(h); await h.settle();
    const clip = { x: 0, y: 0, width: 330, height: 560 };
    await channelsNav(h).getByRole('button', { name: h.t('voice.open', { name: 'harbour' }), exact: true }).hover(); await wait(h, 300);
    await h.shoot(`${v.tag}-list-hover-voice`, { clip });
    await row(h, 'memes').hover(); await wait(h, 300);
    await h.shoot(`${v.tag}-list-hover-text`, { clip });
    const plus = channelsNav(h).getByRole('button', { name: h.t('create.channel.open'), exact: true });
    if (await plus.isVisible().catch(() => false)) { await plus.hover(); await wait(h, 300); await h.shoot(`${v.tag}-list-hover-plus`, { clip }); }
    await h.page.mouse.move(-10, -10);
    await row(h, 'general').focus(); await h.page.keyboard.press('ArrowDown'); await wait(h, 300);
    await h.shoot(`${v.tag}-list-focus-next`, { clip });
    await h.page.keyboard.press('ArrowDown'); await wait(h, 300);
    await h.shoot(`${v.tag}-list-focus-next-2`, { clip });
  } },
  'header-overflow': { ...desktop, sizes: [[1024, 768]], async run(h, v) {
    await desktopHome(h); await h.settle();
    await h.shoot(`${v.tag}-header-overflow`, { clip: { x: 300, y: 0, width: 724, height: 60 } });
  } },
  'header-announcement': { ...desktop1440, async run(h, v) {
    await desktopHome(h);
    await row(h, 'news').click(); await wait(h, 800); await h.settle();
    await h.shoot(`${v.tag}-header-announcement`, { clip: { x: 300, y: 0, width: 600, height: 60 } });
  } },
  'stage-grid': { ...desktop, async run(h, v) {
    await desktopHome(h);
    await joinVoice(h, 'harbour'); await h.settle();
    await h.shoot(`${v.tag}-stage-grid`);
  } },
  'stage-focused': { ...desktopSignal, async run(h, v) {
    await desktopHome(h);
    await joinVoice(h, 'harbour');
    await h.page.locator('.d-call-tile__focus').first().click(); await wait(h, 800); await h.settle();
    await h.shoot(`${v.tag}-stage-focused`);
    await h.page.keyboard.press('Escape');
  } },
  dock: { ...desktop, async run(h, v) {
    await desktopHome(h);
    await joinVoice(h, 'harbour');
    await row(h, 'general').click(); await wait(h, 1500); await h.settle();
    await h.shoot(`${v.tag}-dock`);
    await h.shoot(`${v.tag}-dock-crop`, { clip: { x: 0, y: v.h - 400, width: 330, height: 400 } });
  } },

  // ---- phone
  'phone-servers': { ...phone, async run(h, v) {
    await phoneHome(h); await h.settle();
    await h.shoot(`${v.tag}-servers`);
    await h.page.locator('.d-channel-list__list').evaluate((e) => { e.scrollTop = e.scrollHeight; });
    await h.settle();
    await h.shoot(`${v.tag}-servers-scrolled`);
  } },
  'phone-channel': { ...phone, async run(h, v) {
    await phoneHome(h);
    await row(h, 'general').click(); await wait(h, 1000); await h.settle();
    await h.shoot(`${v.tag}-channel`);
  } },
  'phone-channel-keyboard': { ...phone, async run(h, v) {
    // The keyboard is simulated through the visual viewport, as iOS does (the app compares it with the window's height).
    const kb = v.w === 390 ? 336 : 291;
    await phoneHome(h);
    await row(h, 'general').click(); await wait(h, 800);
    await h.page.evaluate((kbh) => {
      const vv = window.visualViewport;
      const height = window.innerHeight - kbh;
      Object.defineProperty(vv, 'height', { configurable: true, get: () => height });
      Object.defineProperty(vv, 'offsetTop', { configurable: true, get: () => 0 });
    }, kb);
    await composer(h, 'general').focus();
    await h.page.evaluate(() => { window.visualViewport.dispatchEvent(new Event('resize')); window.dispatchEvent(new Event('resize')); });
    await wait(h, 600); await h.settle({ keepFocus: true });
    await h.shoot(`${v.tag}-channel-keyboard-vv`, { clip: { x: 0, y: 0, width: v.w, height: v.h - kb } });
    await h.page.evaluate(() => { const vv = window.visualViewport; delete vv.height; delete vv.offsetTop; document.activeElement?.blur(); vv.dispatchEvent(new Event('resize')); });
  } },
  'phone-sheets': { ...phone, async run(h, v) {
    await phoneHome(h);
    await row(h, 'general').click(); await wait(h, 800);
    const menu = h.page.getByRole('button', { name: h.t('nav.channel.menu'), exact: true });
    await menu.click(); await wait(h); await h.settle({ keepFocus: true });
    await h.shoot(`${v.tag}-sheet-channel-menu`);
    const members = h.page.getByRole('menuitem', { name: h.t('members.toggle'), exact: true });
    if (await members.isVisible().catch(() => false)) {
      await members.click(); await wait(h, 600); await h.settle();
      await h.shoot(`${v.tag}-sheet-members`);
      const close = h.page.getByRole('button', { name: h.t('members.close'), exact: true });
      if (await close.isVisible().catch(() => false)) await close.click(); else await h.page.keyboard.press('Escape');
    } else await h.page.keyboard.press('Escape');
    await wait(h, 400);
    await phoneHome(h);
    const server = h.page.getByRole('button', { name: h.t('nav.server.menu'), exact: true });
    if (await server.isVisible().catch(() => false)) { await server.click(); await wait(h); await h.settle({ keepFocus: true }); await h.shoot(`${v.tag}-sheet-server-menu`); await h.page.keyboard.press('Escape'); }
  } },
  'phone-voice-preview': { ...phone, async run(h, v) {
    await openPath(h, h.state.voicePath, 'the voice view'); await h.settle();
    await h.shoot(`${v.tag}-voice-preview`);
  } },
  'phone-dms': { ...phone, async run(h, v) {
    await phoneHome(h);
    await homeTile(h).click(); await wait(h, 600); await h.settle();
    await h.shoot(`${v.tag}-dms`);
    if (await dmRow(h).isVisible().catch(() => false)) { await dmRow(h).click(); await wait(h, 800); await h.settle(); await h.shoot(`${v.tag}-dm-open`); }
  } },
  'phone-activity': { ...phone, async run(h, v) {
    await phoneHome(h);
    await tab(h, 'nav.tab.activity').click(); await wait(h, 600); await h.settle();
    await h.shoot(`${v.tag}-activity`);
  } },
  'phone-you': { ...phone, async run(h, v) {
    await phoneHome(h);
    await tab(h, 'nav.tab.you').click(); await wait(h, 600); await h.settle();
    await h.shoot(`${v.tag}-you`);
    await h.page.locator('.d-app-shell__sidebar, .dw-phone-pane').first().evaluate((e) => { e.scrollTop = 9999; }).catch(() => {});
    await h.settle(); await h.shoot(`${v.tag}-you-scrolled`);
  } },
  'phone-settings': { ...phone, async run(h, v) {
    await phoneHome(h);
    await tab(h, 'nav.tab.you').click(); await wait(h, 600);
    const sections = h.page.locator('[data-settings-section]');
    const n = await sections.count();
    for (let i = 0; i < n; i++) {
      const s = sections.nth(i);
      const name = await s.getAttribute('data-settings-section');
      await s.scrollIntoViewIfNeeded().catch(() => {});
      await s.click(); await wait(h, 600); await h.settle();
      await h.shoot(`${v.tag}-settings-${name}`);
      await back(h).click().catch(() => h.page.keyboard.press('Escape')); await wait(h, 400);
    }
  } },
  'phone-stage': { ...phoneSignal, async run(h, v) {
    await phoneHome(h);
    await h.page.getByRole('button', { name: h.t('voice.open', { name: 'harbour' }), exact: true }).first().click(); await wait(h, 800);
    if (!(await h.page.locator('[data-call-phase="in"]').first().isVisible().catch(() => false))) {
      await h.page.locator('.dw-voice-stage').getByRole('button', { name: h.t('voice.join'), exact: true }).click();
      await h.page.locator('[data-call-phase="in"]').first().waitFor({ timeout: 60_000 }); await wait(h, 2500);
    }
    await h.settle(); await h.shoot(`${v.tag}-stage`);
    await h.page.locator('.d-call-stage').evaluate((e) => { (e.closest('.dw-voice-stage') ?? e).scrollTop = 9999; }).catch(() => {});
    await h.settle(); await h.shoot(`${v.tag}-stage-scrolled`);
    await h.page.locator('.d-call-tile__focus').nth(1).click().catch(() => {}); await wait(h, 600); await h.settle();
    await h.shoot(`${v.tag}-stage-focused`);
    await h.page.keyboard.press('Escape').catch(() => {});
    await back(h).click().catch(() => {}); await wait(h, 500); await h.settle();
    await h.shoot(`${v.tag}-servers-in-call`);
    await row(h, 'general').click(); await wait(h, 800); await h.settle();
    await h.shoot(`${v.tag}-channel-in-call`);
  } },
};

// ---------------------------------------------------------------------------------------------------
// The sign-up and sign-in screens: browsers of their own, through the invite and through Björn's account.

steps.auth = { who: null, sizes: [[1440, 900]], themes: ['signal'], tag: 'd', async run() { await authScreens(); } };

async function authScreens() {
  const { chromium } = require('@playwright/test');
  const { t } = await import(path.join(REPO, 'packages/web/src/strings/index.ts'));
  const { nextCode } = await import(path.join(REPO, 'e2e/web/support/totp.ts'));
  const state = readState();
  const browser = await chromium.launch({ headless: true });
  const variants = [['d1440-signal', 1440, 900, 'dark', 'no-preference'], ['d1440-light', 1440, 900, 'light', 'no-preference'], ['d1440-high-contrast', 1440, 900, 'dark', 'more'],
    ['d1024-signal', 1024, 768, 'dark', 'no-preference'], ['p390-signal', 390, 844, 'dark', 'no-preference']];
  const open = async () => {
    const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, colorScheme: 'dark', reducedMotion: 'reduce', locale: 'en-GB', baseURL: state.publicUrl });
    return { ctx, page: await ctx.newPage() };
  };
  const shotAll = async (page, name) => {
    await page.mouse.move(-10, -10);
    for (const [tag, w, h, cs, contrast] of variants) {
      await page.setViewportSize({ width: w, height: h });
      await page.emulateMedia({ colorScheme: cs, contrast });
      await page.waitForTimeout(250);
      await page.screenshot({ path: path.join(OUT_ROOT, `${tag}-${name}.png`) });
    }
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.emulateMedia({ colorScheme: 'dark', contrast: 'no-preference' });
    console.log('shot', name);
  };
  try {
    const { ctx, page } = await open();
    const by = (role, key) => page.getByRole(role, { name: t(key), exact: true });
    await page.goto(state.invitePath); await page.waitForTimeout(1500);
    await shotAll(page, 'invite-landing');
    const invite = decodeURIComponent(state.invitePath.slice(3));
    await page.goto(`/welcome?invite=${encodeURIComponent(invite)}`);
    await page.locator('.d-onboarding-frame h1').waitFor({ timeout: 60_000 });
    await shotAll(page, 'signup-1-invite');
    await by('button', 'onboarding.next').click();
    await by('heading', 'onboarding.identity.title').waitFor({ timeout: 60_000 });
    await shotAll(page, 'signup-2-identity-empty');
    await page.getByLabel(t('onboarding.identity.username'), { exact: true }).fill('sigrid');
    await page.getByLabel(t('onboarding.identity.display'), { exact: true }).fill('Sigrid');
    const pw = page.getByLabel(t('onboarding.identity.password'), { exact: true });
    if (await pw.count() > 0) await pw.fill('ux-sigrid-password');
    await shotAll(page, 'signup-2-identity');
    await by('button', 'onboarding.next').click();
    await by('heading', 'onboarding.keys.title').waitFor({ timeout: 60_000 });
    await page.locator('.d-recovery-key li').first().waitFor({ timeout: 60_000 });
    await shotAll(page, 'signup-3-recovery-key');
    await by('checkbox', 'onboarding.keys.acknowledge').check();
    await by('button', 'onboarding.next').click();
    await by('heading', 'onboarding.browser.title').waitFor({ timeout: 60_000 });
    await shotAll(page, 'signup-4-browser');
    await by('button', 'onboarding.browser.submit').click();
    await by('heading', 'factor.choose.title').waitFor({ timeout: 60_000 });
    await shotAll(page, 'signup-5-factor-choose');
    await page.getByRole('radio', { name: new RegExp(t('factor.choose.app')) }).check();
    await by('button', 'factor.choose.next').click();
    await by('heading', 'factor.totp.title').waitFor({ timeout: 60_000 });
    await page.waitForFunction(() => /^[A-Z2-7]{16,}$/.test(document.querySelector('#factor-totp-key')?.value ?? ''), null, { timeout: 60_000 });
    await shotAll(page, 'signup-6-factor-app');
    const a = { totpSecret: await page.getByLabel(t('factor.totp.key'), { exact: true }).inputValue(), lastCounter: -1 };
    await page.getByLabel(t('factor.totp.code'), { exact: true }).fill(await nextCode(state.controlUrl, a));
    await by('button', 'factor.totp.submit').click();
    await by('heading', 'factor.codes.title').waitFor({ timeout: 60_000 });
    await shotAll(page, 'signup-7-factor-codes');
    await by('checkbox', 'factor.codes.ack').check();
    await by('button', 'factor.codes.next').click();
    await by('heading', 'onboarding.done.title').waitFor({ timeout: 60_000 });
    await shotAll(page, 'signup-8-done');
    await ctx.close();

    // Björn, on a fresh browser.
    const second = await open();
    const pg = second.page;
    const by2 = (role, key) => pg.getByRole(role, { name: t(key), exact: true });
    const bj = state.people.bjorn;
    await pg.goto('/welcome');
    await by2('button', 'onboarding.connect.signIn').waitFor({ timeout: 60_000 });
    await shotAll(pg, 'signin-0-entry');
    await by2('button', 'onboarding.connect.signIn').click();
    await pg.getByRole('heading', { name: /^Sign in to / }).waitFor({ timeout: 60_000 });
    await pg.getByLabel(t('signin.login.username'), { exact: true }).fill('bjorn');
    await pg.getByLabel(t('signin.login.password'), { exact: true }).fill(bj.password);
    await shotAll(pg, 'signin-1-login');
    await by2('button', 'signin.login.submit').click();
    await by2('heading', 'signin.totp.title').waitFor({ timeout: 60_000 });
    await pg.getByLabel(t('signin.totp.code'), { exact: true }).fill(await nextCode(state.controlUrl, bj.totp));
    await shotAll(pg, 'signin-2-code');
    await by2('button', 'signin.login.submit').click();
    await by2('heading', 'signin.key.title').waitFor({ timeout: 60_000 });
    await pg.getByRole('textbox', { name: t('signin.key.label'), exact: true }).fill(bj.recoveryKey);
    await shotAll(pg, 'signin-3-recovery-key');
    await by2('button', 'signin.key.submit').click();
    await by2('heading', 'signin.done.title').waitFor({ timeout: 60_000 });
    await shotAll(pg, 'signin-4-done');
    await second.ctx.close();
  } finally { await browser.close(); }
}

// ---------------------------------------------------------------------------------------------------
// The runner.

const argv = process.argv.slice(2);
const opt = (name) => { const i = argv.indexOf(`--${name}`); return i === -1 ? undefined : argv[i + 1]; };
if (argv.includes('--list')) {
  for (const [name, s] of Object.entries(steps)) console.log(name.padEnd(24), s.who ?? '(own browsers)', s.sizes.map((z) => z[0]).join('/'), s.themes.join('/'));
  process.exit(0);
}
const valued = new Set(['--size', '--theme']);
const wanted = argv.filter((a, i) => !a.startsWith('--') && !valued.has(argv[i - 1]));
const names = wanted.length === 0 ? Object.keys(steps) : wanted;
const unknown = names.filter((n) => steps[n] === undefined);
if (unknown.length > 0) { console.error(`unknown steps: ${unknown.join(', ')} (node e2e/ux/shots.mjs --list)`); process.exit(2); }
const onlySize = opt('size')?.split(',').map(Number);
const onlyTheme = opt('theme')?.split(',');

const failures = [];
const conns = new Map();
try {
  for (const name of names) {
    const step = steps[name];
    if (step.who === null) {
      try { await step.run(); } catch (e) { failures.push(`${name}: ${String(e).split('\n')[0]}`); console.log('FAILED', name, String(e).split('\n')[0]); }
      continue;
    }
    if (!conns.has(step.who)) conns.set(step.who, await connect(step.who));
    const h = conns.get(step.who);
    for (const [w, ht] of step.sizes) {
      if (onlySize !== undefined && !onlySize.includes(w)) continue;
      for (const theme of step.themes) {
        if (onlyTheme !== undefined && !onlyTheme.includes(theme)) continue;
        const v = { name, w, h: ht, theme, tag: `${step.tag}${w}-${theme}` };
        try {
          await h.page.setViewportSize({ width: w, height: ht });
          await h.theme(theme);
          await step.run(h, v);
        } catch (e) { failures.push(`${name} ${v.tag}: ${String(e).split('\n')[0]}`); console.log('FAILED', name, v.tag, String(e).split('\n')[0]); }
        await h.page.keyboard.press('Escape').catch(() => {});
      }
    }
  }
} finally { for (const h of conns.values()) await h.close(); }
if (failures.length > 0) { console.log(`\n${failures.length} step runs failed:\n  ${failures.join('\n  ')}`); process.exit(1); }
