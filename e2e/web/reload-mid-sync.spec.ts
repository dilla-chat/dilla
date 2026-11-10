// A second device of an account reloads while it is still joining its groups, right after messages of the first device
// reached it (the CI trace of layout.spec on PR #59, webkit-web: Ada's "and again" stayed "could not be read on this
// device" on her phone). The phone holds the messages for their sender's signed list (E_DEVICE_LIST_STALE) while its own
// account's history is still on its way; the reload restarts the trust pass with every leaf of the account at cached
// version 0, and each of them must be ingested and its held rows released, not only the first one's.
import { expect, type Page, type Worker } from '@playwright/test';
import {
  TEST_TIMEOUT, WAIT, addServerVia, channelRow, composerBox, copy, expectComposerReady, expectShell, instanceInvite,
  messageRow, newChannelButton, railItem, signIn, signUp, tag,
} from './support/app';
import { test } from './support/second';
import { watchSocketRequests, type SocketRequestSeen } from './support/socket-requests';

test.describe.configure({ timeout: TEST_TIMEOUT * 2 });

const SERVER = 'Mid Sync';
const CHANNELS = ['general', 'raid', 'news'];
const UPLOAD = /^\/v1\/groups\/([0-9a-f]{32})\/message$/;
const HANDSHAKES = /^\/v1\/groups\/([0-9a-f]{32})\/handshakes$/;
/** How long the phone's reads of a signed device-list history take: the CI runner's worker took this long and more. */
const SLOW_HISTORY_MS = 8000;

async function makeChannel(page: Page, name: string): Promise<void> {
  await newChannelButton(page).click();
  const dialog = page.getByRole('dialog', { name: copy('create.channel.title', { server: SERVER }), exact: true });
  await dialog.getByRole('textbox', { name: copy('create.channel.name'), exact: true }).fill(name);
  await dialog.getByRole('textbox', { name: copy('create.channel.name'), exact: true }).press('Enter');
  await expect(dialog).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, name);
}

/** Holds every group join (the external commit, `POST /v1/groups/{id}/resync`) of the worker after the first two groups, for as
 *  long as the worker lives. The join is a gateway request frame, so the page's HTTP routes never see it: the frame is held in
 *  the worker's WebSocket.send, which it matches by the route text inside the frame. `joined` is the groups let through. */
function holdJoinsAfterTwo(): void {
  const proto = WebSocket.prototype;
  if ((proto as WebSocket & { dillaHolding?: true }).dillaHolding === true) return;
  (proto as WebSocket & { dillaHolding?: true }).dillaHolding = true;
  const joined: string[] = [];
  (globalThis as unknown as { dillaJoined: string[] }).dillaJoined = joined;
  const send = proto.send;
  proto.send = function (this: WebSocket, data: string | ArrayBufferLike | Blob | ArrayBufferView): void {
    if (typeof data !== 'string' && !(data instanceof Blob)) {
      const bytes = ArrayBuffer.isView(data) ? new Uint8Array(data.buffer, data.byteOffset, data.byteLength) : new Uint8Array(data);
      const head = String.fromCharCode(...bytes.subarray(0, 200));
      const m = /\/v1\/groups\/([0-9a-f]{32})\/resync/.exec(head);
      if (m !== null) {
        const group = m[1]!;
        if (joined.length < 2 && !joined.includes(group)) joined.push(group);
        if (!joined.includes(group)) return;
      }
    }
    send.call(this, data);
  };
}

/** Sends in a channel and answers the group the upload went to. `seen` is the page's socket request log
 *  (watchSocketRequests): the upload is a request frame on the gateway socket, not an HTTP request. */
async function sendIn(page: Page, channel: string, text: string, seen: SocketRequestSeen[]): Promise<string> {
  await channelRow(page, channel).click();
  const box = composerBox(page, channel);
  await box.fill(text, { timeout: WAIT });
  // A trust refresh may disable the native button between actionability and mouse-up.
  // Observe native submit before the click: only an unsubmitted, untouched draft can be
  // clicked again. Once any submit happened, its real upload must succeed without retry.
  const before = seen.length;
  const uploads = (): SocketRequestSeen[] => seen.slice(before).filter((r) => r.method === 'POST' && UPLOAD.test(r.path));
  await page.evaluate(() => {
    const state = { submits: 0, dispose: () => {} };
    const observe = (event: Event): void => {
      if (event.target instanceof HTMLFormElement && event.target.matches('.d-composer')) state.submits++;
    };
    document.addEventListener('submit', observe, true);
    state.dispose = () => document.removeEventListener('submit', observe, true);
    (window as unknown as { __midSyncSubmit: typeof state }).__midSyncSubmit = state;
  });
  const deadline = Date.now() + WAIT;
  const remaining = (): number => Math.max(1, deadline - Date.now());
  const attempts: { submits: number; uploads: number }[] = [];
  let upload: SocketRequestSeen;
  try {
    const send = page.locator('.d-app-shell__main .d-composer').getByRole('button', { name: copy('shell.composer.send'), exact: true });
    for (;;) {
      await expect(box).toBeEditable({ timeout: remaining() });
      await expect(send).toBeEnabled({ timeout: remaining() });
      await send.click({ timeout: remaining() });
      const submits = await page.evaluate(() => (window as unknown as { __midSyncSubmit: { submits: number } }).__midSyncSubmit.submits);
      attempts.push({ submits, uploads: uploads().length });
      if (submits > 0 || uploads().length > 0) break;
      await expect(box).toHaveValue(text, { timeout: remaining() });
      if (Date.now() >= deadline || attempts.length >= 3) throw new Error('Send did not dispatch a native submit within the bounded attempts');
    }
    await expect.poll(() => uploads().length, { timeout: remaining(), message: 'the submitted message produces its real upload' }).toBeGreaterThan(0);
    upload = uploads()[0]!;
  } finally {
    await page.evaluate(() => (window as unknown as { __midSyncSubmit: { dispose(): void } }).__midSyncSubmit.dispose());
    await test.info().attach(`mid-sync-send-${channel}-${text.split(' ').at(-1)}.json`, {
      body: Buffer.from(JSON.stringify({ attempts, uploads: uploads().length })), contentType: 'application/json',
    });
  }
  await expect(messageRow(page, channel, text)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  return UPLOAD.exec(upload.path)![1]!;
}

test('messages that reach a second device mid-sync are readable after it reloads, in every group it joined', async ({ page, second, instanceName }) => {
  const adaSeen = watchSocketRequests(page);
  const ada = await signUp(page, instanceInvite(), instanceName);
  await addServerVia(page, 'create');
  const start = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
  await start.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  await expect(start).toBeHidden({ timeout: WAIT });
  await expectComposerReady(page, 'general');
  for (const name of CHANNELS.slice(1)) await makeChannel(page, name);
  // One message per channel before the phone, which also names each channel's group.
  const channelOf = new Map<string, string>();
  for (const name of CHANNELS) channelOf.set(await sendIn(page, name, `before the phone ${tag()}`, adaSeen), name);
  const serverPath = new URL(page.url()).pathname;

  // The phone joins two groups, and the rest of its joins wait: it is mid-sync when the messages reach it.
  const phone = await second.open();
  // The joins are request frames: they are held in the phone's worker, which lets the first two groups' through.
  // Every worker the page starts, now and after a navigation, holds them: the patch is on the prototype, so it applies to
  // a socket the worker opens later.
  const hold = (worker: Worker): void => { void worker.evaluate(holdJoinsAfterTwo).catch(() => undefined); };
  phone.page.on('worker', hold);
  for (const worker of phone.page.workers()) hold(worker);
  const joinedNow = async (): Promise<string[]> => {
    const lists = await Promise.all(phone.page.workers().map((w) => w.evaluate(() => (globalThis as unknown as { dillaJoined?: string[] }).dillaJoined ?? []).catch(() => [] as string[])));
    return lists.flat();
  };
  // Its own account's signed history is slow to arrive (the CI runner): the messages are held for their sender's list.
  await phone.context.route(/\/device-list\?after=/, async (route) => {
    await new Promise((r) => setTimeout(r, SLOW_HISTORY_MS));
    await route.fallback().catch(() => undefined);
  });
  // A joined group is caught up once its handshakes were read (the catch-up after the join).
  const caughtUp = new Set<string>();
  watchSocketRequests(phone.page, (r) => { const m = r.method === 'GET' ? HANDSHAKES.exec(r.path) : null; if (m !== null) caughtUp.add(m[1]!); });
  await signIn(phone.page, ada, instanceName);
  let joined: string[] = [];
  await expect.poll(async () => { joined = await joinedNow(); return joined.length === 2 && joined.every((g) => caughtUp.has(g)); }, { timeout: WAIT }).toBe(true);

  // Ada writes in both of those channels; the phone reloads while the messages reach it.
  const sent: [string, string][] = [];
  for (const group of joined) {
    const channel = channelOf.get(group)!;
    const text = `mid sync in ${channel} ${tag()}`;
    await sendIn(page, channel, text, adaSeen);
    sent.push([channel, text]);
  }
  await phone.page.waitForTimeout(120);
  await phone.page.goto(serverPath);
  await expectShell(phone.page);
  await railItem(phone.page, SERVER).click();
  for (const [channel, text] of sent) {
    await channelRow(phone.page, channel).click();
    await expect(messageRow(phone.page, channel, text)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  }
});
