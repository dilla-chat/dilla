import { randomBytes } from 'node:crypto';
import { expect } from '@playwright/test';
import { arr, decode, encode } from '../../packages/client-core/src/cbor/index';
import { test as appTest, addServerVia, completeSignUp, copy, makeInviteLink, newAccount, signUp, type Account } from './support/app';
import { prepareBotRuntimeBinaries, startTestHost, type HostConfig, type StartedHost } from './support/host';

const HOST: HostConfig = { port: 8521, control: 8522, webRoot: 'packages/web/dist', botRuntime: true, advanceClockWithWall: true };
const SERVER = 'Announcement Scheduler Acceptance';
const MESSAGE = `scheduled browser notice ${randomBytes(5).toString('hex')}`;

appTest.use({ baseURL: `http://127.0.0.1:${String(HOST.port)}` });

appTest.beforeAll(async () => {
  appTest.setTimeout(360_000);
  await prepareBotRuntimeBinaries();
});

/** Capture actual core-worker replies from the shipping UI and provide a test call over that same authenticated worker. */
function installCoreWorkerBridge(): void {
  type Reply = { t?: string; id?: number; ok?: boolean; value?: unknown; error?: { code?: string; detail?: string } };
  type Call = { t: 'call'; id: number; command: { m: string; [key: string]: unknown } };
  type Pending = { command: Call['command']; resolve?: (value: unknown) => void; reject?: (error: Error) => void };
  const state = window as unknown as { __dillaAnnouncementBridge?: {
    replies: { command: Call['command']; reply: Reply }[];
    invoke(command: Call['command']): Promise<unknown>;
  } };
  const NativeWorker = window.Worker;
  const workers: Worker[] = [];
  const pending = new Map<number, Pending>();
  let nextId = 1_000_000;
  class ObservedWorker extends NativeWorker {
    constructor(url: string | URL, options?: WorkerOptions) {
      super(url, options);
      workers.push(this);
      this.addEventListener('message', event => {
        const reply = event.data as Reply;
        if (reply?.t !== 'ret' || typeof reply.id !== 'number') return;
        const active = pending.get(reply.id);
        if (active === undefined) return;
        pending.delete(reply.id);
        state.__dillaAnnouncementBridge?.replies.push({ command: active.command, reply });
        if (active.resolve === undefined) return;
        if (reply.ok === true) active.resolve(reply.value);
        else active.reject?.(new Error(`${reply.error?.code ?? 'worker-error'} ${reply.error?.detail ?? ''}`.trim()));
      });
    }
    postMessage(message: unknown, options?: Transferable[] | StructuredSerializeOptions): void {
      const call = message as Call;
      if (call?.t === 'call' && typeof call.id === 'number' && typeof call.command?.m === 'string' && !pending.has(call.id)) {
        pending.set(call.id, { command: call.command });
      }
      super.postMessage(message, options as Transferable[]);
    }
  }
  Object.defineProperty(window, 'Worker', { value: ObservedWorker, configurable: true, writable: true });
  state.__dillaAnnouncementBridge = {
    replies: [],
    invoke(command) {
      const worker = workers.at(-1);
      if (worker === undefined) return Promise.reject(new Error('shipping core worker is not ready'));
      const id = nextId++;
      return new Promise((resolve, reject) => {
        pending.set(id, { command, resolve, reject });
        worker.postMessage({ t: 'call', id, command });
      });
    },
  };
}

type InteractionPage = { responses: { requestId: string; operation: number; status: number; revision: string | null; data: number[] }[]; through: string | null };

async function coreCall<T>(page: import('@playwright/test').Page, command: Record<string, unknown>): Promise<T> {
  return page.evaluate(async (input) => {
    const bridge = (window as unknown as { __dillaAnnouncementBridge?: { invoke(command: never): Promise<unknown> } }).__dillaAnnouncementBridge;
    if (bridge === undefined) throw new Error('shipping core worker bridge was not installed');
    return bridge.invoke(input as never);
  }, command) as Promise<T>;
}

function requestId(): string { return randomBytes(16).toString('hex'); }

async function botInteraction(page: import('@playwright/test').Page, communityId: string, operation: number, args: unknown[], revision: string | null): Promise<string> {
  const id = requestId();
  const result = await coreCall<{ requestId: string; upload: string }>(page, {
    m: 'humanInteractionRequest', communityId, channelId: null, requestId: id, operation, args, revision,
  });
  expect(result).toEqual({ requestId: id, upload: 'accepted' });
  return id;
}

async function interactionResponse(page: import('@playwright/test').Page, communityId: string, operation: number, requestId: string): Promise<{ status: number; revision: string | null; data: Uint8Array }> {
  let found: { status: number; revision: string | null; data: Uint8Array } | undefined;
  await expect.poll(async () => {
    const result = await coreCall<InteractionPage & { blocked: boolean }>(page, { m: 'readBotInteractionResponses', communityId, operations: [operation] });
    const row = result.responses.find(response => response.requestId === requestId);
    if (row !== undefined) found = { status: row.status, revision: row.revision, data: new Uint8Array(row.data) };
    if (result.through !== null) await coreCall(page, { m: 'ackBotInteractionResponses', communityId, through: result.through });
    return found !== undefined;
  }, { timeout: 12_000, intervals: [250, 500, 1_000] }).toBe(true);
  if (found === undefined) throw new Error(`Bot interaction ${String(operation)} had no response`);
  return found;
}

function stockholmMinuteAfterWithTime(seconds: number): { date: string; hour: number; minute: number; at: number } {
  const parts = new Intl.DateTimeFormat('en-GB', { timeZone: 'Europe/Stockholm', year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', hourCycle: 'h23', timeZoneName: 'longOffset' }).formatToParts(new Date(Date.now() + seconds * 1000));
  const value = Object.fromEntries(parts.map(part => [part.type, part.value]));
  const offset = /^GMT([+-])(\d{2}):(\d{2})$/u.exec(value.timeZoneName ?? '');
  if (offset === null) throw new Error(`cannot read Stockholm offset ${value.timeZoneName}`);
  const minutes = (Number(offset[2]) * 60 + Number(offset[3])) * (offset[1] === '-' ? -1 : 1);
  const hour = Number(value.hour);
  const minute = Number(value.minute);
  const at = Date.UTC(Number(value.year), Number(value.month) - 1, Number(value.day), hour, minute) - minutes * 60_000;
  return { date: `${value.year}-${value.month}-${value.day}`, hour, minute, at };
}

type DebugGroup = { id: string; epoch: number; seq: number; epoch_unknown: boolean; messages: number };
type BotSendAttempt = { method: string; route: string; group: string; status: number; error_code: string; epoch: number; epoch_unknown: boolean };
type BotRuntimeError = { method: string; path: string; status: number; error_code: string; error_detail?: string; group_epoch: number; group_epoch_unknown: boolean };
type GroupHealAttempt = { method: string; route: string; group: string; status: number; error_code: string; epoch: number; epoch_unknown: boolean };
type BotInteractionHealAttempt = { method: string; route: string; group: string; status: number; error_code: string; epoch: number; epoch_unknown: boolean };

async function targetGroups(host: StartedHost, target: string): Promise<DebugGroup[]> {
  const response = await fetch(`${host.control}/debug/group-state`, {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ target }),
  });
  if (!response.ok) throw new Error(`group state = HTTP ${response.status}: ${await response.text()}`);
  return (await response.json() as { groups: DebugGroup[] }).groups;
}

async function botSendAttempts(host: StartedHost): Promise<BotSendAttempt[]> {
  const response = await fetch(`${host.control}/debug/bot-send-stats`);
  if (!response.ok) throw new Error(`Bot send stats = HTTP ${response.status}: ${await response.text()}`);
  return (await response.json() as { attempts: BotSendAttempt[] }).attempts;
}

async function botRuntimeErrors(host: StartedHost): Promise<BotRuntimeError[]> {
  const response = await fetch(`${host.control}/debug/bot-runtime-errors`);
  if (!response.ok) throw new Error(`Bot runtime errors = HTTP ${response.status}: ${await response.text()}`);
  return (await response.json() as { errors: BotRuntimeError[] }).errors;
}

async function groupHealAttempts(host: StartedHost): Promise<GroupHealAttempt[]> {
  const response = await fetch(`${host.control}/debug/group-heal-attempts`);
  if (!response.ok) throw new Error(`group heal attempts = HTTP ${response.status}: ${await response.text()}`);
  return (await response.json() as { attempts: GroupHealAttempt[] }).attempts;
}

async function botInteractionHealAttempts(host: StartedHost): Promise<BotInteractionHealAttempt[]> {
  const response = await fetch(`${host.control}/debug/bot-interaction-heal-attempts`);
  if (!response.ok) throw new Error(`Bot interaction heal attempts = HTTP ${response.status}: ${await response.text()}`);
  return (await response.json() as { attempts: BotInteractionHealAttempt[] }).attempts;
}

appTest('creates and sends a one-off announcement through the browser client, then survives a Bot restart without a duplicate', async ({ page, instanceName }) => {
  appTest.setTimeout(360_000);
  await page.addInitScript(installCoreWorkerBridge);
  let host: StartedHost | null = null;
  try {
    host = await startTestHost(HOST);
    const account = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, 'Ada', host.control);
    await addServerVia(page, 'create');
    const create = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
    await create.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
    await page.keyboard.press('Enter');
    const general = page.getByRole('button', { name: copy('voice.open', { name: 'general' }), exact: true });
    await expect(general).toBeVisible({ timeout: 30_000 });
    const created = await page.evaluate(() => {
      const bridge = (window as unknown as { __dillaAnnouncementBridge?: { replies: { command: { m: string; kind?: string }; reply: { ok?: boolean; value?: unknown } }[] } }).__dillaAnnouncementBridge;
      return bridge?.replies.filter(row => row.command.m === 'createCommunity' && row.reply.ok === true).at(-1)?.reply.value ?? null;
    }) as { communityId?: string } | null;
    if (created?.communityId === undefined) throw new Error('shipping UI did not return the created community ID');
    const communityId = created.communityId;
    const channel = await page.evaluate(() => {
      const bridge = (window as unknown as { __dillaAnnouncementBridge?: { replies: { command: { m: string; kind?: string }; reply: { ok?: boolean; value?: unknown } }[] } }).__dillaAnnouncementBridge;
      return bridge?.replies.filter(row => row.command.m === 'createChannel' && row.command.kind === 'text' && row.reply.ok === true).at(-1)?.reply.value ?? null;
    }) as { channelId?: string } | null;
    if (channel?.channelId === undefined) throw new Error('shipping UI did not return its starter text channel ID');
    const textChannel = page.getByRole('button', { name: 'general', exact: true });
    await expect(textChannel).toBeVisible();
    await textChannel.click();

    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    const settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
    await settings.getByRole('switch', { name: copy('serverSettings.bot.cap.announcements'), exact: true }).click();
    await settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true }).click();
    const stepUp = page.getByRole('dialog', { name: copy('factorSettings.stepUp.title'), exact: true });
    await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(account.password);
    await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();
    await expect(settings.locator('.dw-bot-settings').getByRole('status').filter({ hasText: copy('admin.bot.status.ready') })).toHaveText(copy('admin.bot.status.ready'), { timeout: 90_000 });
    await page.keyboard.press('Escape');

    const welcomeSettingsId = await botInteraction(page, communityId, 1, [5], null);
    expect((await interactionResponse(page, communityId, 1, welcomeSettingsId)).status).toBe(0);

    // The schedule stores minute precision. Thirty seconds ahead can truncate back to the
    // current, already elapsed minute when this request happens early in that minute.
    const runAt = stockholmMinuteAfterWithTime(90);
    expect(runAt.at).toBeGreaterThan(Date.now() + 25_000);
    const schedule = encode([1n, new Uint8Array(16).fill(0x31), Uint8Array.from(channel.channelId.match(/../gu)!.map(byte => Number.parseInt(byte, 16))),
      MESSAGE, 'one_off', 'Europe/Stockholm', runAt.date, runAt.hour, runAt.minute, 0, 1, 1]);
    const putId = await botInteraction(page, communityId, 51, [null, schedule], '0');
    const put = await interactionResponse(page, communityId, 51, putId);
    expect(put.status).toBe(0);
    expect(put.revision).toBe('1');
    expect(decode(put.data)).toEqual([]);

    const listId = await botInteraction(page, communityId, 50, [0], null);
    const listed = await interactionResponse(page, communityId, 50, listId);
    expect(listed.status).toBe(0);
    const [version, offset, total, rows] = arr(decode(listed.data), 4);
    expect([version, offset, total]).toEqual([1n, 0n, 1n]);
    const listedSchedule = arr(arr(rows)[0] ?? null);
    expect(listedSchedule.slice(0, 5)).toEqual([
      new Uint8Array(16).fill(0x31),
      Uint8Array.from(channel.channelId.match(/../gu)!.map(byte => Number.parseInt(byte, 16))),
      MESSAGE, 'one_off', 'Europe/Stockholm',
    ]);

    await expect(page.getByText(MESSAGE, { exact: true })).toHaveCount(1, { timeout: 120_000 });
    await host.restartBotRuntime();
    await expect(page.getByText(MESSAGE, { exact: true })).toHaveCount(1, { timeout: 60_000 });
    const afterRestartId = await botInteraction(page, communityId, 50, [0], null);
    const afterRestart = await interactionResponse(page, communityId, 50, afterRestartId);
    const [, , afterTotal, afterRows] = arr(decode(afterRestart.data), 4);
    expect(afterTotal).toBe(1n);
    expect(arr(afterRows)).toHaveLength(1);
    await expect(page.getByText(MESSAGE, { exact: true })).toHaveCount(1);
  } finally {
    await host?.stop();
  }
});

appTest('paired restore refuses a stale managed Bot announcement until a member heals the channel group', async ({ page, instanceName, browser, guard }) => {
  appTest.setTimeout(900_000);
  await page.addInitScript(installCoreWorkerBridge);
  let host: StartedHost | null = null;
  let secondContext: import('@playwright/test').BrowserContext | null = null;
  let intentionalOfflineConsoleStart: number | null = null;
  try {
    host = await startTestHost(HOST);
    const account = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, 'Ada', host.control);
    await addServerVia(page, 'create');
    const create = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
    await create.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
    await page.keyboard.press('Enter');
    await expect(page.getByRole('button', { name: copy('voice.open', { name: 'general' }), exact: true })).toBeVisible({ timeout: 30_000 });
    const created = await page.evaluate(() => {
      const bridge = (window as unknown as { __dillaAnnouncementBridge?: { replies: { command: { m: string; kind?: string }; reply: { ok?: boolean; value?: unknown } }[] } }).__dillaAnnouncementBridge;
      return bridge?.replies.filter(row => row.command.m === 'createCommunity' && row.reply.ok === true).at(-1)?.reply.value ?? null;
    }) as { communityId?: string } | null;
    const channel = await page.evaluate(() => {
      const bridge = (window as unknown as { __dillaAnnouncementBridge?: { replies: { command: { m: string; kind?: string }; reply: { ok?: boolean; value?: unknown } }[] } }).__dillaAnnouncementBridge;
      return bridge?.replies.filter(row => row.command.m === 'createChannel' && row.command.kind === 'text' && row.reply.ok === true).at(-1)?.reply.value ?? null;
    }) as { channelId?: string } | null;
    if (created?.communityId === undefined || channel?.channelId === undefined) throw new Error('shipping client did not return community and channel IDs');
    const communityId = created.communityId;
    const channelId = channel.channelId;
    await page.getByRole('button', { name: 'general', exact: true }).click();

    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    const settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    await settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
      .getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
    await settings.getByRole('switch', { name: copy('serverSettings.bot.cap.announcements'), exact: true }).click();
    await settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true }).click();
    const stepUp = page.getByRole('dialog', { name: copy('factorSettings.stepUp.title'), exact: true });
    await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(account.password);
    await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();
    await expect(settings.locator('.dw-bot-settings').getByRole('status').filter({ hasText: copy('admin.bot.status.ready') })).toHaveText(copy('admin.bot.status.ready'), { timeout: 90_000 });
    await page.keyboard.press('Escape');

    const welcomeSettingsId = await botInteraction(page, communityId, 1, [5], null);
    expect((await interactionResponse(page, communityId, 1, welcomeSettingsId)).status).toBe(0);
    const runAt = stockholmMinuteAfterWithTime(300);
    const schedule = encode([1n, new Uint8Array(16).fill(0x42), Uint8Array.from(channelId.match(/../gu)!.map(byte => Number.parseInt(byte, 16))),
      MESSAGE, 'one_off', 'Europe/Stockholm', runAt.date, runAt.hour, runAt.minute, 0, 1, 1]);
    const putId = await botInteraction(page, communityId, 51, [null, schedule], '0');
    expect((await interactionResponse(page, communityId, 51, putId)).status).toBe(0);
    const inviteLink = await makeInviteLink(page, SERVER);

    const before = await targetGroups(host, channelId);
    expect(before, 'the shipping text channel has one active MLS group').toHaveLength(1);
    const original = before[0];
    if (original === undefined) throw new Error('text channel has no active group');
    // Firefox may keep an established gateway WebSocket alive through setOffline(true).
    // Leave the app to close it before the second member advances the group.
    const channelURL = page.url();
    await page.goto('about:blank');
    intentionalOfflineConsoleStart = guard.consoleErrors.length;
    await page.context().setOffline(true);
    await host.snapshotPair('paired-restore');

    secondContext = await browser.newContext({ baseURL: host.url });
    await guard.attach(secondContext);
    const secondPage = await secondContext.newPage();
    await secondPage.goto(new URL(inviteLink).pathname);
    await secondPage.getByRole('link', { name: 'Open dilla in this browser' }).click();
    await completeSignUp(secondPage, newAccount('Bea', host.control), instanceName);
    await expect(secondPage.getByRole('navigation', { name: copy('shell.rail.label'), exact: true })
      .getByRole('button', { name: SERVER, exact: true })).toBeVisible();
    await expect.poll(async () => (await targetGroups(host!, channelId)).find(group => group.id === original.id)?.epoch ?? original.epoch,
      { timeout: 60_000, intervals: [500, 1_000] }).toBeGreaterThan(original.epoch);
    const advanced = (await targetGroups(host, channelId)).find(group => group.id === original.id);
    if (advanced === undefined || advanced.epoch <= original.epoch) throw new Error('the joined member did not advance the channel group epoch');

    await secondContext.setOffline(true);
    await host.restorePair('paired-restore');
    const restored = (await targetGroups(host, channelId)).find(group => group.id === original.id);
    expect(restored, 'restored archive retains the same channel group').toBeDefined();
    expect(restored?.epoch).toBe(original.epoch);
    expect(restored?.epoch_unknown).toBe(true);
    expect(restored?.messages).toBe(original.messages);

    let interactionHeal: BotInteractionHealAttempt | undefined;
    await expect.poll(async () => {
      interactionHeal = (await botInteractionHealAttempts(host!)).find(attempt => attempt.method === 'POST' && attempt.status >= 200 && attempt.status < 300);
      return interactionHeal;
    }, { timeout: 60_000, intervals: [250, 500, 1_000] }).toBeDefined();
    expect(interactionHeal?.route).toBe('/v1/bot-runtime/installations/{installation}/interaction-groups/{group}/heal');
    expect(interactionHeal?.group).not.toBe(original.id);
    expect(interactionHeal?.epoch_unknown).toBe(false);
    expect((await targetGroups(host, channelId)).find(group => group.id === original.id)?.epoch_unknown,
      'healing the private Bot interaction group leaves the channel group stale').toBe(true);

    await expect.poll(() => Date.now(), { timeout: 360_000, intervals: [250, 500] }).toBeGreaterThanOrEqual(runAt.at);
    let refused: BotSendAttempt | undefined;
    await expect.poll(async () => {
      refused = (await botSendAttempts(host!)).find(attempt => attempt.group === original.id && attempt.status === 403 && attempt.epoch_unknown);
      return refused;
    }, { timeout: 60_000, intervals: [250, 500, 1_000] }).toBeDefined().catch(async error => {
      throw new Error(`no managed Bot send attempt; runtime HTTP failures: ${JSON.stringify(await botRuntimeErrors(host!))}; ${String(error)}`);
    });
    expect(refused?.error_code, 'the server rejected the Bot POST because its exact target group awaited member heal').toBe('E_LEAF_NOT_CURRENT');
    expect(refused?.epoch).toBe(original.epoch);
    expect(refused?.method).toBe('POST');
    expect(refused?.route).toBe('/v1/groups/{group}/bot/messages');
    const stillUnknown = (await targetGroups(host, channelId)).find(group => group.id === original.id);
    expect(stillUnknown?.epoch_unknown, 'the Bot send attempt did not heal the group').toBe(true);
    expect(stillUnknown?.messages, 'a Bot send was not stored before a member heal').toBe(original.messages);

    const previousAttempts = (await botSendAttempts(host)).length;
    await secondContext.close();
    secondContext = null;
    await page.context().setOffline(false);
    await page.goto(channelURL);
    await expect.poll(async () => {
      const group = (await targetGroups(host!, channelId)).find(row => row.id === original.id);
      return group === undefined ? false : !group.epoch_unknown;
    }, { timeout: 90_000, intervals: [500, 1_000] }).toBe(true);
    const healed = (await targetGroups(host, channelId)).find(group => group.id === original.id);
    expect(healed?.epoch_unknown).toBe(false);
    const memberHeal = (await groupHealAttempts(host)).find(attempt => attempt.group === original.id && attempt.method === 'POST' && attempt.status >= 200 && attempt.status < 300);
    expect(memberHeal, 'a human member healed the exact channel group through the authenticated heal API').toBeDefined();
    expect(memberHeal?.epoch).toBe(original.epoch);
    await expect(page.getByText(MESSAGE, { exact: true })).toHaveCount(1, { timeout: 120_000 });
    let accepted: BotSendAttempt | undefined;
    await expect.poll(async () => {
      accepted = (await botSendAttempts(host!)).slice(previousAttempts).find(attempt => attempt.group === original.id && attempt.status >= 200 && attempt.status < 300);
      return accepted;
    }, { timeout: 30_000, intervals: [250, 500] }).toBeDefined();
    expect(accepted?.error_code).toBe('');
    expect(accepted?.epoch_unknown).toBe(false);
    expect(accepted?.epoch).toBe(original.epoch);
    expect(accepted?.group).toBe(refused?.group);
    expect(accepted?.method).toBe('POST');
    expect(accepted?.route).toBe(refused?.route);
    const afterPost = (await targetGroups(host, channelId)).find(group => group.id === original.id);
    expect(afterPost?.messages).toBe(original.messages + 1);
    const intentionalOfflineConsole = guard.consoleErrors.slice(intentionalOfflineConsoleStart);
    expect(intentionalOfflineConsole.every(line => line.includes('ERR_INTERNET_DISCONNECTED'))).toBe(true);
    guard.consoleErrors.splice(intentionalOfflineConsoleStart, intentionalOfflineConsole.length);
  } finally {
    await page.context().setOffline(false).catch(() => undefined);
    await secondContext?.setOffline(false).catch(() => undefined);
    if (intentionalOfflineConsoleStart !== null) {
      for (let index = guard.consoleErrors.length - 1; index >= intentionalOfflineConsoleStart; index--) {
        if (guard.consoleErrors[index]?.includes('ERR_INTERNET_DISCONNECTED')) guard.consoleErrors.splice(index, 1);
      }
    }
    await secondContext?.close();
    await host?.stop();
  }
});
