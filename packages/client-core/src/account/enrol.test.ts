import { describe, expect, it, vi } from 'vitest';
import { arr, bin, decode, encode } from '../cbor';
import { toHex } from '../hex';
import { DillaHttpError } from '../http/errors';
import { FAKE_RECOVERY_KEY, FakeCore, fakeStateOf } from '../testing/fake-core';
import { fakeListBlob, readFakeList } from '../testing/fake-list';
import { FakeServer, NOW_S, sessionFor } from '../testing/fake-server';
import { Enrol, ensureBackups, refreshOwnDeviceList, repairBackupState, type EnrolFetched } from './enrol';

const INSTANCE = new Uint8Array(16).fill(0xab);
const DEVICE_A = new Uint8Array(16).fill(0xa1); // the account's first browser
const DEVICE_B = new Uint8Array(16).fill(0xb2); // this browser, enrolling
const B_HEX = toHex(DEVICE_B);
const PASSWORD = 'correct horse battery';
/** A root object of the real size: [1, nonce(12), ct(86)] is 103 bytes (L-HTTP-50). */
const ROOT = encode([1, new Uint8Array(12).fill(1), new Uint8Array(86).fill(2)]);
const STATE = encode([1, new Uint8Array(12).fill(3), new Uint8Array(200).fill(4)]);
const ONLY_A = [{ deviceId: DEVICE_A, revokedAt: null }];
const A_AND_B = [{ deviceId: DEVICE_A, revokedAt: null }, { deviceId: DEVICE_B, revokedAt: null }];
const utf8 = (s: string): Uint8Array => new TextEncoder().encode(s);
/** The log with each request's query string (task 10's paths() prints the pathname only). */
const lines = (s: FakeServer): string[] => s.log.map((r) => `${r.method} ${r.path}${r.query}`);

async function httpError(p: Promise<unknown>): Promise<DillaHttpError> {
  try {
    await p;
  } catch (err) {
    if (err instanceof DillaHttpError) return err;
    throw err;
  }
  throw new Error('nothing was thrown');
}

/** An account whose first browser (DEVICE_A) published list v1 naming only itself and, unless switched off, both
 *  sealed objects; this browser is DEVICE_B. */
function account(opts: { totp?: string; root?: boolean; state?: boolean } = {}) {
  const server = new FakeServer();
  const userId = server.register('ada', DEVICE_A);
  server.setPassword('ada', PASSWORD, opts.totp);
  server.publishList(userId, ONLY_A, 1n);
  if (opts.root !== false) server.putBackupObject(userId, 0, ROOT);
  if (opts.state !== false) server.putBackupObject(userId, 1, STATE);
  const core = new FakeCore({ deviceId: DEVICE_B });
  const { routes, session } = sessionFor(server, core);
  const enrol = new Enrol({ core, routes, session, now: () => NOW_S * 1000 });
  const U = toHex(userId);
  return { server, core, routes, session, enrol, userId, U, listPath: `/v1/users/${U}/device-list` };
}

async function upToKey(a: ReturnType<typeof account>): Promise<EnrolFetched> {
  await a.enrol.login('ada', PASSWORD);
  await a.enrol.register(INSTANCE);
  return a.enrol.fetch(a.userId);
}

describe('Enrol', () => {
  it('adds this browser: login, a pending registration, the sealed objects, the key, list v2, an enrolled session, the state object', async () => {
    const { server, core, routes, enrol, userId, U, listPath } = account();
    expect(await enrol.login('ada', PASSWORD)).toEqual({ needs: 0 });
    expect(await enrol.register(INSTANCE)).toEqual({ userId });
    expect(core.identity()).toMatchObject({ phase: 3, userId, deviceId: DEVICE_B });
    const fetched = await enrol.fetch(userId);
    expect([fetched.root, fetched.state]).toEqual([ROOT, STATE]);
    const served = arr(decode(fetched.listBody), 4);
    expect([served[0], served[1]]).toEqual([1n, fakeListBlob(userId, ONLY_A, 1n)]);
    await enrol.complete(FAKE_RECOVERY_KEY, fetched, 'ada');
    expect(server.paths()).toEqual([
      'POST /v1/auth/password/login',
      `POST /v1/devices/${B_HEX}/sessions/challenge`, `POST /v1/devices/${B_HEX}/sessions`,
      'GET /v1/backups/0/0', 'GET /v1/backups/1/0', `GET ${listPath}`,
      `PUT ${listPath}`,
      `POST /v1/devices/${B_HEX}/sessions/challenge`, `POST /v1/devices/${B_HEX}/sessions`,
      'GET /v1/instance', 'PUT /v1/backups/1/0',
    ]);
    expect(server.log.map((r) => r.auth)).toEqual([null, null, null, 'tok-1', 'tok-1', 'tok-1', 'tok-1', null, null, 'tok-2', 'tok-2']);
    expect([server.tokenScope.get('tok-1'), server.tokenScope.get('tok-2')]).toEqual([1, 0]);
    const steps = ['enrolBegin', 'enrolSessionSign', 'sessionStore', 'enrolRegistered', 'enrolComplete', 'deviceListPublished', 'instanceSignerKeyPin',
      'sessionSign', 'stateSealedUploaded'];
    expect(core.calls.filter((c) => steps.includes(c))).toEqual(['enrolBegin', 'enrolSessionSign', 'sessionStore', 'enrolRegistered',
      'enrolComplete', 'deviceListPublished', 'sessionSign', 'sessionStore', 'instanceSignerKeyPin', 'stateSealedUploaded']);
    expect(core.instanceSignerPub).toEqual(server.externalSenderPub);
    expect(core.identity()).toEqual({ phase: 2, instanceId: INSTANCE, userId, deviceId: DEVICE_B, username: 'ada', listPublished: true });
    expect(core.session()?.token).toBe('tok-2');
    expect(server.deviceLists.get(U)?.version).toBe(2n);
    expect(readFakeList(server.deviceLists.get(U)?.blob ?? new Uint8Array(0))?.entries).toEqual(A_AND_B);
    expect(decode(server.log[10]?.body ?? new Uint8Array(0))).toEqual([core.sealedObjects().state]);
    expect((await routes.getBackup(1))?.object).toEqual(core.sealedObjects().state);
    expect((await routes.getBackup(0))?.object).toEqual(ROOT);
  });

  it('refuses authenticated discovery that conflicts with an existing instance signer pin', async () => {
    const a = account();
    a.core.instanceSignerPub = new Uint8Array(32).fill(0x44);
    const fetched = await upToKey(a);

    await expect(a.enrol.complete(FAKE_RECOVERY_KEY, fetched, 'ada')).rejects.toMatchObject({ code: 'E_CORE_STATE' });
    expect(a.server.paths().slice(-4)).toEqual([
      `PUT ${a.listPath}`, `POST /v1/devices/${B_HEX}/sessions/challenge`,
      `POST /v1/devices/${B_HEX}/sessions`, 'GET /v1/instance',
    ]);
    expect(a.server.count('GET', '/v1/instance')).toBe(1);
    expect(a.server.count('PUT', '/v1/backups/1/0')).toBe(0);
    expect(a.core.instanceSignerPub).toEqual(new Uint8Array(32).fill(0x44));
  });

  it('puts the assertion in the registration body only, and zero-fills its bytes afterwards', async () => {
    const a = account();
    let login: Uint8Array = new Uint8Array(0);
    const sign = a.core.enrolSessionSign.bind(a.core);
    vi.spyOn(a.core, 'enrolSessionSign').mockImplementation((nonce, bytes) => {
      login = bytes;
      return sign(nonce, bytes);
    });
    await a.enrol.login('ada', PASSWORD);
    await a.enrol.register(INSTANCE);
    const assertion = a.server.assertions[0] ?? '';
    expect(assertion).not.toBe('');
    const body = arr(decode(a.server.log[2]?.body ?? new Uint8Array()), 5);
    expect(body[1]).toBe(0n);
    expect(body[4]).toEqual(utf8(assertion));
    expect(bin(arr(body[3] ?? null, 5)[0] ?? null, 16)).toEqual(DEVICE_B);
    expect(login.length).toBe(utf8(assertion).length);
    expect(login.every((b) => b === 0)).toBe(true);
    const others = a.server.log.filter((_, i) => i !== 2).map((r) => new TextDecoder().decode(r.body));
    for (const text of others) expect(text.includes(assertion)).toBe(false);
  });

  it('a refused password is final: no assertion, no enrolment', async () => {
    const { server, core, enrol } = account();
    const err = await httpError(enrol.login('ada', 'wrong'));
    expect([err.status, err.code]).toEqual([401, 'E_UNAUTHENTICATED']);
    await expect(enrol.register(INSTANCE)).rejects.toThrow('E_NO_ASSERTION');
    expect(server.paths()).toEqual(['POST /v1/auth/password/login']);
    expect(core.calls).not.toContain('enrolBegin');
    expect(core.identity().phase).toBe(0);
  });

  it('verifies the second factor before registering; a wrong code spends the assertion', async () => {
    const { server, core, enrol } = account({ totp: '123456' });
    expect(await enrol.login('ada', PASSWORD)).toEqual({ needs: 1 });
    await expect(enrol.register(INSTANCE)).rejects.toThrow('E_NO_ASSERTION');
    const wrong = await httpError(enrol.totp('000000'));
    expect([wrong.status, wrong.code]).toEqual([401, 'E_UNAUTHENTICATED']);
    await expect(enrol.totp('123456')).rejects.toThrow('E_NO_ASSERTION');
    expect(await enrol.login('ada', PASSWORD)).toEqual({ needs: 1 });
    await enrol.totp('123456');
    await enrol.register(INSTANCE);
    expect(server.paths()).toEqual([
      'POST /v1/auth/password/login', 'POST /v1/auth/totp/verify',
      'POST /v1/auth/password/login', 'POST /v1/auth/totp/verify',
      `POST /v1/devices/${B_HEX}/sessions/challenge`, `POST /v1/devices/${B_HEX}/sessions`,
    ]);
    expect(server.assertions).toHaveLength(3);
    const body = arr(decode(server.log[5]?.body ?? new Uint8Array()), 5);
    expect(body[4]).toEqual(utf8(server.assertions[2] ?? ''));
    expect(core.calls.filter((c) => c === 'enrolBegin')).toHaveLength(1);
    expect(core.identity()).toMatchObject({ phase: 3, deviceId: DEVICE_B });
  });

  it('a pending session is required: an enrolled answer to a registration is refused and nothing is stored', async () => {
    const { server, core, enrol } = account();
    server.scope = 0;
    await enrol.login('ada', PASSWORD);
    await expect(enrol.register(INSTANCE)).rejects.toThrow('E_SESSION_SCOPE');
    expect(core.session()).toBeNull();
    expect(core.calls).not.toContain('enrolRegistered');
    expect(core.identity()).toMatchObject({ phase: 3, userId: null, deviceId: DEVICE_B });
    await expect(enrol.register(INSTANCE)).rejects.toThrow('E_NO_ASSERTION');
  });

  it('a registration whose answer was lost is retried with the same device: the instance finds the row and answers pending', async () => {
    const { server, core, enrol, userId } = account();
    const sessions = `/v1/devices/${B_HEX}/sessions`;
    server.once('POST', sessions, 'lose');
    await enrol.login('ada', PASSWORD);
    const lost = await httpError(enrol.register(INSTANCE));
    expect([lost.status, lost.code]).toEqual([0, 'E_NETWORK']);
    expect(core.identity()).toMatchObject({ phase: 3, userId: null, deviceId: DEVICE_B });
    expect(server.devices.get(B_HEX)).toBe(toHex(userId)); // the instance created the row before the answer was lost
    await enrol.login('ada', PASSWORD);
    expect(await enrol.register(INSTANCE)).toEqual({ userId });
    expect(core.calls.filter((c) => c === 'enrolBegin')).toHaveLength(1);
    expect(server.count('POST', sessions)).toBe(2);
    expect(server.tokenScope.get('tok-2')).toBe(1);
  });

  it('a registration the instance refused before creating a row is retried with the same device, which registers it then', async () => {
    const { server, core, enrol, userId } = account();
    const sessions = `/v1/devices/${B_HEX}/sessions`;
    // The one refusal a registration meets before its row exists: a cap whose live rows are all listed (DEVICE_A).
    server.maxDevices = 1;
    await enrol.login('ada', PASSWORD);
    const refused = await httpError(enrol.register(INSTANCE));
    expect([refused.status, refused.code, refused.detail]).toEqual([403, 'E_FORBIDDEN', 'device cap reached']);
    expect(core.identity()).toMatchObject({ phase: 3, userId: null, deviceId: DEVICE_B });
    expect(server.devices.has(B_HEX)).toBe(false);
    server.maxDevices = 8; // the operator raised the cap (or the owner revoked a listed device)
    await enrol.login('ada', PASSWORD);
    expect(await enrol.register(INSTANCE)).toEqual({ userId });
    const posts = server.log.filter((r) => r.method === 'POST' && r.path.endsWith('/sessions')).map((r) => r.path);
    expect(posts).toEqual([sessions, sessions]);
    expect(Array.isArray(arr(decode(server.log[server.log.length - 1]?.body ?? new Uint8Array(0)), 5)[3])).toBe(true);
    expect(core.calls.filter((c) => c === 'enrolBegin')).toHaveLength(1);
    expect(server.devices.get(B_HEX)).toBe(toHex(userId));
  });

  // A 429 at registration comes only from the per-address and per-device establish meter now (protocol/02 refusals).
  it('does not retry a spent registration assertion when the rate wait is 1000 ms', async () => {
    const a = account();
    a.server.once('POST', `/v1/devices/${B_HEX}/sessions`, { status: 429, body: ['E_RATE_LIMITED', '', 1000] });
    await a.enrol.login('ada', PASSWORD);
    const err = await httpError(a.enrol.register(INSTANCE));
    expect([err.status, err.code, err.retryAfterMs]).toEqual([429, 'E_RATE_LIMITED', 1000]);
    expect(a.server.count('POST', `/v1/devices/${B_HEX}/sessions`)).toBe(1);
    expect(a.core.identity()).toMatchObject({ phase: 3, userId: null });
  });

  it('a cap of listed devices reaches the caller as 403 device cap reached; the cap and the rate otherwise replace, never refuse', async () => {
    const capped = account();
    capped.server.maxDevices = 1;
    await capped.enrol.login('ada', PASSWORD);
    const cap = await httpError(capped.enrol.register(INSTANCE));
    expect([cap.status, cap.code, cap.detail]).toEqual([403, 'E_FORBIDDEN', 'device cap reached']);
    expect(capped.server.devices.has(B_HEX)).toBe(false);
    expect(capped.core.identity()).toMatchObject({ phase: 3, userId: null });

    const rated = account();
    rated.server.enrolmentsPerHour = 1; // DEVICE_A, created this hour, already fills it (ruling 32), and it is listed
    await rated.enrol.login('ada', PASSWORD);
    expect(await rated.enrol.register(INSTANCE)).toEqual({ userId: rated.userId });
    expect([rated.server.devices.get(B_HEX), rated.server.revoked.size]).toEqual([rated.U, 0]);

    // A password holder's unlisted row at the cap: this browser's registration replaces it and is admitted.
    const full = account();
    const planted = new Uint8Array(16).fill(0xee);
    full.server.devices.set(toHex(planted), full.U);
    full.server.createdAt.set(toHex(planted), NOW_S);
    full.server.maxDevices = 2;
    await full.enrol.login('ada', PASSWORD);
    expect(await full.enrol.register(INSTANCE)).toEqual({ userId: full.userId });
    expect([full.server.revoked.has(toHex(planted)), full.server.revoked.has(toHex(DEVICE_A))]).toEqual([true, false]);
  });

  it('a missing root object is E_NO_BACKUP and nothing more is read', async () => {
    const a = account({ root: false });
    await a.enrol.login('ada', PASSWORD);
    await a.enrol.register(INSTANCE);
    await expect(a.enrol.fetch(a.userId)).rejects.toThrow('E_NO_BACKUP');
    expect(a.server.paths().slice(-1)).toEqual(['GET /v1/backups/0/0']);
  });

  it('a 404 on the state is not an error: fetch resolves with an empty state and complete passes it to enrolComplete unchanged', async () => {
    const a = account({ state: false });
    const seen: Uint8Array[] = [];
    const complete = a.core.enrolComplete.bind(a.core);
    vi.spyOn(a.core, 'enrolComplete').mockImplementation((input) => {
      seen.push(input.stateSealed);
      return complete(input);
    });
    const fetched = await upToKey(a);
    expect(fetched.state).toEqual(new Uint8Array(0));
    expect(a.server.paths().slice(-3)).toEqual(['GET /v1/backups/0/0', 'GET /v1/backups/1/0', `GET ${a.listPath}`]);
    await a.enrol.complete(FAKE_RECOVERY_KEY, fetched, 'ada');
    expect(seen.map((s) => s.length)).toEqual([0]);
    expect(a.core.identity()).toMatchObject({ phase: 2, listPublished: true });
    expect(a.server.paths().slice(-1)).toEqual(['PUT /v1/backups/1/0']);
    expect((await a.routes.getBackup(1))?.object).toEqual(a.core.sealedObjects().state);
  });

  // WORKER-WEB-01: the pending session must not outlive the list that names this browser, or a failed or
  // interrupted establish leaves a pending token that ensure() keeps for hours.
  it('drops the pending session once the list is published: a failed establish leaves no session, and ensure establishes an enrolled one', async () => {
    const a = account();
    const fetched = await upToKey(a);
    expect(a.core.session()?.token).toBe('tok-1');
    a.server.once('POST', `/v1/devices/${B_HEX}/sessions`, 'network');
    await expect(a.enrol.complete(FAKE_RECOVERY_KEY, fetched, 'ada')).rejects.toMatchObject({ code: 'E_NETWORK' });
    expect(a.core.identity()).toMatchObject({ phase: 2, listPublished: true });
    expect(a.core.session()).toBeNull();
    const steps = ['deviceListPublished', 'sessionClear', 'sessionSign'];
    expect(a.core.calls.filter((c) => steps.includes(c))).toEqual(['deviceListPublished', 'sessionClear', 'sessionSign']);
    expect(await a.session.ensure()).toBe(true);
    expect(a.core.session()?.token).toBe('tok-2');
    expect(a.server.tokenScope.get('tok-2')).toBe(0);
  });

  // REGISTRATION-DEVICES-02: a password holder's registrations replaced this browser's unlisted row before its
  // list PUT (or between the PUT and the establish); the caller tells the person, it is not a revocation.
  it('a row evicted before the list PUT is E_SIGNIN_EVICTED', async () => {
    const a = account();
    const fetched = await upToKey(a);
    a.server.revoked.set(B_HEX, NOW_S);
    await expect(a.enrol.complete(FAKE_RECOVERY_KEY, fetched, 'ada')).rejects.toThrow('E_SIGNIN_EVICTED');
    expect(a.server.paths()).toContain(`PUT ${a.listPath}`);
    expect(a.server.deviceLists.get(a.U)?.version).toBe(1n);
  });

  it('a row evicted between the list PUT and the establish is E_SIGNIN_EVICTED', async () => {
    const a = account();
    const fetched = await upToKey(a);
    const refused = { status: 401, body: ['E_UNAUTHENTICATED', '', null] };
    a.server.once('POST', `/v1/devices/${B_HEX}/sessions`, refused);
    a.server.once('POST', `/v1/devices/${B_HEX}/sessions`, refused);
    await expect(a.enrol.complete(FAKE_RECOVERY_KEY, fetched, 'ada')).rejects.toThrow('E_SIGNIN_EVICTED');
    expect(a.server.deviceLists.get(a.U)?.version).toBe(2n);
    expect(a.server.paths()).not.toContain('PUT /v1/backups/1/0');
  });

  // BACKUPS-RECOVERY-02: DEVICE_A sealed the state object of its v2 and lost the list PUT; the instance serves v1.
  const X = new Uint8Array(16).fill(0xc3);
  const interruptedV2 = (userId: Uint8Array): Uint8Array => encode([2n, fakeListBlob(userId, [...ONLY_A, { deviceId: X, revokedAt: null }], 1n),
    new Uint8Array(64).fill(5), new Uint8Array(32).fill(0x71)]);

  it('an interrupted publication in the state object goes to the instance first, and this browser joins the list after it', async () => {
    const a = account({ state: false });
    const v2 = interruptedV2(a.userId);
    a.server.putBackupObject(a.userId, 1, fakeStateOf(v2));
    const fetched = await upToKey(a);
    await a.enrol.complete(FAKE_RECOVERY_KEY, fetched, 'ada');
    expect(lines(a.server).filter((l) => l.startsWith('PUT'))).toEqual([`PUT ${a.listPath}`, `PUT ${a.listPath}`, 'PUT /v1/backups/1/0']);
    expect(decode(a.server.log.find((r) => r.method === 'PUT')?.body ?? new Uint8Array(0))).toEqual(decode(v2));
    expect((a.server.listHistory.get(a.U) ?? []).map((row) => row.version)).toEqual([1n, 2n, 3n]);
    expect(readFakeList(a.server.deviceLists.get(a.U)?.blob ?? new Uint8Array(0))?.entries)
      .toEqual([...ONLY_A, { deviceId: X, revokedAt: null }, { deviceId: DEVICE_B, revokedAt: null }]);
    expect(a.core.identity()).toMatchObject({ phase: 2, listPublished: true });
  });

  it('an interrupted publication the instance already holds (409) does not stop the enrolment', async () => {
    const a = account({ state: false });
    const v2 = interruptedV2(a.userId);
    a.server.putBackupObject(a.userId, 1, fakeStateOf(v2));
    const fetched = await upToKey(a);
    a.server.once('PUT', a.listPath, { status: 409, body: ['E_INVALID_REQUEST', '', null] });
    a.server.publishList(a.userId, [...ONLY_A, { deviceId: X, revokedAt: null }], 1n); // another device published it
    await a.enrol.complete(FAKE_RECOVERY_KEY, fetched, 'ada');
    expect(a.server.deviceLists.get(a.U)?.version).toBe(3n);
  });

  it('an interrupted publication refused otherwise starts the sign-in over (E_LIST_RACE), nothing else is sent', async () => {
    const a = account({ state: false });
    a.server.putBackupObject(a.userId, 1, fakeStateOf(interruptedV2(a.userId)));
    const fetched = await upToKey(a);
    a.server.once('PUT', a.listPath, { status: 400, body: ['E_INVALID_REQUEST', '', null] });
    await expect(a.enrol.complete(FAKE_RECOVERY_KEY, fetched, 'ada')).rejects.toThrow('E_LIST_RACE');
    expect(lines(a.server).filter((l) => l.startsWith('PUT'))).toEqual([`PUT ${a.listPath}`]);
    expect(a.server.deviceLists.get(a.U)?.version).toBe(1n);
  });

  it('a wrong recovery key changes nothing, and the right one works after it', async () => {
    const a = account();
    const fetched = await upToKey(a);
    const err: unknown = await a.enrol.complete('Z'.repeat(52), fetched, 'ada').catch((e: unknown) => e);
    expect(err).toMatchObject({ name: 'CoreError', code: 'E_RECOVERY_KEY' });
    expect(a.core.identity()).toMatchObject({ phase: 3, userId: a.userId, deviceId: DEVICE_B });
    expect(a.server.paths().filter((p) => p.startsWith('PUT'))).toEqual([]);
    await a.enrol.complete(FAKE_RECOVERY_KEY, fetched, 'ada');
    expect(a.core.identity()).toMatchObject({ phase: 2, listPublished: true });
  });

  it('a list another device published meanwhile, without this browser, is E_LIST_RACE and nothing more is sent', async () => {
    const a = account();
    const fetched = await upToKey(a);
    a.server.publishList(a.userId, ONLY_A); // v2 by DEVICE_A, naming only itself
    await expect(a.enrol.complete(FAKE_RECOVERY_KEY, fetched, 'ada')).rejects.toThrow('E_LIST_RACE');
    expect(lines(a.server).slice(-3)).toEqual([`PUT ${a.listPath}`, `GET ${a.listPath}`, `GET ${a.listPath}?after=1`]);
    expect(a.server.count('POST', `/v1/devices/${B_HEX}/sessions`)).toBe(1);
    expect(a.server.paths()).not.toContain('PUT /v1/backups/1/0');
    expect(a.core.calls).not.toContain('deviceListPublished');
    expect(a.core.ownDeviceList().version).toBe(2n);
  });

  it('a lost answer to its own list is recognised through the history and the enrolment completes', async () => {
    const a = account();
    const fetched = await upToKey(a);
    a.server.once('PUT', a.listPath, 'lose');
    await a.enrol.complete(FAKE_RECOVERY_KEY, fetched, 'ada');
    expect(lines(a.server).slice(-8)).toEqual([
      `PUT ${a.listPath}`, `PUT ${a.listPath}`, `GET ${a.listPath}`, `GET ${a.listPath}?after=1`,
      `POST /v1/devices/${B_HEX}/sessions/challenge`, `POST /v1/devices/${B_HEX}/sessions`, 'GET /v1/instance', 'PUT /v1/backups/1/0',
    ]);
    expect(a.server.deviceLists.get(a.U)?.version).toBe(2n);
    expect(readFakeList(a.server.deviceLists.get(a.U)?.blob ?? new Uint8Array(0))?.entries).toEqual(A_AND_B);
    expect(a.core.identity()).toMatchObject({ phase: 2, listPublished: true });
    // The core adopted its own v2 from the history and dropped the candidate itself (task 10, FakeCore 6i).
    expect(a.core.calls).not.toContain('deviceListPublished');
  });

  it('reset drops the enrolment and the assertion', async () => {
    const a = account();
    await a.enrol.login('ada', PASSWORD);
    await a.enrol.register(INSTANCE);
    a.enrol.reset();
    expect(a.core.identity().phase).toBe(0);
    expect(a.core.calls).toContain('enrolReset');
    await expect(a.enrol.register(INSTANCE)).rejects.toThrow('E_NO_ASSERTION');

    const b = account();
    await b.enrol.login('ada', PASSWORD);
    b.enrol.reset();
    expect(b.core.calls).not.toContain('enrolReset');
    await expect(b.enrol.register(INSTANCE)).rejects.toThrow('E_NO_ASSERTION');
    expect(b.server.paths()).toEqual(['POST /v1/auth/password/login']);
  });
});

// web3.6 C2: the assertion's debt (`needs`), a passkey login and a backup code.
describe('Enrol second factors', () => {
  it('a passkey-only account owes needs 2: no app code is taken for it', async () => {
    const a = account();
    a.server.addPasskey('ada', 'Laptop');
    expect(await a.enrol.login('ada', PASSWORD)).toEqual({ needs: 2 });
    await expect(a.enrol.totp('123456')).rejects.toThrow('E_NO_ASSERTION');
    expect(a.server.paths()).toEqual(['POST /v1/auth/password/login']);
  });

  it('a backup code settles an assertion owing needs 2 or needs 1, and the registration spends the new one', async () => {
    const a = account();
    a.server.addPasskey('ada', 'Laptop');
    a.server.factorsOf('ada').codes = ['A'.repeat(26), 'B'.repeat(26)];
    expect(await a.enrol.login('ada', PASSWORD)).toEqual({ needs: 2 });
    await expect(a.enrol.register(INSTANCE)).rejects.toThrow('E_NO_ASSERTION');
    expect(await a.enrol.login('ada', PASSWORD)).toEqual({ needs: 2 });
    await a.enrol.backupCode('A'.repeat(26));
    await a.enrol.register(INSTANCE);
    expect(a.server.paths().slice(0, 3)).toEqual(['POST /v1/auth/password/login', 'POST /v1/auth/password/login', 'POST /v1/auth/recovery/verify']);
    const body = arr(decode(a.server.log.find((r) => r.path === `/v1/devices/${B_HEX}/sessions`)?.body ?? new Uint8Array()), 5);
    expect(body[4]).toEqual(utf8(a.server.assertions.at(-1) ?? ''));
    expect(a.server.backupCodesOf('ada')).toEqual(['B'.repeat(26)]);

    const b = account({ totp: '123456' });
    b.server.factorsOf('ada').codes = ['C'.repeat(26)];
    expect(await b.enrol.login('ada', PASSWORD)).toEqual({ needs: 1 });
    await b.enrol.backupCode('C'.repeat(26));
    expect((await b.enrol.register(INSTANCE)).userId).toEqual(b.userId);
  });

  it('a wrong backup code spends the assertion; a backup code needs an owed assertion', async () => {
    const a = account();
    a.server.addPasskey('ada', 'Laptop');
    a.server.factorsOf('ada').codes = ['A'.repeat(26)];
    await a.enrol.login('ada', PASSWORD);
    const wrong = await httpError(a.enrol.backupCode('Z'.repeat(26)));
    expect([wrong.status, wrong.code]).toEqual([401, 'E_UNAUTHENTICATED']);
    await expect(a.enrol.backupCode('A'.repeat(26))).rejects.toThrow('E_NO_ASSERTION');
    const free = account();
    expect(await free.enrol.login('ada', PASSWORD)).toEqual({ needs: 0 });
    await expect(free.enrol.backupCode('A'.repeat(26))).rejects.toThrow('E_NO_ASSERTION');
    expect(free.server.paths()).toEqual(['POST /v1/auth/password/login']);
  });

  it('a passkey that verified the user registers at once; one that did not owes a code; a refused one keeps the held assertion', async () => {
    const a = account({ totp: '123456' });
    const cred = a.server.addPasskey('ada', 'Laptop');
    const begin = await a.routes.passkeyLoginBegin();
    expect(await a.enrol.passkey(begin.ceremonyId, `${toHex(cred)}:nouv`)).toEqual({ needs: 1 });
    await expect(a.enrol.register(INSTANCE)).rejects.toThrow('E_NO_ASSERTION');
    // An unknown passkey is refused and the code step still has its assertion.
    const unknown = await httpError(a.enrol.passkey((await a.routes.passkeyLoginBegin()).ceremonyId, 'ee'));
    expect([unknown.status, unknown.code]).toEqual([401, 'E_PASSKEY_UNKNOWN']);
    await a.enrol.totp('123456');
    expect((await a.enrol.register(INSTANCE)).userId).toEqual(a.userId);

    const b = account();
    const key = b.server.addPasskey('ada', 'Laptop');
    expect(await b.enrol.passkey((await b.routes.passkeyLoginBegin()).ceremonyId, toHex(key))).toEqual({ needs: 0 });
    expect((await b.enrol.register(INSTANCE)).userId).toEqual(b.userId);
  });
});

/** A browser of a web-1-era account: identified with its v1 accepted; the instance holds no list until a test publishes one. */
function enrolled() {
  const server = new FakeServer();
  const userId = server.register('ada', DEVICE_A);
  const core = FakeCore.identified({ instanceId: INSTANCE, userId, deviceId: DEVICE_A, username: 'ada' });
  const { routes, session } = sessionFor(server, core);
  const U = toHex(userId);
  return { server, core, routes, session, userId, U, listPath: `/v1/users/${U}/device-list` };
}

function sealed(core: FakeCore, root: Uint8Array | null, state: Uint8Array | null) {
  let uploaded = false;
  vi.spyOn(core, 'sealedObjects').mockImplementation(() => ({ root, state, stateUploaded: uploaded }));
  return vi.spyOn(core, 'stateSealedUploaded').mockImplementation(() => {
    uploaded = true;
  });
}

describe('ensureBackups', () => {
  it('uploads the root and the state when the instance holds neither, then nothing', async () => {
    const e = enrolled();
    const marked = sealed(e.core, ROOT, STATE);
    await e.session.establish();
    e.server.log.splice(0);
    await ensureBackups(e.core, e.routes);
    expect(e.server.paths()).toEqual(['GET /v1/backups', 'PUT /v1/backups/0/0', 'PUT /v1/backups/1/0']);
    expect(decode(e.server.log[1]?.body ?? new Uint8Array())).toEqual([ROOT]);
    expect(decode(e.server.log[2]?.body ?? new Uint8Array())).toEqual([STATE]);
    expect(marked).toHaveBeenCalledTimes(1);
    e.server.log.splice(0);
    await ensureBackups(e.core, e.routes);
    expect(e.server.paths()).toEqual(['GET /v1/backups']);
    expect(marked).toHaveBeenCalledTimes(1);
    expect((await e.routes.getBackup(0))?.object).toEqual(ROOT);
    expect((await e.routes.getBackup(1))?.object).toEqual(STATE);
  });

  it('detects a different stored root after a 409 and alerts before uploading state', async () => {
    const e = enrolled();
    const marked = sealed(e.core, ROOT, STATE);
    const other = encode([1, new Uint8Array(12).fill(9), new Uint8Array(86).fill(8)]);
    e.server.putBackupObject(e.userId, 0, other);
    e.server.once('PUT', '/v1/backups/0/0', { status: 409, body: ['E_INVALID_REQUEST', 'root object already stored', null] });
    await e.session.establish();
    e.server.log.splice(0);
    // A stale listing can race an upload. Force the listing to report no root.
    e.server.once('GET', '/v1/backups', { status: 200, body: [] });
    await expect(ensureBackups(e.core, e.routes)).rejects.toThrow('E_ROOT_MISMATCH');
    expect(e.server.paths()).toEqual(['GET /v1/backups', 'PUT /v1/backups/0/0', 'GET /v1/backups/0/0']);
    expect(marked).not.toHaveBeenCalled();
  });

  it('detects a planted root already shown in the listing before uploading state', async () => {
    const e = enrolled();
    const marked = sealed(e.core, ROOT, STATE);
    const other = encode([1, new Uint8Array(12).fill(9), new Uint8Array(86).fill(8)]);
    e.server.putBackupObject(e.userId, 0, other);
    await e.session.establish();
    e.server.log.splice(0);
    // A stolen enrolled session can pre-empt the owner's first root upload.
    await expect(ensureBackups(e.core, e.routes)).rejects.toThrow('E_ROOT_MISMATCH');
    expect(e.server.paths()).toEqual(['GET /v1/backups', 'GET /v1/backups/0/0']);
    expect(marked).not.toHaveBeenCalled();
  });

  it('accepts equal root bytes after a raced 409 and uploads state', async () => {
    const e = enrolled();
    const marked = sealed(e.core, ROOT, STATE);
    e.server.putBackupObject(e.userId, 0, ROOT);
    await e.session.establish();
    e.server.log.splice(0);
    e.server.once('GET', '/v1/backups', { status: 200, body: [] });
    e.server.once('PUT', '/v1/backups/0/0', { status: 409, body: ['E_INVALID_REQUEST', '', null] });
    await ensureBackups(e.core, e.routes);
    expect(e.server.paths()).toEqual(['GET /v1/backups', 'PUT /v1/backups/0/0', 'GET /v1/backups/0/0', 'PUT /v1/backups/1/0']);
    expect(marked).toHaveBeenCalledTimes(1);
  });

  // An operator's purge of the root's bytes tombstones them while the root stays served (protocol/09 Admin): the
  // identical re-PUT answers 410 E_PRUNED, which is a stored root to compare, like the 409.
  it('accepts equal root bytes after a 410 E_PRUNED on the root re-PUT and uploads state', async () => {
    const e = enrolled();
    const marked = sealed(e.core, ROOT, STATE);
    e.server.putBackupObject(e.userId, 0, ROOT);
    await e.session.establish();
    e.server.log.splice(0);
    e.server.once('GET', '/v1/backups', { status: 200, body: [] });
    e.server.once('PUT', '/v1/backups/0/0', { status: 410, body: ['E_PRUNED', 'the object was removed', null] });
    await ensureBackups(e.core, e.routes);
    expect(e.server.paths()).toEqual(['GET /v1/backups', 'PUT /v1/backups/0/0', 'GET /v1/backups/0/0', 'PUT /v1/backups/1/0']);
    expect(marked).toHaveBeenCalledTimes(1);
    expect((await e.routes.getBackup(1))?.object).toEqual(STATE);
  });

  it('detects a different stored root after a 410 E_PRUNED on the root re-PUT', async () => {
    const e = enrolled();
    const marked = sealed(e.core, ROOT, STATE);
    e.server.putBackupObject(e.userId, 0, encode([1, new Uint8Array(12).fill(9), new Uint8Array(86).fill(8)]));
    await e.session.establish();
    e.server.log.splice(0);
    e.server.once('GET', '/v1/backups', { status: 200, body: [] });
    e.server.once('PUT', '/v1/backups/0/0', { status: 410, body: ['E_PRUNED', '', null] });
    await expect(ensureBackups(e.core, e.routes)).rejects.toThrow('E_ROOT_MISMATCH');
    expect(e.server.paths()).toEqual(['GET /v1/backups', 'PUT /v1/backups/0/0', 'GET /v1/backups/0/0']);
    expect(marked).not.toHaveBeenCalled();
  });

  it('a 410 on the root PUT that is not E_PRUNED propagates', async () => {
    const e = enrolled();
    const marked = sealed(e.core, ROOT, STATE);
    await e.session.establish();
    e.server.once('PUT', '/v1/backups/0/0', { status: 410, body: ['E_GONE', '', null] });
    const err = await httpError(ensureBackups(e.core, e.routes));
    expect([err.status, err.code]).toEqual([410, 'E_GONE']);
    expect(marked).not.toHaveBeenCalled();
  });

  it('re-uploads a state absent from the listing despite the uploaded hint', async () => {
    const e = enrolled();
    e.server.putBackupObject(e.userId, 0, ROOT);
    const marked = sealed(e.core, ROOT, STATE);
    e.core.stateSealedUploaded();
    await e.session.establish();
    e.server.log.splice(0);
    await ensureBackups(e.core, e.routes);
    expect(e.server.paths()).toEqual(['GET /v1/backups', 'GET /v1/backups/0/0', 'PUT /v1/backups/1/0']);
    expect(marked).toHaveBeenCalledTimes(2);
    expect((await e.routes.getBackup(1))?.object).toEqual(STATE);
  });

  it('uploads the state again while it is not marked uploaded, though the instance holds one', async () => {
    const e = enrolled();
    e.server.putBackupObject(e.userId, 0, ROOT);
    e.server.putBackupObject(e.userId, 1, encode([1, new Uint8Array(12), new Uint8Array(8)]));
    sealed(e.core, ROOT, null);
    await e.session.establish();
    await ensureBackups(e.core, e.routes); // confirms the pre-existing root once
    const marked = sealed(e.core, ROOT, STATE);
    e.server.log.splice(0);
    await ensureBackups(e.core, e.routes);
    expect(e.server.paths()).toEqual(['GET /v1/backups', 'PUT /v1/backups/1/0']);
    expect(marked).toHaveBeenCalledTimes(1);
    expect((await e.routes.getBackup(1))?.object).toEqual(STATE);
  });

  it('a refused state upload propagates and the state stays unmarked', async () => {
    const e = enrolled();
    const marked = sealed(e.core, ROOT, STATE);
    e.server.once('PUT', '/v1/backups/1/0', { status: 507, body: ['E_STORAGE_FULL', '', null] });
    const err = await httpError(ensureBackups(e.core, e.routes));
    expect([err.status, err.code]).toEqual([507, 'E_STORAGE_FULL']);
    expect(marked).not.toHaveBeenCalled();
    expect((await e.routes.getBackup(0))?.object).toEqual(ROOT);
  });

  it('uploads nothing when the core holds no sealed objects', async () => {
    const e = enrolled();
    sealed(e.core, null, null);
    await e.session.establish();
    e.server.log.splice(0);
    await ensureBackups(e.core, e.routes);
    expect(e.server.paths()).toEqual(['GET /v1/backups']);
  });
});

// BACKUPS-RECOVERY-03: a stolen session can replace the state object with junk, which the core refuses past list v1
// (the rollback floor). A browser cannot open the instance's copy; when its own sealed state carries the list it
// accepted as the newest, a stored object with other bytes is behind that list or junk, and it uploads its own.
describe('repairBackupState', () => {
  const JUNK = encode([1, new Uint8Array(12).fill(0x0e), new Uint8Array(16).fill(0x0f)]);

  it('re-uploads this browser\'s current state over junk the instance holds', async () => {
    const e = enrolled();
    const marked = sealed(e.core, ROOT, STATE);
    e.server.putBackupObject(e.userId, 1, JUNK);
    await e.session.establish();
    e.server.log.splice(0);
    expect(await repairBackupState(e.core, e.routes)).toBe(true);
    expect(e.server.paths()).toEqual(['GET /v1/backups/1/0', 'PUT /v1/backups/1/0']);
    expect((await e.routes.getBackup(1))?.object).toEqual(STATE);
    expect(marked).toHaveBeenCalledTimes(1);
    e.server.log.splice(0);
    expect(await repairBackupState(e.core, e.routes)).toBe(false);
    expect(e.server.paths()).toEqual(['GET /v1/backups/1/0']);
  });

  it('re-uploads it when the instance holds none', async () => {
    const e = enrolled();
    sealed(e.core, ROOT, STATE);
    await e.session.establish();
    e.server.log.splice(0);
    expect(await repairBackupState(e.core, e.routes)).toBe(true);
    expect(e.server.paths()).toEqual(['GET /v1/backups/1/0', 'PUT /v1/backups/1/0']);
  });

  it('leaves another device\'s object alone when this browser\'s own state is not the newest', async () => {
    const e = enrolled();
    sealed(e.core, ROOT, STATE);
    vi.spyOn(e.core, 'stateSealedCurrent').mockReturnValue(false);
    e.server.putBackupObject(e.userId, 1, JUNK);
    await e.session.establish();
    e.server.log.splice(0);
    expect(await repairBackupState(e.core, e.routes)).toBe(false);
    expect(e.server.paths()).toEqual(['GET /v1/backups/1/0']);
    expect((await e.routes.getBackup(1))?.object).toEqual(JUNK);
  });

  it('does nothing without a sealed state of its own', async () => {
    const e = enrolled();
    sealed(e.core, ROOT, null);
    await e.session.establish();
    e.server.log.splice(0);
    expect(await repairBackupState(e.core, e.routes)).toBe(false);
    expect(e.server.paths()).toEqual([]);
  });
});

describe('refreshOwnDeviceList', () => {
  it('answers from the stored list when the instance holds no newer version', async () => {
    const e = enrolled();
    e.server.publishList(e.userId, ONLY_A);
    await e.session.establish();
    e.server.log.splice(0);
    expect(await refreshOwnDeviceList(e.core, e.routes, e.userId)).toEqual({ version: 1n, listed: true });
    expect(lines(e.server)).toEqual([`GET ${e.listPath}`]);
    expect(e.core.calls).not.toContain('ownDeviceListUpdate');
  });

  it('adopts the newer versions through the history after the stored one', async () => {
    const e = enrolled();
    e.server.publishList(e.userId, ONLY_A);
    e.server.publishList(e.userId, A_AND_B);
    e.server.publishList(e.userId, A_AND_B);
    await e.session.establish();
    e.server.log.splice(0);
    expect(await refreshOwnDeviceList(e.core, e.routes, e.userId)).toEqual({ version: 3n, listed: true });
    expect(lines(e.server)).toEqual([`GET ${e.listPath}`, `GET ${e.listPath}?after=1`]);
    expect(e.core.ownDeviceList().version).toBe(3n);
  });

  it('walks the history in pages of 64', async () => {
    const e = enrolled();
    for (let v = 1; v <= 71; v++) e.server.publishList(e.userId, ONLY_A);
    await e.session.establish();
    e.server.log.splice(0);
    expect(await refreshOwnDeviceList(e.core, e.routes, e.userId)).toEqual({ version: 71n, listed: true });
    expect(lines(e.server)).toEqual([`GET ${e.listPath}`, `GET ${e.listPath}?after=1`, `GET ${e.listPath}?after=65`]);
  });

  it('reports a device the newest list leaves out', async () => {
    const e = enrolled();
    e.server.publishList(e.userId, ONLY_A);
    await e.session.establish(); // while DEVICE_A is still listed: the gate answers enrolled
    e.server.publishList(e.userId, [{ deviceId: DEVICE_B, revokedAt: null }]);
    expect(await refreshOwnDeviceList(e.core, e.routes, e.userId)).toEqual({ version: 2n, listed: false });
    expect(lines(e.server).slice(-2)).toEqual([`GET ${e.listPath}`, `GET ${e.listPath}?after=1`]);
  });
});

// Task S1 fix round 1: a wire-4 head carries the account-kind attestation as a fifth element; the list body the core's
// enrolComplete decodes is the four-element chain row.
describe('Enrol.fetch on a wire-4 head', () => {
  it('hands the core the four-element chain row, not the raw five-element head', async () => {
    const chain = encode([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32)]);
    const raw = encode([1, new Uint8Array([9]), new Uint8Array(64), new Uint8Array(32), new Uint8Array([0x84])]);
    const routes = {
      getBackup: vi.fn((kind: number) => Promise.resolve({ object: new Uint8Array([kind + 1]) })),
      getDeviceList: vi.fn(() => Promise.resolve({ version: 1n, blob: new Uint8Array([9]), raw })),
    };
    const enrol = new Enrol({ core: new FakeCore({}), routes: routes as never, session: {} as never, now: () => 0 });
    const fetched = await enrol.fetch(new Uint8Array(16).fill(1));
    expect(fetched.listBody).toEqual(chain);
    expect(arr(decode(fetched.listBody), 4)).toHaveLength(4);
  });
});
