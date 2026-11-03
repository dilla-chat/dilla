import { desktopE2E, desktopApplication } from './support/electron.ts';
import { expect, type BrowserContext, type Page } from '@playwright/test';
import { copy, instanceInvite, joinCommunity, openChannel, sendText, signUp, WAIT } from './support/app';
import { test } from './support/app';

// Tab B is a second page of the first tab's context, opened at the app: the store, the Web Lock and BroadcastChannel
// are per origin and profile, so the second fixture's own persistent profile (support/second.ts) would be a fresh
// device at onboarding, never other-tab. The page closes in finally; the context is the fixture's.

async function reopenDesktop(page: Page, context: BrowserContext, action: 'close' | 'minimize' | 'activate'): Promise<void> {
  const application = desktopApplication(context);
  const id = await application.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].id);
  const workers = page.workers();
  expect(workers.length).toBeGreaterThan(0);
  if (action !== 'activate') {
    await application.evaluate(({ BrowserWindow }, action) => {
      const window = BrowserWindow.getAllWindows()[0];
      if (action === 'close') window.close(); else window.minimize();
    }, action);
    await expect.poll(() => application.evaluate(({ BrowserWindow }) => {
      const window = BrowserWindow.getAllWindows()[0]; return window.isMinimized() || !window.isVisible();
    })).toBe(true);
  }
  await application.evaluate(({ app }) => { app.emit('activate'); app.emit('activate'); });
  await expect.poll(() => application.evaluate(({ BrowserWindow }) => {
    const window = BrowserWindow.getAllWindows()[0]; return window.isVisible() && !window.isMinimized();
  })).toBe(true);
  expect(await application.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows().map(window => window.id))).toEqual([id]);
  expect(context.pages()).toHaveLength(1);
  expect(page.workers()).toHaveLength(workers.length);
  for (const worker of workers) expect(page.workers()).toContain(worker);
  const info = await page.evaluate(() => (window as any).dillaDesktop.info());
  expect(Object.keys(info).sort()).toEqual(['capabilities', 'instance', 'platform', 'version']);
  expect(info.instance).toBe(new URL('/', page.url()).href);
}

test(desktopE2E() ? 'closing to tray and reopening keeps the same enrolled instance' : 'the second tab takes the lock only after the first releases it', async ({ page, context, peer, instanceName }) => {
  const setup = await peer.setup({});
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  if (desktopE2E()) {
    await reopenDesktop(page, context, 'close');
    await sendText(page, peer.channelName, 'same desktop device sends after reopening');
    await peer.waitFor('same desktop device sends after reopening');
    return;
  }
  const other = { page: await context.newPage() };
  try {
    await other.page.goto('/');
    await expect(other.page.getByRole('button', { name: copy('tabs.use') })).toBeVisible({ timeout: WAIT });
    await other.page.getByRole('button', { name: copy('tabs.use') }).click();
    await expect(other.page.getByRole('textbox', { name: /message/i })).toBeVisible({ timeout: WAIT });
    await expect(page.getByRole('textbox', { name: /message/i })).toHaveCount(0);
    expect(await page.evaluate(() => document.activeElement !== document.body)).toBe(true);
    await sendText(other.page, peer.channelName, 'new leader sends');
    await peer.waitFor('new leader sends');
  } finally { await other.page.close(); }
});

test(desktopE2E() ? 'desktop reactivation exposes only public routing and capability metadata' : 'takeover notices contain exactly three public routing fields', async ({ page, context, peer, instanceName }) => {
  const setup = await peer.setup({});
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  if (desktopE2E()) { await reopenDesktop(page, context, 'activate'); return; }
  const install = () => {
    const w = window as typeof window & { __noticeKeys?: [string, string[]][] };
    w.__noticeKeys = [];
    const old = BroadcastChannel.prototype.postMessage;
    BroadcastChannel.prototype.postMessage = function (this: BroadcastChannel, value: unknown) {
      w.__noticeKeys!.push([this.name.split(':')[0], Object.keys(value as object).sort()]);
      return old.call(this, value);
    };
  };
  await page.evaluate(install);
  const other = { page: await context.newPage() };
  try {
    await other.page.goto('/');
    await other.page.evaluate(install);
    await other.page.getByRole('button', { name: copy('tabs.use') }).click();
    await expect(other.page.getByRole('textbox', { name: /message/i })).toBeVisible({ timeout: WAIT });
    const keys = [
      ...(await page.evaluate(() => (window as typeof window & { __noticeKeys: [string, string[]][] }).__noticeKeys)),
      ...(await other.page.evaluate(() => (window as typeof window & { __noticeKeys: [string, string[]][] }).__noticeKeys)),
    ];
    const takeover = keys.filter(([channel]) => channel === 'dilla-core');
    expect(takeover.length).toBeGreaterThan(0);
    for (const [, names] of takeover) expect(names).toEqual(['instance', 'nonce', 't']);
    // Product walk I7: the voice-presence channel carries only whether a call is on ({t, instance, on}) or the question
    // ({t, instance}); nothing else crosses tabs.
    for (const [channel, names] of keys.filter(([c]) => c !== 'dilla-core')) {
      expect(channel).toBe('dilla-voice');
      expect([['instance', 'on', 't'], ['instance', 't']]).toContainEqual(names);
    }
  } finally { await other.page.close(); }
});

// C1 hand-back: the paused tab takes dilla back with its own Use this tab (A -> B -> A), and works again.
test(desktopE2E() ? 'repeated minimize and restore preserves the worker and encrypted chat' : 'hand-over works both ways: A to B, then back to A', async ({ page, context, peer, instanceName }) => {
  const setup = await peer.setup({});
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  if (desktopE2E()) {
    await reopenDesktop(page, context, 'minimize');
    await reopenDesktop(page, context, 'close');
    await sendText(page, peer.channelName, 'same worker sends after minimizing and reopening');
    await peer.waitFor('same worker sends after minimizing and reopening');
    return;
  }
  const other = { page: await context.newPage() };
  try {
    await other.page.goto('/');
    await other.page.getByRole('button', { name: copy('tabs.use') }).click({ timeout: WAIT });
    await expect(other.page.getByRole('textbox', { name: /message/i })).toBeVisible({ timeout: WAIT });
    await expect(page.getByRole('heading', { name: copy('tabs.paused') })).toBeVisible({ timeout: WAIT });
    await expect(page.getByText(copy('tabs.pausedDetail'))).toBeVisible();
    // A takes it back from its paused view.
    await page.getByRole('button', { name: copy('tabs.use') }).click();
    await expect(page.getByRole('heading', { name: copy('tabs.paused') })).toHaveCount(0, { timeout: WAIT });
    await openChannel(page, peer.communityName, peer.channelName);
    await expect(other.page.getByRole('heading', { name: copy('tabs.paused') })).toBeVisible({ timeout: WAIT });
    await expect(other.page.getByRole('textbox', { name: /message/i })).toHaveCount(0);
    await sendText(page, peer.channelName, 'first tab leads again');
    await peer.waitFor('first tab leads again');
  } finally { await other.page.close(); }
});
