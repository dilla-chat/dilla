// Bot v2 task 20: the picker reads the Bot's commands from the catalog held on this device, and a Bot command is one sealed
// request whose answers come back over the socket. Linux-only (it runs the managed Bot runtime): listed with the other
// Bot specs in the Linux-only testIgnore of playwright.desktop.config.ts.
import { expect, type Locator, type Page } from '@playwright/test';
import { addServerVia, chooseBotCapabilities, composerBox, copy, enableBotButton, openChannel, signUp, test } from './support/app';
import { prepareBotRuntimeBinaries, startTestHost, type HostConfig, type StartedHost } from './support/host';
import { installWorkerPort, portSlice } from './support/worker-port';
import { e2ePort } from './support/ports';

const PORT = Number(process.env.DILLA_E2E_BOT_COMMANDS_PORT ?? e2ePort(8611));
const HOST: HostConfig = { port: PORT, control: PORT + 1, webRoot: 'packages/web/dist', botRuntime: true, advanceClockWithWall: true };
const COMMUNITY = 'Sealed command acceptance';
const GENERAL = 'general';
const WAIT = process.env.CI === 'true' ? 90_000 : 30_000;
const BOT_WAIT = 90_000;
/** G2: Enter to the first answer, at p95, within the connection's RTT plus this. */
const G2_HEADROOM_MS = 100;
const G2_RUNS = 20;

test.use({ baseURL: `http://127.0.0.1:${String(PORT)}` });
test.use({ actionTimeout: WAIT });
test.describe.configure({ timeout: 360_000 });

test.beforeAll(async () => {
  test.setTimeout(360_000);
  await prepareBotRuntimeBinaries();
});

async function createCommunity(page: Page): Promise<void> {
  await addServerVia(page, 'create');
  const dialog = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await dialog.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(COMMUNITY);
  await page.keyboard.press('Enter');
  await openChannel(page, COMMUNITY, GENERAL);
}

/** Enables the built-in Bot with the given capabilities through the shipped settings and waits for it to be ready. */
async function enableBot(page: Page, password: string, capabilities: string[]): Promise<void> {
  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click()
    .then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  const settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
  const nav = settings.getByRole('navigation', { name: copy('admin.nav'), exact: true });
  await expect(nav).toBeVisible({ timeout: WAIT });
  await nav.getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
  await chooseBotCapabilities(settings, capabilities);
  await enableBotButton(settings).click();
  const stepUp = page.getByRole('dialog', { name: copy('factorSettings.stepUp.title'), exact: true });
  await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(password);
  await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();
  await expect(settings.locator('.dw-bot-settings').getByRole('status').filter({ hasText: copy('admin.bot.status.ready') }))
    .toHaveText(copy('admin.bot.status.ready'), { timeout: BOT_WAIT });
  await settings.getByRole('button', { name: copy('settings.close'), exact: true }).click();
  await expect(settings).toBeHidden();
}

function commandRegion(page: Page): Locator {
  return page.getByRole('region', { name: 'Command composer', exact: true });
}

/** Opens the `/` list from the channel composer. */
async function openList(page: Page): Promise<Locator> {
  await composerBox(page, GENERAL).fill('/');
  const region = commandRegion(page);
  await expect(region.getByRole('textbox', { name: 'Command', exact: true })).toBeVisible({ timeout: WAIT });
  return region;
}

async function closeList(page: Page): Promise<void> {
  await commandRegion(page).getByRole('textbox', { name: 'Command', exact: true }).press('Escape');
  await expect(commandRegion(page)).toHaveCount(0);
  await composerBox(page, GENERAL).fill('');
}

/** The last private command result in the feed. */
function commandResult(page: Page): Locator {
  return page.locator('.d-bot-result .d-message-row__body').last();
}

/** The worker messages the page posts, so a test can tell that opening the picker asked the worker for nothing. */
async function traceWorkerCalls(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const root = window as unknown as { __botCommandsCalls?: string[] };
    root.__botCommandsCalls = [];
    const OriginalWorker = window.Worker;
    window.Worker = new Proxy(OriginalWorker, {
      construct(target, args, newTarget) {
        const worker = Reflect.construct(target, args, newTarget) as Worker;
        const post = worker.postMessage.bind(worker);
        worker.postMessage = (message: unknown, transfer?: Transferable[] | StructuredSerializeOptions) => {
          const m = (message as { command?: { m?: unknown } } | null)?.command?.m;
          if (typeof m === 'string') root.__botCommandsCalls?.push(m);
          post(message, transfer as Transferable[]);
        };
        return worker;
      },
    }) as typeof Worker;
  });
}

function workerCalls(page: Page): Promise<string[]> {
  return page.evaluate(() => [...((window as unknown as { __botCommandsCalls?: string[] }).__botCommandsCalls ?? [])]);
}

async function stopAppBeforeHost(page: Page, host: StartedHost | null): Promise<void> {
  await page.goto('about:blank').catch(() => undefined);
  await host?.stop();
}

test('picker shows Bot commands with no network after sync', async ({ page, instanceName }) => {
  let host: StartedHost | null = null;
  try {
    await traceWorkerCalls(page);
    host = await startTestHost(HOST);
    const owner = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, 'Ada', host.control);
    await createCommunity(page);
    await enableBot(page, owner.password, ['reminders', 'music']);
    // The catalog arrives by push (op 61) or the connect sync; once the list shows it, it is held on this device.
    const synced = await openList(page);
    await expect(synced.getByRole('option', { name: /^\/remind,/u })).toBeVisible({ timeout: BOT_WAIT });
    await closeList(page);

    await page.context().setOffline(true);
    const before = (await workerCalls(page)).length;
    const region = await openList(page);
    await expect(region.getByRole('option', { name: /^\/remind,/u })).toBeVisible();
    await expect(region.getByRole('option', { name: /^\/play,/u })).toBeVisible();
    await expect(region.getByText(copy('commands.loading'), { exact: true })).toHaveCount(0);
    // Opening the list asked the worker for nothing: no status read, no capability round trip. Typing `/` in the composer
    // still tells the channel the member is typing (the composer's own `typing` call), which is no Bot request.
    expect((await workerCalls(page)).slice(before).filter(m => m !== 'typing')).toEqual([]);
    await page.context().setOffline(false);
  } finally { await stopAppBeforeHost(page, host); }
});

test('/remind round trip', async ({ page, instanceName }) => {
  let host: StartedHost | null = null;
  try {
    host = await startTestHost(HOST);
    const owner = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, 'Ada', host.control);
    await createCommunity(page);
    await enableBot(page, owner.password, ['reminders']);
    const reminderText = `sealed reminder ${Date.now()}`;
    const list = await openList(page);
    await list.getByRole('textbox', { name: 'Command', exact: true }).fill('/remind');
    await expect(list.getByRole('option', { name: /^\/remind,/u })).toBeVisible({ timeout: BOT_WAIT });
    await list.getByRole('textbox', { name: 'Command', exact: true }).press('Enter');
    const when = list.getByRole('combobox', { name: 'when', exact: true });
    await when.fill('in 1 minute');
    await when.press('Tab');
    const text = list.getByRole('textbox', { name: 'text', exact: true });
    await text.fill(reminderText);
    await text.press('Enter');
    // The Bot's sealed answer, opened on this device.
    await expect(commandResult(page)).toContainText('Saved', { timeout: BOT_WAIT });
    // The host's clock runs with the wall clock: the reminder falls due a minute later and shows in the inbox's Due tab.
    await page.getByRole('button', { name: 'Reminders', exact: true }).click();
    const inbox = page.getByRole('dialog', { name: 'Reminders', exact: true });
    await expect.poll(async () => {
      await inbox.getByRole('tab', { name: /^Due/u }).click();
      return inbox.getByText(reminderText).count();
    }, { timeout: 180_000, intervals: [2_000, 5_000], message: 'the reminder should fall due and show after one minute' }).toBeGreaterThan(0);
  } finally { await stopAppBeforeHost(page, host); }
});

test('Enter to first answer is within RTT + 100 ms at p95', async ({ page, instanceName }) => {
  let host: StartedHost | null = null;
  try {
    await installWorkerPort(page.context());
    host = await startTestHost(HOST);
    const owner = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, 'Ada', host.control);
    await createCommunity(page);
    await enableBot(page, owner.password, ['reminders']);
    const warm = await openList(page);
    await expect(warm.getByRole('option', { name: /^\/remind,/u })).toBeVisible({ timeout: BOT_WAIT });
    await closeList(page);
    // The RTT the gateway measures from its heartbeat ack (the connection slice's pingMs).
    await expect.poll(async () => (await portSlice<{ pingMs?: number }>(page, 'connection'))?.pingMs ?? null,
      { timeout: 120_000, intervals: [1_000, 2_000, 5_000] }).not.toBeNull();
    const rtt = (await portSlice<{ pingMs?: number }>(page, 'connection'))?.pingMs ?? 0;

    // Enter (captured before the picker sees it) to the moment the private result row first shows the Bot's answer.
    await page.evaluate(() => {
      const root = window as unknown as { __g2?: { start: number; first: number; pending: string } };
      root.__g2 = { start: 0, first: 0, pending: 'Saving your reminder…' };
      document.addEventListener('keydown', event => {
        if (event.key === 'Enter' && root.__g2 !== undefined && root.__g2.start === 0) root.__g2.start = performance.now();
      }, { capture: true });
      new MutationObserver(() => {
        const g2 = root.__g2;
        if (g2 === undefined || g2.start === 0 || g2.first !== 0) return;
        const rows = document.querySelectorAll('.d-bot-result .d-message-row__body');
        const text = rows[rows.length - 1]?.textContent ?? '';
        if (text !== '' && !text.includes(g2.pending)) g2.first = performance.now();
      }).observe(document.body, { subtree: true, childList: true, characterData: true });
    });
    const samples: number[] = [];
    for (let run = 0; run < G2_RUNS; run++) {
      const region = await openList(page);
      const picker = region.getByRole('textbox', { name: 'Command', exact: true });
      await picker.fill('/remind');
      await picker.press('Tab');
      await region.getByRole('combobox', { name: 'when', exact: true }).fill('in 10 minutes');
      await region.getByRole('combobox', { name: 'when', exact: true }).press('Tab');
      await region.getByRole('textbox', { name: 'text', exact: true }).fill(`G2 probe ${String(run)}`);
      await page.evaluate(() => { const root = window as unknown as { __g2: { start: number; first: number } }; root.__g2.start = 0; root.__g2.first = 0; });
      await region.getByRole('textbox', { name: 'text', exact: true }).press('Enter');
      await expect.poll(() => page.evaluate(() => (window as unknown as { __g2: { first: number } }).__g2.first), { timeout: BOT_WAIT }).toBeGreaterThan(0);
      samples.push(await page.evaluate(() => { const g2 = (window as unknown as { __g2: { start: number; first: number } }).__g2; return g2.first - g2.start; }));
      await expect(commandRegion(page)).toHaveCount(0);
    }
    samples.sort((a, b) => a - b);
    const p95 = samples[Math.ceil(samples.length * 0.95) - 1];
    test.info().annotations.push({ type: 'G2', description: `rtt ${String(rtt)} ms, p95 ${p95.toFixed(1)} ms, samples ${samples.map(s => s.toFixed(0)).join(' ')}` });
    console.log(`G2 rtt ${String(rtt)} ms, p95 ${p95.toFixed(1)} ms, samples ${samples.map(s => s.toFixed(0)).join(' ')}`);
    expect(p95, `rtt ${String(rtt)} ms, samples ${samples.map(s => s.toFixed(0)).join(' ')}`).toBeLessThanOrEqual(rtt + G2_HEADROOM_MS);
  } finally { await stopAppBeforeHost(page, host); }
});
