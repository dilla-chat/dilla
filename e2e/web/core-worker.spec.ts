import type { Page } from '@playwright/test';
import type { HarnessApi, HarnessFile, HarnessOpen, HarnessResult } from '../../packages/client-core/harness/main';
import type {
  AccountState, BadgeState, ChannelSummary, Command, CommunitySummary, ConnectionState, DeviceSummary, MemberSummary, NoticesState,
  PinnedItem, TimelineItem, TimelineState, TrayItem,
} from '../../packages/client-core/src/index';
import { WebDriver, instanceInvite, testHostUrl, testkitEnv, type PeerReceived } from './support/driver';
import { expect, test } from './support/second-harness';
import { completeFactor, nextCode } from './support/totp';

const CONTROL = process.env.DILLA_TESTKIT_CONTROL ?? 'http://127.0.0.1:8454';

const CI = process.env.CI === 'true';
const WAIT = CI ? 90_000 : 30_000;
const HEX32 = /^[0-9a-f]{32}$/;
const CROCKFORD_GROUP = /^[0-9A-HJKMNP-TV-Z]{4}$/;

type HarnessWindow = Window & { dilla: HarnessApi };

async function call(page: Page, command: Command): Promise<HarnessResult> {
  return page.evaluate((c) => (window as unknown as HarnessWindow).dilla.call(c), command);
}

async function ok(page: Page, command: Command): Promise<unknown> {
  const result = await call(page, command);
  expect(result, `${command.m}: ${JSON.stringify(result)}`).toMatchObject({ ok: true });
  return result.ok ? result.value : undefined;
}

function slice<T>(page: Page, name: string): Promise<T | null> {
  return page.evaluate((n) => (window as unknown as HarnessWindow).dilla.get(n), name) as Promise<T | null>;
}

async function phaseOf(page: Page): Promise<string | null> {
  return (await slice<AccountState>(page, 'account'))?.phase ?? null;
}

async function openHarness(page: Page): Promise<void> {
  await page.goto('/');
  await expect(page.locator('#status')).toHaveText('ready', { timeout: WAIT });
  await ok(page, { m: 'start' });
}

test('the core worker signs up, joins, reads and sends, survives a gateway restart and a reload, and keeps a second tab out', async ({ page, context }) => {
  test.setTimeout(CI ? 900_000 : 300_000);
  const pageErrors: string[] = [];
  page.on('pageerror', (e) => pageErrors.push(e.message));
  const peer = await WebDriver.start(testHostUrl(), testkitEnv());
  const peerGot: PeerReceived[] = [];
  const peerReceives = async (body: string): Promise<PeerReceived> => {
    await expect.poll(async () => {
      peerGot.push(...(await peer.sync()).received);
      return peerGot.some((r) => r.body === body);
    }, { timeout: WAIT, message: `the peer receives ${JSON.stringify(body)}` }).toBe(true);
    return peerGot.find((r) => r.body === body)!;
  };
  try {
    const s = await peer.setup({ community: 'harness community', channel: 'general' });
    expect((await peer.register()).epoch).toBe(0);
    const username = `web${(peer.seed & 0xff_ffff).toString(16).padStart(6, '0')}`;
    const timeline = async (): Promise<TimelineItem[]> =>
      (await slice<TimelineState>(page, `timeline:${s.channel_id}`))?.items ?? [];

    await test.step('sign up through the bridge with an instance invite', async () => {
      await openHarness(page);
      await expect.poll(() => phaseOf(page), { timeout: WAIT }).toBe('needs-signup');
      const fresh = (await slice<AccountState>(page, 'account'))!;
      expect(fresh.instance).toMatchObject({ name: 'dilla.test', registrationMode: 0 });
      expect(fresh.instance!.id).toMatch(HEX32);
      expect(fresh.recoveryKey).toBeNull();
      await ok(page, { m: 'signupBegin' });
      const keys = (await slice<AccountState>(page, 'account'))!;
      expect(keys.phase).toBe('signup-keys');
      expect(keys.recoveryKey).toHaveLength(13);
      for (const group of keys.recoveryKey!) expect(group).toMatch(CROCKFORD_GROUP);
      // An instance invite names no community, so the signup joins nothing (L-TS-09 signupSubmit result).
      // web3.6: the signup stops at factor-setup until an authenticator app is confirmed.
      expect(await ok(page, {
        m: 'signupSubmit', invite: instanceInvite(), username, display: 'Web Harness',
        password: keys.instance!.passwordSignup ? 'harness password 1' : null, recoveryKeyAcknowledged: true,
      })).toEqual({ communityId: null, joinError: null, factorOwed: true });
      expect((await completeFactor(page, CONTROL)).result).toEqual({ communityId: null, joinError: null });
      const ready = (await slice<AccountState>(page, 'account'))!;
      expect(ready.phase).toBe('ready');
      expect(ready.user?.username).toBe(username);
      expect(ready.user?.id).toMatch(HEX32);
      expect(ready.deviceId).toMatch(HEX32);
      expect(ready.recoveryKey).toBeNull();
      expect(ready.error).toBeNull();
      await expect.poll(async () => (await slice<ConnectionState>(page, 'connection'))?.status, { timeout: WAIT }).toBe('online');
      expect(await slice<CommunitySummary[]>(page, 'communities')).toEqual([]);
    });

    const me = (await slice<AccountState>(page, 'account'))!;
    const userId = me.user!.id;
    const deviceId = me.deviceId!;

    await test.step("join the peer's community with its invite and see its channel and members", async () => {
      expect(await ok(page, { m: 'joinCommunity', invite: s.invite_code })).toEqual({ communityId: s.community_id });
      expect(await slice<CommunitySummary[]>(page, 'communities')).toEqual([{ id: s.community_id, name: 'harness community', owner: s.user_id }]);
      await ok(page, { m: 'selectCommunity', communityId: s.community_id });
      expect(await slice<ChannelSummary[]>(page, `channels:${s.community_id}`)).toEqual([{
        id: s.channel_id, communityId: s.community_id, kind: 0, mode: 0, name: 'general', topic: '',
        parentId: null, position: 0, group: expect.stringMatching(/^(none|joining|active)$/) as unknown as string,
        slowmode: 0, announcement: false,
      }, {
        // web3.9: every server holds the built-in AFK voice channel at position 2^30; a voice channel is not joinable by the harness core ('unsupported').
        id: expect.stringMatching(HEX32) as unknown as string, communityId: s.community_id, kind: 1, mode: 0, name: 'AFK', topic: '',
        parentId: null, position: 2 ** 30, group: 'unsupported', slowmode: 0, announcement: false,
      }]);
      const members = (await slice<MemberSummary[]>(page, `members:${s.community_id}`))!;
      expect(members).toHaveLength(2);
      expect(members).toEqual(expect.arrayContaining([
        { userId: s.user_id, username: s.username, display: s.display, kind: 0, roleIds: [] },
        { userId, username, display: 'Web Harness', kind: 0, roleIds: [] },
      ]));
    });

    await test.step('open the channel by external join; the peer sees this device', async () => {
      await ok(page, { m: 'openChannel', channelId: s.channel_id });
      await expect.poll(async () => (await slice<TimelineState>(page, `timeline:${s.channel_id}`))?.group, { timeout: WAIT }).toBe('active');
      expect((await slice<ChannelSummary[]>(page, `channels:${s.community_id}`))?.[0]?.group).toBe('active');
      await expect.poll(async () => (await peer.members()).devices, { timeout: WAIT })
        .toEqual(expect.arrayContaining([s.device_id, deviceId]));
    });

    await test.step("receive the peer's message with the peer's authenticated sender", async () => {
      await peer.send('hello from the peer');
      await expect.poll(async () => (await timeline()).find((i) => i.body === 'hello from the peer') ?? null, { timeout: WAIT })
        .toMatchObject({ state: 'ok', reason: '', senderUser: s.user_id, senderDevice: s.device_id, own: false, web: false, bot: false });
    });

    await test.step('send; the peer decrypts it from this device at the browser tier', async () => {
      const sent = (await ok(page, { m: 'send', channelId: s.channel_id, text: 'hello from the browser' })) as { msgId: string };
      expect(sent.msgId).toMatch(HEX32);
      // One item for the message throughout, keyed o<msgId> while pending and after it is stored (L-TS-08 key rule).
      await expect.poll(async () => (await timeline()).filter((i) => i.msgId === sent.msgId).map((i) => [i.key, i.state]), { timeout: WAIT })
        .toEqual([[`o${sent.msgId}`, 'ok']]);
      expect((await timeline()).find((i) => i.msgId === sent.msgId))
        .toMatchObject({ own: true, web: true, senderUser: userId, senderDevice: deviceId, body: 'hello from the browser' });
      expect(await peerReceives('hello from the browser')).toMatchObject({ sender_user: userId, sender_device: deviceId, tier: 1 });
    });

    await test.step('a message sent while the gateway was down arrives after it reconnects', async () => {
      await page.evaluate(() => (window as unknown as HarnessWindow).dilla.hook('gateway-stop'));
      await expect.poll(async () => (await slice<ConnectionState>(page, 'connection'))?.status, { timeout: WAIT }).toBe('offline');
      await peer.send('sent while the browser was away');
      await page.evaluate(() => (window as unknown as HarnessWindow).dilla.hook('gateway-start'));
      await expect.poll(async () => (await slice<ConnectionState>(page, 'connection'))?.status, { timeout: WAIT }).toBe('online');
      await expect.poll(async () => (await timeline()).some((i) => i.body === 'sent while the browser was away' && i.state === 'ok'), {
        timeout: WAIT, message: 'the message sent while the gateway was stopped arrives after the restart (catch-up on ready)',
      }).toBe(true);
    });

    const before = (await timeline()).filter((i) => i.state === 'ok').map((i) => `${i.key} ${i.body}`);
    expect(before).toHaveLength(3);

    await test.step('reload: ready from the stored device key, same identity, same timeline, messages flow both ways', async () => {
      await page.reload();
      await expect(page.locator('#status')).toHaveText('ready', { timeout: WAIT });
      await ok(page, { m: 'start' });
      await expect.poll(() => phaseOf(page), { timeout: WAIT }).toBe('ready');
      const again = (await slice<AccountState>(page, 'account'))!;
      // The display name lands from the cache or the presence push, whichever comes first.
      expect(again.user).toMatchObject({ id: userId, username });
      expect(again.user!.display ?? 'Web Harness').toBe('Web Harness');
      expect(again.deviceId).toBe(deviceId);
      await ok(page, { m: 'selectCommunity', communityId: s.community_id });
      await ok(page, { m: 'openChannel', channelId: s.channel_id });
      await expect.poll(async () => (await timeline()).filter((i) => i.state === 'ok').map((i) => `${i.key} ${i.body}`), { timeout: WAIT })
        .toEqual(before);
      await peer.send('after the reload');
      await expect.poll(async () => (await timeline()).some((i) => i.body === 'after the reload' && i.state === 'ok'), { timeout: WAIT }).toBe(true);
      await ok(page, { m: 'send', channelId: s.channel_id, text: 'browser after the reload' });
      expect(await peerReceives('browser after the reload')).toMatchObject({ sender_user: userId, sender_device: deviceId, tier: 1 });
    });

    await test.step('a second page of the same origin waits as other-tab', async () => {
      const second = await context.newPage();
      second.on('pageerror', (e) => pageErrors.push(e.message));
      await openHarness(second);
      await expect.poll(() => phaseOf(second), { timeout: WAIT, message: 'the second page reports other-tab' }).toBe('other-tab');
      expect(await phaseOf(page)).toBe('ready');
      await second.close();
    });

    expect(pageErrors).toEqual([]);
  } finally {
    await peer.close();
  }
});

test('a second browser joins the account by password and recovery key, badges and marks read through the worker, and forgets itself', async ({ page, secondBrowser }) => {
  test.setTimeout(CI ? 900_000 : 300_000);
  const pageErrors: string[] = [];
  page.on('pageerror', (e) => pageErrors.push(e.message));
  const peer = await WebDriver.start(testHostUrl(), testkitEnv());
  const peerGot: PeerReceived[] = [];
  const peerReceives = async (body: string): Promise<PeerReceived> => {
    await expect.poll(async () => {
      peerGot.push(...(await peer.sync()).received);
      return peerGot.some((r) => r.body === body);
    }, { timeout: WAIT, message: `the peer receives ${JSON.stringify(body)}` }).toBe(true);
    return peerGot.find((r) => r.body === body)!;
  };
  const password = 'harness password 2';
  try {
    const s = await peer.setup({ community: 'enrol community', channel: 'general' });
    expect((await peer.register()).epoch).toBe(0);
    const username = `enr${(peer.seed & 0xff_ffff).toString(16).padStart(6, '0')}`;
    let recoveryKey: string[] = [];
    let userId = '';
    let factor!: Awaited<ReturnType<typeof completeFactor>>;
    let deviceA = '';
    let deviceB = '';

    await test.step('browser A signs up with a password and opens the channel', async () => {
      await openHarness(page);
      await expect.poll(() => phaseOf(page), { timeout: WAIT }).toBe('needs-signup');
      await ok(page, { m: 'signupBegin' });
      const keys = (await slice<AccountState>(page, 'account'))!;
      expect(keys.instance!.passwordSignup).toBe(true);
      recoveryKey = [...keys.recoveryKey!];
      expect(recoveryKey).toHaveLength(13);
      await ok(page, { m: 'signupSubmit', invite: instanceInvite(), username, display: 'Enrol A', password, recoveryKeyAcknowledged: true });
      factor = await completeFactor(page, CONTROL);
      const ready = (await slice<AccountState>(page, 'account'))!;
      expect(ready.phase).toBe('ready');
      userId = ready.user!.id;
      deviceA = ready.deviceId!;
      expect(await ok(page, { m: 'joinCommunity', invite: s.invite_code })).toEqual({ communityId: s.community_id });
      await ok(page, { m: 'selectCommunity', communityId: s.community_id });
      await ok(page, { m: 'openChannel', channelId: s.channel_id });
      await expect.poll(async () => (await slice<TimelineState>(page, `timeline:${s.channel_id}`))?.group, { timeout: WAIT }).toBe('active');
    });

    const { context: bContext, page: b } = await secondBrowser();
    b.on('pageerror', (e) => pageErrors.push(e.message));
    // CI1: B reads the peer's device list 3 s late, so the peer's message below arrives before B trusts its sender and
    // is held, then released by the trust refresh: the badge and notice checks cover that path on every run.
    await bContext.route(new RegExp(`/v1/users/${s.user_id}/device-list`), async (route) => {
      await new Promise((resolve) => { setTimeout(resolve, 3_000); });
      await route.continue();
    });

    await test.step('browser B signs in with the password and the recovery key; a wrong key is refused first', async () => {
      await openHarness(b);
      await expect.poll(() => phaseOf(b), { timeout: WAIT }).toBe('needs-signup');
      await ok(b, { m: 'signInBegin' });
      expect(await slice<AccountState>(b, 'account')).toMatchObject({ phase: 'signin-login', signIn: { username: null } });
      // web3.8 (R6): the password and the code first, the key last. Neither registers; signInKey does, after the key's
      // form check. A well-formed key of another account registers, then the root refuses it, and the registered
      // enrolment waits at signin-key.
      const wrong = [...recoveryKey];
      const first = wrong[0]!;
      wrong[0] = (first[0] === 'A' ? 'B' : 'A') + first.slice(1);
      await ok(b, { m: 'signInLogin', username, password });
      expect(await slice<AccountState>(b, 'account')).toMatchObject({ phase: 'signin-totp', signIn: { username } });
      await ok(b, { m: 'signInTotp', code: await nextCode(CONTROL, factor) });
      expect(await slice<AccountState>(b, 'account')).toMatchObject({ phase: 'signin-key', signIn: { username } });
      expect(await call(b, { m: 'signInKey', recoveryKey: wrong.join(' ') })).toMatchObject({ ok: false, code: 'E_RECOVERY_KEY' });
      expect(await slice<AccountState>(b, 'account')).toMatchObject({ phase: 'signin-key', signIn: { username } });
      await ok(b, { m: 'signInKey', recoveryKey: recoveryKey.join('-').toLowerCase() });
      const ready = (await slice<AccountState>(b, 'account'))!;
      expect(ready).toMatchObject({ phase: 'ready', user: { id: userId, username }, signIn: null, error: null });
      expect(ready.deviceId).toMatch(HEX32);
      expect(ready.deviceId).not.toBe(deviceA);
      deviceB = ready.deviceId!;
    });

    await test.step('B holds the channel group without opening it; a peer message badges and notifies', async () => {
      await expect.poll(async () => (await slice<ChannelSummary[]>(b, `channels:${s.community_id}`))?.find((c) => c.id === s.channel_id)?.group ?? null,
        { timeout: WAIT, message: 'join-all joined the channel group in B' }).toBe('active');
      await expect.poll(async () => (await peer.members()).devices, { timeout: WAIT }).toEqual(expect.arrayContaining([s.device_id, deviceA, deviceB]));
      await peer.send('to both browsers');
      await expect.poll(async () => (await slice<Record<string, BadgeState>>(b, 'badges'))?.[s.channel_id] ?? null, { timeout: WAIT })
        .toEqual({ unread: 1, mentions: 0 });
      // Pre-flight row 2.14(e): badges and notices are two messages, so notices are polled as badges are.
      await expect.poll(async () => ((await slice<NoticesState>(b, 'notices'))?.items ?? []).filter((n) => n.body === 'to both browsers'),
        { timeout: WAIT }).toEqual([expect.objectContaining({
        channelId: s.channel_id, communityId: s.community_id, kind: 'message', senderUser: s.user_id,
      })]);
      await expect.poll(async () => (await slice<TimelineState>(page, `timeline:${s.channel_id}`))?.items.some((i) => i.body === 'to both browsers') ?? false,
        { timeout: WAIT }).toBe(true);
    });

    await test.step('B marks the channel read and sends; A and the peer see B as the sender', async () => {
      await ok(b, { m: 'openChannel', channelId: s.channel_id });
      await ok(b, { m: 'markRead', channelId: s.channel_id });
      expect((await slice<Record<string, BadgeState>>(b, 'badges'))?.[s.channel_id]).toEqual({ unread: 0, mentions: 0 });
      await ok(b, { m: 'send', channelId: s.channel_id, text: 'from the second browser' });
      expect(await peerReceives('from the second browser')).toMatchObject({ sender_user: userId, sender_device: deviceB, tier: 1 });
      await expect.poll(async () => (await slice<TimelineState>(page, `timeline:${s.channel_id}`))?.items.find((i) => i.body === 'from the second browser') ?? null,
        { timeout: WAIT }).toMatchObject({ senderUser: userId, senderDevice: deviceB, own: false, web: true });
    });

    await test.step('settings round-trip and refuse a foreign key', async () => {
      const mute = `mute.channel.${s.channel_id}`;
      await ok(b, { m: 'setSetting', key: 'notify.default', value: 'everything' });
      await ok(b, { m: 'setSetting', key: mute, value: '1' });
      expect(await slice<Record<string, string>>(b, 'settings')).toEqual({ 'notify.default': 'everything', [mute]: '1' });
      await ok(b, { m: 'setSetting', key: 'notify.default', value: null });
      expect(await slice<Record<string, string>>(b, 'settings')).toEqual({ [mute]: '1' });
      expect(await call(b, { m: 'setSetting', key: 'theme', value: 'signal' })).toMatchObject({ ok: false, code: 'E_SETTING_KEY' });
    });

    await test.step('A lists both devices; B forgets itself into cleared, reloads to needs-signup on a fresh worker, and stays listed', async () => {
      const devices = async (): Promise<[string, boolean, boolean, number | null][]> => {
        await ok(page, { m: 'refreshDevices' });
        return ((await slice<DeviceSummary[]>(page, 'devices')) ?? []).map((d) => [d.id, d.listed, d.own, d.revokedAt]);
      };
      expect(await devices()).toEqual([[deviceA, true, true, null], [deviceB, true, false, null]]);
      await ok(b, { m: 'forgetBrowser' });
      expect(await slice<AccountState>(b, 'account')).toMatchObject({ phase: 'cleared', user: null, deviceId: null, error: null });
      // The harness has no App to answer `cleared` with a reload (L-TS-27): the spec reloads B by hand.
      await b.reload();
      await expect(b.locator('#status')).toHaveText('ready', { timeout: WAIT });
      await ok(b, { m: 'start' });
      await expect.poll(() => phaseOf(b), { timeout: WAIT }).toBe('needs-signup');
      expect(await devices()).toEqual([[deviceA, true, true, null], [deviceB, true, false, null]]);
    });

    expect(pageErrors).toEqual([]);
  } finally {
    await peer.close();
  }
});

/** A sync row of the native peer as L-E2E-20 (task 5) answers it; task 10 owns the typed driver wrappers. A row deleted
 *  before the peer ever held it answers msg_id, type, reply_to, sender_user, sender_device and tier as null (INTERFACES-11);
 *  every matcher below compares these fields with === and never calls a method on them. */
interface PeerRow extends Omit<PeerReceived, 'sender_user' | 'sender_device' | 'tier'> {
  sender_user: string | null; sender_device: string | null; tier: number | null;
  msg_id: string | null; type: number | null; reply_to: string | null; deleted: boolean;
  attachments: { index: number; blob_id: string; size: number; mime: string; name: string; thumb: boolean }[];
}

test('two devices of one account fold edits, reactions, replies, pins and deletes, and files go through the worker', async ({ page, secondBrowser }) => {
  test.setTimeout(CI ? 900_000 : 300_000);
  const pageErrors: string[] = [];
  page.on('pageerror', (e) => pageErrors.push(e.message));
  const peer = await WebDriver.start(testHostUrl(), testkitEnv());
  const peerRows: PeerRow[] = [];
  const peerSees = async (pick: (r: PeerRow) => boolean, what: string): Promise<PeerRow> => {
    await expect.poll(async () => {
      peerRows.push(...(await peer.request<{ received: PeerRow[] }>('sync', {})).received);
      return peerRows.some(pick);
    }, { timeout: WAIT, message: `the peer sees ${what}` }).toBe(true);
    return [...peerRows].reverse().find(pick)!;
  };
  const password = 'harness password 3';
  try {
    const s = await peer.setup({ community: 'fold community', channel: 'general' });
    expect((await peer.register()).epoch).toBe(0);
    // Pins count by the pinner's right, which @everyone lacks by default: the owner grants it, as its roles tab would.
    await peer.allowEveryone(1 << 3);
    const username = `fld${(peer.seed & 0xff_ffff).toString(16).padStart(6, '0')}`;
    const ch = s.channel_id;
    const itemsOf = async (p: Page): Promise<TimelineItem[]> => (await slice<TimelineState>(p, `timeline:${ch}`))?.items ?? [];
    const itemOf = async (p: Page, msgId: string): Promise<TimelineItem | null> => (await itemsOf(p)).find((i) => i.msgId === msgId) ?? null;
    let userId = '';
    let factor!: Awaited<ReturnType<typeof completeFactor>>;
    let recoveryKey = '';

    await test.step('A signs up with a password and opens the channel', async () => {
      await openHarness(page);
      await expect.poll(() => phaseOf(page), { timeout: WAIT }).toBe('needs-signup');
      await ok(page, { m: 'signupBegin' });
      recoveryKey = (await slice<AccountState>(page, 'account'))!.recoveryKey!.join(' ');
      await ok(page, { m: 'signupSubmit', invite: instanceInvite(), username, display: 'Fold A', password, recoveryKeyAcknowledged: true });
      factor = await completeFactor(page, CONTROL);
      userId = (await slice<AccountState>(page, 'account'))!.user!.id;
      expect(await ok(page, { m: 'joinCommunity', invite: s.invite_code })).toEqual({ communityId: s.community_id });
      await ok(page, { m: 'selectCommunity', communityId: s.community_id });
      await ok(page, { m: 'openChannel', channelId: ch });
      await expect.poll(async () => (await slice<TimelineState>(page, `timeline:${ch}`))?.group, { timeout: WAIT }).toBe('active');
    });

    const { page: b } = await secondBrowser();
    b.on('pageerror', (e) => pageErrors.push(e.message));

    await test.step('B signs in to the same account and opens the channel', async () => {
      await openHarness(b);
      await expect.poll(() => phaseOf(b), { timeout: WAIT }).toBe('needs-signup');
      await ok(b, { m: 'signInBegin' });
      await ok(b, { m: 'signInLogin', username, password });
      await ok(b, { m: 'signInTotp', code: await nextCode(CONTROL, factor) });
      await ok(b, { m: 'signInKey', recoveryKey });
      expect(await slice<AccountState>(b, 'account')).toMatchObject({ phase: 'ready', user: { id: userId } });
      await ok(b, { m: 'selectCommunity', communityId: s.community_id });
      await ok(b, { m: 'openChannel', channelId: ch });
      await expect.poll(async () => (await slice<TimelineState>(b, `timeline:${ch}`))?.group, { timeout: WAIT }).toBe('active');
    });

    let first = '';
    await test.step("A sends; B edits it (the same user); both show the edit and the peer receives a type-1 row", async () => {
      first = ((await ok(page, { m: 'send', channelId: ch, text: 'first words' })) as { msgId: string }).msgId;
      await expect.poll(async () => (await itemOf(b, first))?.body ?? null, { timeout: WAIT }).toBe('first words');
      await ok(b, { m: 'editMessage', channelId: ch, msgId: first, text: 'better words' });
      for (const p of [page, b]) {
        await expect.poll(async () => { const i = await itemOf(p, first); return i === null ? null : [i.body, i.edited]; }, { timeout: WAIT })
          .toEqual(['better words', true]);
      }
      expect(await peerSees((r) => r.type === 1 && r.reply_to === first, 'the edit')).toMatchObject({ body: 'better words', sender_user: userId });
    });

    await test.step("A and B both react 👍: one chip of count 1 (per user); the peer's 👍 makes it 2", async () => {
      await ok(page, { m: 'react', channelId: ch, msgId: first, emoji: '👍', on: true });
      await ok(b, { m: 'react', channelId: ch, msgId: first, emoji: '👍', on: true });
      // Both reactions are stored before the peer's: the peer's row comes after them in seq order, so a count of 2 (not 3)
      // in each browser is the per-user rule; a per-device fold passes through 2 on its way to 3, so the count is asserted
      // once, after the positive control below (TESTS-06), never polled for.
      await expect.poll(async () => {
        peerRows.push(...(await peer.request<{ received: PeerRow[] }>('sync', {})).received);
        return new Set(peerRows.filter((r) => r.type === 3 && r.reply_to === first).map((r) => r.sender_device)).size;
      }, { timeout: WAIT, message: 'the peer holds the reactions of both devices' }).toBe(2);
      await peer.request('send', { body: '👍', type: 3, reply_to: first });
      // The positive control (lesson c): a later message of the same group; once both browsers show it, every earlier
      // seq — both devices' reactions and the peer's — is folded, so the count below is the final one, not an intermediate.
      await peer.request('send', { body: 'after the reactions' });
      for (const p of [page, b]) {
        await expect.poll(async () => (await itemsOf(p)).some((i) => i.body === 'after the reactions'), { timeout: WAIT }).toBe(true);
        expect((await itemOf(p, first))?.reactions).toEqual([{ emoji: '👍', count: 2, mine: true }]);
      }
    });

    let peerMsg = '';
    let peerSeq = '';
    await test.step("B replies to the peer's message; A shows the reply reference", async () => {
      await peer.send('peer says hi');
      await expect.poll(async () => (await itemsOf(b)).find((i) => i.body === 'peer says hi')?.msgId ?? null, { timeout: WAIT }).not.toBeNull();
      const held = (await itemsOf(b)).find((i) => i.body === 'peer says hi')!;
      peerMsg = held.msgId!;
      peerSeq = held.seq!;
      const answer = ((await ok(b, { m: 'send', channelId: ch, text: 'answer', replyTo: peerMsg })) as { msgId: string }).msgId;
      await expect.poll(async () => (await itemOf(page, answer))?.reply ?? null, { timeout: WAIT })
        .toEqual({ msgId: peerMsg, state: 'ok', senderUser: s.user_id, excerpt: 'peer says hi', seq: peerSeq, threadRoot: null });
      expect(await peerSees((r) => r.msg_id === answer, 'the reply')).toMatchObject({ type: 0, reply_to: peerMsg, body: 'answer' });
    });

    await test.step("A pins the peer's message; the pins slice and B's item show it", async () => {
      await expect.poll(async () => (await itemOf(page, peerMsg))?.seq ?? null, { timeout: WAIT }).toBe(peerSeq);
      const peerTs = (await itemOf(page, peerMsg))!.ts;   // A's own item of the target: PinnedItem.ts is A's core's recv_ts of it
      await ok(page, { m: 'pin', channelId: ch, msgId: peerMsg, on: true });
      await ok(page, { m: 'loadPins', channelId: ch });
      await expect.poll(async () => await slice<PinnedItem[]>(page, `pins:${ch}`), { timeout: WAIT }).toEqual([{
        msgId: peerMsg, seq: peerSeq, senderUser: s.user_id, excerpt: 'peer says hi', pinnedBy: userId,
        pinnedSeq: expect.stringMatching(/^[1-9][0-9]*$/) as unknown as string, ts: peerTs, threadRoot: null,
      }]);
      await expect.poll(async () => (await itemOf(b, peerMsg))?.pinned ?? null, { timeout: WAIT }).toBe(true);
      await ok(page, { m: 'closePins', channelId: ch });
    });

    let fileMsg = '';
    let drawn: HarnessFile = { id: '', sha256: '', size: 0 };
    await test.step('A attaches a PNG drawn by canvas; the worker seals it and makes the thumbnail; B opens both', async () => {
      drawn = await page.evaluate((spec) => (window as unknown as HarnessWindow).dilla.makeFile(spec), { name: 'drawn.png', type: 'image/png', png: { w: 320, h: 240 } });
      const attached = await page.evaluate(([c, ids]) => (window as unknown as HarnessWindow).dilla.attach(c, ids), [ch, [drawn.id]] as const);
      expect(attached).toMatchObject({ ok: true });
      const trayId = ((attached as { ok: true; value: { trayIds: string[] } }).value.trayIds)[0]!;
      await expect.poll(async () => await slice<TrayItem[]>(page, `tray:${ch}`), { timeout: WAIT }).toEqual([
        { id: trayId, name: 'drawn.png', size: drawn.size, mime: 'image/png', image: true, phase: 'ready', reason: '' },
      ]);
      fileMsg = ((await ok(page, { m: 'send', channelId: ch, text: '', attachments: [trayId] })) as { msgId: string }).msgId;
      await expect.poll(async () => await slice<TrayItem[]>(page, `tray:${ch}`), { timeout: WAIT }).toEqual([]);
      await expect.poll(async () => { const i = await itemOf(b, fileMsg); return i === null ? null : [i.state, i.body, i.attachments]; }, { timeout: WAIT })
        .toEqual(['ok', '', [{ index: 0, size: drawn.size, mime: 'image/png', name: 'drawn.png', w: 320, h: 240, thumb: true, kind: 'image', tooLarge: false }]]);
      const seq = (await itemOf(b, fileMsg))!.seq!;
      const full: HarnessOpen = await b.evaluate((c) => (window as unknown as HarnessWindow).dilla.open(c),
        { m: 'openAttachment', channelId: ch, seq, index: 0, thumb: false } as const);
      expect(full).toMatchObject({ ok: true, sha256: drawn.sha256, size: drawn.size, type: 'image/png', name: 'drawn.png', mime: 'image/png' });
      const small: HarnessOpen = await b.evaluate((c) => (window as unknown as HarnessWindow).dilla.open(c),
        { m: 'openAttachment', channelId: ch, seq, index: 0, thumb: true } as const);
      expect(small.ok).toBe(true);
      if (!small.ok) return;
      const webp = small.head.slice(0, 4).join(',') === '82,73,70,70' && small.head.slice(8, 12).join(',') === '87,69,66,80';
      const jpeg = small.head.slice(0, 3).join(',') === '255,216,255';
      expect(webp || jpeg, `thumbnail head ${small.head.join(',')}`).toBe(true);
      expect(small.type).toBe(webp ? 'image/webp' : 'image/jpeg');
      expect(small.size).toBeLessThanOrEqual(8176);
    });

    await test.step('A deletes the file message: both show it deleted, the peer sees the delete, and the purge removes the file', async () => {
      const carried = await peerSees((r) => r.msg_id === fileMsg, 'the file message');
      expect(carried.attachments).toEqual([{ index: 0, blob_id: expect.stringMatching(/^[0-9a-f]{64}$/) as unknown as string,
        size: drawn.size, mime: 'image/png', name: 'drawn.png', thumb: true }]);
      const blobId = carried.attachments[0]!.blob_id;
      expect(await peer.request<{ status: number }>('blob_status', { blob_id: blobId })).toMatchObject({ status: 200 });
      await ok(page, { m: 'deleteMessage', channelId: ch, msgId: fileMsg, seq: String(carried.seq) });
      for (const p of [page, b]) {
        await expect.poll(async () => (await itemOf(p, fileMsg))?.state ?? null, { timeout: WAIT }).toBe('deleted');
      }
      expect(await peerSees((r) => r.type === 2 && r.reply_to === fileMsg, 'the type-2 row')).toMatchObject({ sender_user: userId });
      await peerSees((r) => r.msg_id === fileMsg && r.deleted, 'the tombstone');
      await expect.poll(async () => (await peer.request<{ status: number }>('blob_status', { blob_id: blobId })).status,
        { timeout: WAIT, message: 'the purge deleted the reference' }).toBe(404);
    });

    expect(pageErrors).toEqual([]);
  } finally {
    await peer.close();
  }
});

// The follower is a second page of the same context: the store, the Web Lock and BroadcastChannel are per origin
// and profile, so a second persistent profile (secondBrowser) would be a fresh device at needs-signup, never other-tab.
test('a pause failure retains the old Web Lock and leaves the follower outside', async ({ page, context }) => {
  await openHarness(page);
  await expect.poll(() => phaseOf(page), { timeout: WAIT }).toBe('needs-signup');
  await ok(page, { m: 'signupBegin' });
  const keys = (await slice<AccountState>(page, 'account'))!;
  await ok(page, { m: 'signupSubmit', invite: instanceInvite(), username: `busy${Date.now()}`, display: 'Busy Holder', password: keys.instance!.passwordSignup ? 'busy password 1' : null, recoveryKeyAcknowledged: true });
  await completeFactor(page, CONTROL);
  await expect.poll(() => phaseOf(page), { timeout: WAIT }).toBe('ready');
  const other = { page: await context.newPage() };
  try {
    await openHarness(other.page);
    await expect.poll(() => phaseOf(other.page), { timeout: WAIT }).toBe('other-tab');
    await page.evaluate(() => (window as unknown as HarnessWindow).dilla.hook('pause-fail-once'));
    const answer = await call(page, { m: 'resign', nonce: '01'.repeat(16) });
    expect(answer.ok).toBe(false);
    await expect.poll(() => phaseOf(page), { timeout: WAIT }).toBe('ready');
    await expect.poll(() => phaseOf(other.page), { timeout: WAIT }).toBe('other-tab');
    expect((await slice<AccountState>(page, 'account'))?.error?.code).not.toBe('E_RELEASED');
  } finally { await other.page.close(); }
});

/** Task 4's native call verbs (L-E2E-30); this case runs on the combined tree, where the driver has them. */
interface CallVerbs {
  callJoin(channelId: string): Promise<{ group: string; leaf: number }>;
  callMembers(channelId: string): Promise<{ group: string; devices: string[] }>;
}

// web-3 task 6 (CORE-E2E, combined tree only: it needs task 3's GET /v1/channels/{id}/call and task 4's call verbs). The
// native peer registers the DM's call group; a real browser core joins it by external commit (the staged join checked by
// call_join_check before it is posted), the SFU-less start's 501 maps to `failed`, the browser's self-Remove takes its
// leaf out of the group, and no key byte ever reaches a slice (review M8).
test('a real core joins a DM call group and maps the SFU-less 501 to failed after its self-Remove', async ({ page }) => {
  test.setTimeout(CI ? 600_000 : 180_000);
  const peer = await WebDriver.start(testHostUrl(), testkitEnv());
  const verbs = peer as unknown as CallVerbs;
  try {
    const setup = await peer.setup({ community: 'call community', channel: 'general' });
    await openHarness(page);
    await expect.poll(() => phaseOf(page), { timeout: WAIT }).toBe('needs-signup');
    await ok(page, { m: 'signupBegin' });
    const keys = (await slice<AccountState>(page, 'account'))!;
    await ok(page, { m: 'signupSubmit', invite: instanceInvite(), username: `join${Date.now() % 1_000_000}`, display: 'Joiner',
      password: keys.instance!.passwordSignup ? 'join password 1' : null, recoveryKeyAcknowledged: true });
    await completeFactor(page, CONTROL);
    await expect.poll(() => phaseOf(page), { timeout: WAIT }).toBe('ready');
    const account = (await slice<AccountState>(page, 'account'))!;
    const dm = await peer.openDm(account.user!.id);
    // The instance only proposes the browser's device; the peer's sync_dm commits that Add, and its Welcome is what puts the
    // DM into the browser's list (L-TS-22 rule 7), as badges.spec.ts does it.
    await expect.poll(async () => (await peer.syncDm(dm.channel_id)).members, { timeout: WAIT }).toBe(2);
    await expect.poll(async () => (await slice<{ id: string }[]>(page, 'dms'))?.some((d) => d.id === dm.channel_id) ?? false,
      { timeout: WAIT }).toBe(true);
    const peerCall = await verbs.callJoin(dm.channel_id);
    expect(peerCall.leaf).toBe(0);
    const answer = await call(page, { m: 'callJoin', channelId: dm.channel_id, vdec: '' });
    expect(answer.ok).toBe(false);
    if (!answer.ok) expect(answer.status).toBe(501);
    const failed = (await slice<{ phase: string; error: { status: number } | null; epoch: string | null; code: string | null }>(page, 'call'))!;
    expect(failed).toMatchObject({ phase: 'failed', error: { status: 501 }, epoch: null, code: null });
    // The browser joined the peer's group (its device was a member) and its self-Remove took the leaf out again.
    await expect.poll(async () => (await verbs.callMembers(dm.channel_id)).devices, { timeout: WAIT }).toEqual([setup.device_id]);
    expect((await verbs.callMembers(dm.channel_id)).group).toBe(peerCall.group);
    // No slice carries a key: no baseKey field and no ArrayBuffer anywhere.
    for (const name of ['call', 'voice', 'rings', 'dms', 'sendBlocks', 'account', 'trust']) {
      const value = await page.evaluate((n) => {
        const v = (window as unknown as HarnessWindow).dilla.get(n);
        const buffers = JSON.stringify(v, (_k, x: unknown) => (x instanceof ArrayBuffer || ArrayBuffer.isView(x) ? 'BUFFER' : x));
        return buffers ?? '';
      }, name);
      expect(value, name).not.toMatch(/baseKey|BUFFER/);
    }
  } finally {
    await peer.close();
  }
});

// web-3 task 6b (CORE-E2E, combined tree only: it needs task 3's GET /v1/channels/{id}/call). A real browser core registers
// a real call group for a DM on a test host with no SFU; the start's 501 maps to `failed` after the self-Remove, and a
// second join is allowed again.
test('a real core registers a DM call group and maps the SFU-less 501 to failed after its self-Remove', async ({ page }) => {
  test.setTimeout(CI ? 600_000 : 180_000);
  const peer = await WebDriver.start(testHostUrl(), testkitEnv());
  try {
    await peer.setup({ community: 'call community', channel: 'general' });
    await openHarness(page);
    await expect.poll(() => phaseOf(page), { timeout: WAIT }).toBe('needs-signup');
    await ok(page, { m: 'signupBegin' });
    const keys = (await slice<AccountState>(page, 'account'))!;
    await ok(page, { m: 'signupSubmit', invite: instanceInvite(), username: `call${Date.now() % 1_000_000}`, display: 'Caller',
      password: keys.instance!.passwordSignup ? 'call password 1' : null, recoveryKeyAcknowledged: true });
    await completeFactor(page, CONTROL);
    await expect.poll(() => phaseOf(page), { timeout: WAIT }).toBe('ready');
    const me = (await slice<AccountState>(page, 'account'))!.user!.id;
    const dm = await peer.openDm(me);
    // The peer's sync_dm commits the browser's Add; its Welcome puts the DM into the browser's list (L-TS-22 rule 7).
    await expect.poll(async () => (await peer.syncDm(dm.channel_id)).members, { timeout: WAIT }).toBe(2);
    await expect.poll(async () => (await slice<{ id: string }[]>(page, 'dms'))?.some((d) => d.id === dm.channel_id) ?? false,
      { timeout: WAIT }).toBe(true);
    await ok(page, { m: 'loadVoice', communityId: null, channelId: dm.channel_id });
    const answer = await call(page, { m: 'callJoin', channelId: dm.channel_id, vdec: '' });
    expect(answer.ok).toBe(false);
    if (!answer.ok) expect(answer.status).toBe(501);
    await expect.poll(async () => (await slice<{ phase: string }>(page, 'call'))?.phase, { timeout: WAIT }).toBe('failed');
    const callSlice = (await slice<{ error: { status: number } | null; epoch: string | null; code: string | null }>(page, 'call'))!;
    expect(callSlice).toMatchObject({ error: { status: 501 }, epoch: null, code: null });
    // The forgotten group lets the next attempt start over (a fresh read, the same refusal), never E_CALL_ACTIVE.
    const again = await call(page, { m: 'callJoin', channelId: dm.channel_id, vdec: '' });
    expect(again.ok).toBe(false);
    if (!again.ok) expect(again.code).not.toBe('E_CALL_ACTIVE');
  } finally {
    await peer.close();
  }
});
