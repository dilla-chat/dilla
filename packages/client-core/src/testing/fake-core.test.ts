import { describe, expect, it } from 'vitest';
import { arr, bin, decode, encode, str, u64 } from '../cbor';
import { CoreError } from '../core-port';
import { FAKE_RECOVERY_KEY, FAKE_ROOT_SEALED, FAKE_STATE_SEALED, FakeCore, fakeStateOf } from './fake-core';

import { fakeDskPub, fakeListBlob, readFakeList } from './fake-list';

const INSTANCE = new Uint8Array(16).fill(0xab);
const COMMUNITY = new Uint8Array(16).fill(0xc1);
const CHANNEL = new Uint8Array(16).fill(0xc2);
const GROUP = new Uint8Array(16).fill(0x67);
const DEV_A = new Uint8Array(16).fill(0xa1);
const USER_A = new Uint8Array(16).fill(0xa2);

const member = () => FakeCore.identified({ instanceId: INSTANCE, userId: USER_A, deviceId: DEV_A, username: 'ada' });

function caught(fn: () => unknown): CoreError {
  try {
    fn();
  } catch (err) {
    if (err instanceof CoreError) return err;
    throw err;
  }
  throw new Error('nothing was thrown');
}

const codeOf = (fn: () => unknown): string => caught(fn).code;

describe('FakeCore identity', () => {
  it('walks phase 0 → 1 → 2 with the ledger errors', () => {
    const core = new FakeCore({ deviceId: DEV_A });
    expect(core.identity()).toEqual({ phase: 0, instanceId: null, userId: null, deviceId: null, username: '', listPublished: false });
    expect(codeOf(() => core.sessionSign(new Uint8Array(32), 0))).toBe('E_CORE_NO_IDENTITY');
    expect(codeOf(() => core.sessionStore({ token: 't', expires: 2n, idleExpires: 1n }))).toBe('E_CORE_NO_IDENTITY');
    expect(caught(() => core.signupReset())).toMatchObject({ code: 'E_CORE_STATE', detail: 'no signup is pending' });
    expect(core.signupBegin(INSTANCE)).toBe(FAKE_RECOVERY_KEY);
    expect(FAKE_RECOVERY_KEY).toHaveLength(52);
    expect(core.identity()).toEqual({ phase: 1, instanceId: INSTANCE, userId: null, deviceId: DEV_A, username: '', listPublished: false });
    expect(codeOf(() => core.signupBegin(INSTANCE))).toBe('E_CORE_STATE');
    expect(codeOf(() => core.keyPackages(1, false))).toBe('E_CORE_NO_IDENTITY');
    expect(codeOf(() => core.sessionSign(new Uint8Array(32), 2 as unknown as 0))).toBe('E_CORE_INPUT');
    const req = arr(decode(core.signupRequest('INVITE', 'ada', 'Ada', null)), 8);
    expect([str(req[0] ?? null), str(req[1] ?? null), str(req[2] ?? null), req[6]]).toEqual(['INVITE', 'ada', 'Ada', null]);
    const deviceEntry = arr(req[7] ?? null, 5);
    expect(bin(deviceEntry[0] ?? null)).toEqual(DEV_A);
    expect(decode(bin(deviceEntry[4] ?? null))).toEqual(['fake.cred', new Uint8Array(16), DEV_A]);
    const list = core.signupComplete(USER_A, 'ada', 1_800_000_000n, new Uint8Array(32).fill(7));
    const body = arr(decode(list), 4);
    expect(u64(body[0] ?? null)).toBe(1n);
    expect(decode(bin(body[1] ?? null))).toEqual(['fake.list', USER_A, DEV_A, 1_800_000_000n]);
    expect(core.identity()).toEqual({ phase: 2, instanceId: INSTANCE, userId: USER_A, deviceId: DEV_A, username: 'ada', listPublished: false });
    expect(caught(() => core.signupReset())).toMatchObject({ code: 'E_CORE_STATE', detail: 'the identity is complete' });
    expect(core.deviceListBody()).toEqual(list);
    core.deviceListPublished();
    expect(core.identity().listPublished).toBe(true);
    const kp = arr(decode(core.keyPackages(3, true)), 2);
    expect(arr(kp[0] ?? null).map((k) => decode(bin(k)))).toEqual([
      ['fake.kp', DEV_A, 0n, 0n], ['fake.kp', DEV_A, 1n, 0n], ['fake.kp', DEV_A, 2n, 0n],
    ]);
    expect(decode(bin(kp[1] ?? null))).toEqual(['fake.kp', DEV_A, 3n, 1n]);
    expect(arr(decode(core.keyPackages(2, false)), 2)[1]).toBeNull();
    expect(codeOf(() => core.keyPackages(33, false))).toBe('E_CORE_INPUT');
    expect(decode(core.sessionSign(new Uint8Array(32).fill(9), 0))).toEqual([new Uint8Array(32).fill(9), 0n, new Uint8Array(64).fill(6), null, null]);
  });

  it('resets a pending signup that has no session record', () => {
    const core = new FakeCore();
    core.signupBegin(INSTANCE);
    core.signupReset();
    expect(core.identity().phase).toBe(0);
    expect(core.signupBegin(INSTANCE)).toBe(FAKE_RECOVERY_KEY);
  });

  it('refuses to reset a pending signup whose account is registered, and changes nothing', () => {
    const core = new FakeCore({ deviceId: DEV_A });
    core.signupBegin(INSTANCE);
    core.sessionStore({ token: 'tok', expires: 9n, idleExpires: 8n });
    const e = caught(() => core.signupReset());
    expect([e.code, e.detail]).toEqual(['E_CORE_STATE', 'the account is registered']);
    expect(core.identity()).toEqual({ phase: 1, instanceId: INSTANCE, userId: null, deviceId: DEV_A, username: '', listPublished: false });
    expect(core.session()).toEqual({ token: 'tok', expires: 9n, idleExpires: 8n });
  });

  it('keeps one session record', () => {
    const core = member();
    expect(core.session()).toBeNull();
    core.sessionStore({ token: 't', expires: 2n, idleExpires: 1n });
    expect(core.session()).toEqual({ token: 't', expires: 2n, idleExpires: 1n });
    core.sessionStore({ token: 'u', expires: 4n, idleExpires: 3n });
    expect(core.session()).toEqual({ token: 'u', expires: 4n, idleExpires: 3n });
    core.sessionClear();
    expect(core.session()).toBeNull();
  });
});

describe('FakeCore group half', () => {
  it('records every authoritySet call in hex, in order, and logs it (web3.5 R1)', () => {
    const core = member();
    const a = new Uint8Array(16).fill(0xa1);
    const b = new Uint8Array(16).fill(0xb2);
    core.authoritySet(GROUP, [a, b], [b], [a]);
    core.authoritySet(GROUP, [], [], []);
    const hex = (x: Uint8Array): string => Array.from(x, (v) => v.toString(16).padStart(2, '0')).join('');
    expect(core.authoritySets).toEqual([
      { groupId: hex(GROUP), pinners: [hex(a), hex(b)], broadcasters: [hex(b)], mentionable: [hex(a)] },
      { groupId: hex(GROUP), pinners: [], broadcasters: [], mentionable: [] },
    ]);
    expect(core.calls.filter((c) => c === 'authoritySet')).toHaveLength(2);
  });

  it('throws not modelled for every group, commit, send, outbox and timeline method, and logs the call', () => {
    const core = member();
    const apply = caught(() => core.groupApply(GROUP, new Uint8Array([0x80]), new Uint8Array([0x80]), 1n));
    expect([apply.code, apply.detail]).toEqual(['E_CORE_STATE', 'not modelled']);
    const send = caught(() => core.sendPrepare(GROUP, { type: 0, replyTo: null, body: 'hi', attachments: [], threadId: null }, 1n));
    expect([send.code, send.detail]).toEqual(['E_CORE_STATE', 'not modelled']);
    const others: [string, () => unknown][] = [
      ['groups', () => core.groups()],
      ['groupRow', () => core.groupRow(GROUP)],
      ['markRead', () => core.markRead(GROUP, 1n, 1n)],
      ['activity', () => core.activity()],
      ['groupCreate', () => core.groupCreate(GROUP, COMMUNITY, CHANNEL, 1n, new Uint8Array(32))],
      ['groupRegistered', () => core.groupRegistered(GROUP, 1n)],
      ['groupDiscard', () => core.groupDiscard(GROUP)],
      ['groupJoinExternal', () => core.groupJoinExternal({ groupId: GROUP, communityId: COMMUNITY, channelId: CHANNEL, policyVersion: 1n },
        new Uint8Array([0x80]), new Uint8Array([0x80]))],
      ['groupJoined', () => core.groupJoined(GROUP, 1n)],
      ['welcomesApply', () => core.welcomesApply(new Uint8Array([0x80]), [])],
      ['commitBuild', () => core.commitBuild(GROUP, new Uint8Array([0x80]))],
      ['commitConfirm', () => core.commitConfirm(GROUP)],
      ['commitAbort', () => core.commitAbort(GROUP)],
      ['cursorBody', () => core.cursorBody(GROUP)],
      ['cursorAcked', () => core.cursorAcked(GROUP, 1n, 1n)],
      ['messageDeleted', () => core.messageDeleted(GROUP, 1n)],
      ['removalCursor', () => core.removalCursor(GROUP)],
      ['removalActorHistoryVersion', () => core.removalActorHistoryVersion(GROUP, { revision: 1n, proof: new Uint8Array(), signature: new Uint8Array(64), acceptedAt: 1n })],
      ['refreshRemovalActorHistory', () => core.refreshRemovalActorHistory(GROUP, { revision: 1n, proof: new Uint8Array(), signature: new Uint8Array(64), acceptedAt: 1n }, new Uint8Array(), new Uint8Array())],
      ['ingestRemoval', () => core.ingestRemoval(GROUP, { revision: 1n, proof: new Uint8Array(), signature: new Uint8Array(64), acceptedAt: 1n })],
      ['sendEncrypt', () => core.sendEncrypt(GROUP)],
      ['sendConfirm', () => core.sendConfirm(GROUP, new Uint8Array([0x80]))],
      ['sendRequeue', () => core.sendRequeue(GROUP)],
      ['sendFail', () => core.sendFail(GROUP, 'E_X')],
      ['sendRetry', () => core.sendRetry(GROUP)],
      ['sendDiscard', () => core.sendDiscard(GROUP)],
      ['outbox', () => core.outbox(GROUP)],
      ['timeline', () => core.timeline(GROUP, 0n, 10)],
      ['pins', () => core.pins(GROUP)],
      ['attachmentGet', () => core.attachmentGet(GROUP, 1n, 0)],
      ['purges', () => core.purges()],
      ['purgeDone', () => core.purgeDone(GROUP, 1n)],
      ['ownRolesSet', () => core.ownRolesSet(COMMUNITY, [])],
    ];
    for (const [, fn] of others) expect(caught(fn)).toMatchObject({ code: 'E_CORE_STATE', detail: 'not modelled' });
    expect(core.calls).toEqual(['groupApply', 'sendPrepare', ...others.map(([name]) => name)]);
  });
});

describe('FakeCore test hooks', () => {
  it('logs calls, injects one failure, and refuses calls while paused', async () => {
    const core = member();
    core.failOnce('session', new CoreError('E_CORE_STORAGE', 'disk'));
    expect(codeOf(() => core.session())).toBe('E_CORE_STORAGE');
    expect(core.session()).toBeNull();
    expect(core.calls.slice(-2)).toEqual(['session', 'session']);
    core.pause();
    expect(codeOf(() => core.identity())).toBe('E_STORE_PAUSED');
    await core.resume();
    expect(core.identity().phase).toBe(2);
    core.close();
    expect(codeOf(() => core.identity())).toBe('E_STORE_PAUSED');
  });
});

const DEV_B = new Uint8Array(16).fill(0xb1);
const DEV_C = new Uint8Array(16).fill(0xc3);
const grouped = (FAKE_RECOVERY_KEY.toLowerCase().match(/.{4}/g) ?? []).join('-');

/** The 200 body of GET .../device-list naming entries at version v. */
function served(v: bigint, entries: { deviceId: Uint8Array; revokedAt: bigint | null }[]): Uint8Array {
  return encode([v, fakeListBlob(USER_A, entries, 1_800_000_000n), new Uint8Array(64).fill(5), new Uint8Array(32)]);
}

function enrolling(): FakeCore {
  const core = new FakeCore({ deviceId: DEV_B });
  core.enrolBegin(INSTANCE);
  core.enrolRegistered(USER_A);
  return core;
}

const complete = (core: FakeCore, recoveryKey: string, listBody: Uint8Array) =>
  core.enrolComplete({ recoveryKey, rootSealed: FAKE_ROOT_SEALED, stateSealed: FAKE_STATE_SEALED, listBody, username: 'ada', now: 1_800_000_100n });

describe('FakeCore enrolment (phase 3)', () => {
  it('walks phase 0 → 3 → 2 and builds the next list naming both devices', () => {
    const core = new FakeCore({ deviceId: DEV_B });
    expect(core.sealedObjects()).toEqual({ root: null, state: null, stateUploaded: false });
    const begun = core.enrolBegin(INSTANCE);
    expect(begun).toEqual({ deviceId: DEV_B, dskPub: fakeDskPub(DEV_B) });
    expect(core.identity()).toEqual({ phase: 3, instanceId: INSTANCE, userId: null, deviceId: DEV_B, username: '', listPublished: false });
    expect(codeOf(() => core.keyPackages(1, false))).toBe('E_CORE_NO_IDENTITY');
    expect(codeOf(() => core.deviceListBody())).toBe('E_CORE_NO_IDENTITY');
    expect(codeOf(() => core.ownDeviceList())).toBe('E_CORE_NO_IDENTITY');
    expect(caught(() => core.signupRequest('I', 'u', 'U', null))).toMatchObject({ code: 'E_CORE_STATE', detail: 'no signup is pending' });
    expect(caught(() => core.enrolBegin(INSTANCE))).toMatchObject({ code: 'E_CORE_STATE', detail: 'an enrolment is pending' });
    const login = new TextEncoder().encode('asrt-1');
    const body = arr(decode(core.enrolSessionSign(new Uint8Array(32).fill(9), login)), 5);
    expect(bin(body[4] ?? null)).toEqual(login);
    expect(bin(arr(body[3] ?? null, 5)[0] ?? null)).toEqual(DEV_B);
    expect(codeOf(() => core.enrolSessionSign(new Uint8Array(32), new Uint8Array(0)))).toBe('E_CORE_INPUT');
    expect(codeOf(() => core.enrolSessionSign(new Uint8Array(32), new Uint8Array(257)))).toBe('E_CORE_INPUT');
    expect(caught(() => complete(core, FAKE_RECOVERY_KEY, served(1n, [{ deviceId: DEV_A, revokedAt: null }]))))
      .toMatchObject({ code: 'E_CORE_STATE', detail: 'no user recorded' });
    core.sessionStore({ token: 'tok-p', expires: 9n, idleExpires: 8n });
    expect(codeOf(() => core.enrolRegistered(new Uint8Array(16)))).toBe('E_CORE_INPUT');
    core.enrolRegistered(USER_A);
    core.enrolRegistered(USER_A);
    expect(caught(() => core.enrolRegistered(new Uint8Array(16).fill(0x77)))).toMatchObject({ code: 'E_CORE_STATE', detail: 'user already recorded' });
    expect(caught(() => complete(core, FAKE_RECOVERY_KEY, served(1n, [{ deviceId: DEV_A, revokedAt: null }, { deviceId: DEV_B, revokedAt: null }]))))
      .toMatchObject({ code: 'E_CORE_STATE', detail: 'device is listed' });
    expect(codeOf(() => complete(core, FAKE_RECOVERY_KEY, encode([1, 2])))).toBe('E_CORE_INPUT');
    const out = complete(core, grouped, served(1n, [{ deviceId: DEV_A, revokedAt: null }]));
    const put = arr(decode(out.deviceListBody), 4);
    expect(u64(put[0] ?? null)).toBe(2n);
    expect(readFakeList(bin(put[1] ?? null))).toEqual({ userId: USER_A, at: 1_800_000_100n,
      entries: [{ deviceId: DEV_A, revokedAt: null }, { deviceId: DEV_B, revokedAt: null }] });
    expect(core.identity()).toEqual({ phase: 2, instanceId: INSTANCE, userId: USER_A, deviceId: DEV_B, username: 'ada', listPublished: false });
    expect(core.deviceListBody()).toEqual(out.deviceListBody);
    expect(core.sealedObjects()).toEqual({ root: FAKE_ROOT_SEALED, state: out.stateSealed, stateUploaded: false });
    expect(core.ownDeviceList().version).toBe(1n);
    core.deviceListPublished();
    expect(core.ownDeviceList()).toMatchObject({ version: 2n, published: true });
    expect(core.ownDeviceList().entries.map((e) => e.deviceId)).toEqual([DEV_A, DEV_B]);
    core.stateSealedUploaded();
    expect(core.sealedObjects().stateUploaded).toBe(true);
  });

  it('refuses a wrong key with E_RECOVERY_KEY, an empty detail, and keeps the enrolment', () => {
    const core = enrolling();
    const e = caught(() => complete(core, FAKE_RECOVERY_KEY.replace('0', '1'), served(1n, [{ deviceId: DEV_A, revokedAt: null }])));
    expect([e.code, e.detail]).toEqual(['E_RECOVERY_KEY', '']);
    expect(core.identity()).toMatchObject({ phase: 3, userId: USER_A });
    expect(core.sealedObjects().root).toBeNull();
  });

  it('enrolReset returns to phase 0 and drops the session record', () => {
    const core = enrolling();
    core.sessionStore({ token: 'tok-p', expires: 9n, idleExpires: 8n });
    core.enrolReset();
    expect(core.identity().phase).toBe(0);
    expect(core.session()).toBeNull();
    expect(caught(() => core.enrolReset())).toMatchObject({ code: 'E_CORE_STATE', detail: 'no enrolment is pending' });
  });

  it('never looks at the state object it is handed: an empty one succeeds (ruling 28)', () => {
    const remade = encode([1, new Uint8Array(12).fill(0x35), new Uint8Array(40).fill(0x36)]);
    const core = enrolling();
    const out = core.enrolComplete({ recoveryKey: FAKE_RECOVERY_KEY, rootSealed: FAKE_ROOT_SEALED, stateSealed: new Uint8Array(0),
      listBody: served(1n, [{ deviceId: DEV_A, revokedAt: null }]), username: 'ada', now: 1_800_000_100n });
    expect(out.stateSealed).toEqual(remade);
    expect(core.identity().phase).toBe(2);
    const listed = member();
    const revoked = listed.deviceListRevoke({ recoveryKey: FAKE_RECOVERY_KEY, rootSealed: FAKE_ROOT_SEALED, stateSealed: new Uint8Array(0),
      listBody: listed.deviceListBody(), deviceIds: [DEV_A], now: 1_800_000_200n });
    expect(revoked.stateSealed).toEqual(remade);
  });
});

describe('FakeCore own list, revocation and settings', () => {
  it('adopts newer lists from a history body and reports whether this device is listed', () => {
    const core = member();
    expect(core.ownDeviceListUpdate(encode([]))).toEqual({ version: 1n, listed: true });
    const history = encode([
      [2, fakeListBlob(USER_A, [{ deviceId: DEV_A, revokedAt: null }, { deviceId: DEV_B, revokedAt: null }], 2n), new Uint8Array(64), new Uint8Array(32)],
      [3, fakeListBlob(USER_A, [{ deviceId: DEV_A, revokedAt: 3n }, { deviceId: DEV_B, revokedAt: null }], 3n), new Uint8Array(64), new Uint8Array(32)],
    ]);
    expect(core.ownDeviceListUpdate(history)).toEqual({ version: 3n, listed: false });
    expect(codeOf(() => core.ownDeviceListUpdate(history))).toBe('E_DEVICE_LIST_STALE');
    expect(core.ownDeviceListUpdate(encode([]))).toEqual({ version: 3n, listed: false });
    expect(core.ownDeviceList().entries.map((e) => [e.deviceId, e.revokedAt])).toEqual([[DEV_A, 3n], [DEV_B, null]]);
  });

  it('drops an unpublished candidate that an adopted version overtook', () => {
    const core = enrolling();
    complete(core, FAKE_RECOVERY_KEY, served(1n, [{ deviceId: DEV_A, revokedAt: null }]));
    const row = [2, fakeListBlob(USER_A, [{ deviceId: DEV_A, revokedAt: null }, { deviceId: DEV_C, revokedAt: null }], 5n), new Uint8Array(64), new Uint8Array(32)];
    expect(core.ownDeviceListUpdate(encode([row]))).toEqual({ version: 2n, listed: false });
    expect(core.identity().listPublished).toBe(true);
    expect(core.deviceListBody()).toEqual(encode(row));
  });

  it('builds a revoking list for listed devices and refuses unknown ones', () => {
    const core = FakeCore.identified({ instanceId: INSTANCE, userId: USER_A, deviceId: DEV_A, username: 'ada',
      listEntries: [{ deviceId: DEV_A, revokedAt: null }, { deviceId: DEV_B, revokedAt: null }] });
    const listBody = core.deviceListBody();
    const input = { recoveryKey: FAKE_RECOVERY_KEY, rootSealed: FAKE_ROOT_SEALED, stateSealed: FAKE_STATE_SEALED, listBody, deviceIds: [DEV_B], now: 1_800_000_200n };
    const out = core.deviceListRevoke(input);
    const put = arr(decode(out.deviceListBody), 4);
    expect(u64(put[0] ?? null)).toBe(2n);
    expect(readFakeList(bin(put[1] ?? null))?.entries).toEqual([{ deviceId: DEV_A, revokedAt: null }, { deviceId: DEV_B, revokedAt: 1_800_000_200n }]);
    expect(core.identity().listPublished).toBe(false);
    expect(core.sealedObjects()).toMatchObject({ state: out.stateSealed, stateUploaded: false });
    expect(codeOf(() => core.deviceListRevoke({ ...input, deviceIds: [DEV_C] }))).toBe('E_CORE_NOT_FOUND');
    expect(codeOf(() => core.deviceListRevoke({ ...input, deviceIds: [] }))).toBe('E_CORE_INPUT');
    expect(codeOf(() => core.deviceListRevoke({ ...input, recoveryKey: 'nope' }))).toBe('E_RECOVERY_KEY');
  });

  // BACKUPS-RECOVERY-02, as the core does: a state object whose list is the served list's successor is an interrupted
  // publication, returned and signed on; a state list further on is the older-list refusal; a dropped candidate
  // falls back to the accepted list.
  it('signs on an interrupted publication, refuses a state list further on, and drops a candidate to the accepted list', () => {
    const core = FakeCore.identified({ instanceId: INSTANCE, userId: USER_A, deviceId: DEV_A, username: 'ada',
      listEntries: [{ deviceId: DEV_A, revokedAt: null }, { deviceId: DEV_B, revokedAt: null }] });
    const listBody = core.deviceListBody();
    const entriesV2 = [{ deviceId: DEV_A, revokedAt: null }, { deviceId: DEV_B, revokedAt: null }, { deviceId: DEV_C, revokedAt: null }];
    const v2 = encode([2n, fakeListBlob(USER_A, entriesV2, 1n), new Uint8Array(64).fill(5), new Uint8Array(32).fill(0x71)]);
    const input = { recoveryKey: FAKE_RECOVERY_KEY, rootSealed: FAKE_ROOT_SEALED, stateSealed: fakeStateOf(v2), listBody, deviceIds: [DEV_C], now: 1_800_000_300n };
    const v4 = encode([4n, fakeListBlob(USER_A, entriesV2, 1n), new Uint8Array(64).fill(5), new Uint8Array(32).fill(0x72)]);
    expect(caught(() => core.deviceListRevoke({ ...input, stateSealed: fakeStateOf(v4) })))
      .toMatchObject({ code: 'E_CORE_INPUT', detail: 'the instance served an older device list' });
    const out = core.deviceListRevoke(input);
    expect(out.interrupted).toEqual(v2);
    const put = arr(decode(out.deviceListBody), 4);
    expect(u64(put[0] ?? null)).toBe(3n);
    expect(readFakeList(bin(put[1] ?? null))?.entries).toEqual([{ deviceId: DEV_A, revokedAt: null }, { deviceId: DEV_B, revokedAt: null },
      { deviceId: DEV_C, revokedAt: 1_800_000_300n }]);
    expect(core.ownDeviceList().version).toBe(2n);
    core.deviceListDrop();
    expect(core.deviceListBody()).toEqual(v2);
    expect(core.identity().listPublished).toBe(false);
    expect(core.deviceListRevoke({ ...input, listBody: v2, stateSealed: FAKE_STATE_SEALED }).interrupted).toBeNull();
  });

  // Fix-wave review NEW-2, as the core does: a dropped candidate takes its sealed state with it; the state from before
  // the revocation comes back with its upload mark, and a published candidate keeps its own.
  it('a dropped sign-out restores the sealed state from before it; a published revocation keeps its own', () => {
    const core = FakeCore.identified({ instanceId: INSTANCE, userId: USER_A, deviceId: DEV_A, username: 'ada', stateUploaded: true,
      listEntries: [{ deviceId: DEV_A, revokedAt: null }, { deviceId: DEV_B, revokedAt: null }] });
    const before = core.sealedObjects();
    const input = { recoveryKey: FAKE_RECOVERY_KEY, rootSealed: FAKE_ROOT_SEALED, stateSealed: FAKE_STATE_SEALED,
      listBody: core.deviceListBody(), deviceIds: [DEV_A], now: 1_800_000_400n };
    core.deviceListRevoke(input);
    expect(core.sealedObjects()).not.toEqual(before);
    core.deviceListDrop();
    expect(core.sealedObjects()).toEqual(before);
    expect(core.stateSealedCurrent()).toBe(true);
    const out = core.deviceListRevoke({ ...input, deviceIds: [DEV_B] });
    core.deviceListPublished();
    core.deviceListDrop();
    expect(core.sealedObjects()).toMatchObject({ state: out.stateSealed, stateUploaded: false });
  });

  it('keeps settings in every phase within the byte bounds', () => {
    const core = new FakeCore();
    expect(core.settings()).toEqual({});
    core.settingPut('notify.default', 'everything');
    core.settingPut('mute.channel.' + 'a'.repeat(32), '1');
    core.settingDelete('mute.channel.' + 'a'.repeat(32));
    core.settingDelete('absent');
    expect(core.settings()).toEqual({ 'notify.default': 'everything' });
    for (const [k, v] of [['', 'x'], ['k'.repeat(129), 'x'], ['é'.repeat(65), 'x'], ['k', 'v'.repeat(1025)]] as const) {
      expect(codeOf(() => core.settingPut(k, v))).toBe('E_CORE_INPUT');
    }
    core.settingPut('k'.repeat(128), 'v'.repeat(1024));
    expect(Object.keys(core.settings())).toEqual(['k'.repeat(128), 'notify.default']);
  });
});
