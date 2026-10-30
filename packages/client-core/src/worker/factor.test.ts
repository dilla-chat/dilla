// web3.6 C2: the second factor before the shell. The factor-setup phase gated in enterReady, factorSetupDone, the global
// E_FACTOR_REQUIRED hook (HttpDeps.onRefusal), and the passkey and backup-code sign-in steps.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { encodeFrame, Op } from '../gateway/frames';
import { toHex } from '../hex';
import type { AccountState, FactorsState } from '../state/types';
import { ACCEPTS } from './controller';
import type { CborInput } from '../cbor';
import { COMMUNITY, DEVICE, INSTANCE_DOC, INSTANCE_ID, LOCK, PASSWORD, RECOVERY_KEY, SUBMIT, USER, cbor, refusal, toReady, world, type World } from './world';

afterEach(() => { vi.useRealTimers(); });

const CEREMONY_HEX = 'ce'.repeat(16);
const LOGIN = { m: 'signInLogin', username: ' web ', password: PASSWORD } as const;
// web3.8 (R6): the key comes last and registers.
const KEY = { m: 'signInKey', recoveryKey: RECOVERY_KEY } as const;

async function toKeys(w: World): Promise<void> {
  w.call(1, { m: 'start' });
  await vi.waitFor(() => expect(w.account()?.phase).toBe('needs-signup'));
  w.call(2, { m: 'signupBegin' });
  await vi.waitFor(() => expect(w.account()?.phase).toBe('signup-keys'));
}

async function toLoginStep(w: World): Promise<void> {
  w.call(1, { m: 'start' });
  await vi.waitFor(() => expect(w.account()?.phase).toBe('needs-signup'));
  w.call(2, { m: 'signInBegin' });
  await vi.waitFor(() => expect(w.account()?.phase).toBe('signin-login'));
}

async function answered(w: World, id: number): Promise<unknown> {
  await vi.waitFor(() => expect(w.ret(id)).toBeDefined());
  return w.ret(id);
}

const factors = (w: World): FactorsState | undefined => w.last<FactorsState>('factors');
const plain = (v: unknown): string => JSON.stringify(v, (_k, x: unknown) => (typeof x === 'bigint' ? x.toString() : x)) ?? '';

describe('the factor-setup phase (web3.6 C2)', () => {
  it('a signup owing a factor stops at factor-setup; factorSetupDone refuses while owed, then enters ready and joins', async () => {
    const w = world({ phase: 0, owed: true });
    const negotiation = vi.fn(() => Promise.resolve({ wire: 3, e2ee: 1, media: 1, botAvailable: true }));
    (w.routes as unknown as { negotiateSession: typeof negotiation }).negotiateSession = negotiation;
    w.routes.getInvite.mockImplementation(() => Promise.resolve({ communityId: COMMUNITY, communityName: 'friends', expires: 0n }));
    await toKeys(w);
    w.call(3, SUBMIT);
    expect(await answered(w, 3)).toEqual({ t: 'ret', id: 3, ok: true, value: { communityId: null, joinError: null, factorOwed: true } });
    expect(w.account()).toMatchObject({ phase: 'factor-setup', factorSetup: { origin: 'signup' }, recoveryKey: null,
      user: { id: toHex(USER) }, deviceId: toHex(DEVICE) });
    expect(factors(w)).toMatchObject({ owed: true, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true });
    expect(w.gateway.starts).toBe(0);
    expect(w.routes.joinCommunity).not.toHaveBeenCalled();
    expect(negotiation).not.toHaveBeenCalled();
    expect(w.routes.postKeyPackages).not.toHaveBeenCalled();
    expect(w.account.ensureBackups).not.toHaveBeenCalled();

    w.call(4, { m: 'factorSetupDone', invite: null });
    expect(await answered(w, 4)).toEqual({ t: 'ret', id: 4, ok: false,
      error: { code: 'E_FACTOR_REQUIRED', detail: '', status: 0, retryAfterMs: null } });
    expect(w.account()).toMatchObject({ phase: 'factor-setup', factorSetup: { origin: 'signup' } });
    expect(w.routes.joinCommunity).not.toHaveBeenCalled();
    expect(negotiation).not.toHaveBeenCalled();

    w.state.owed = false;
    w.call(5, { m: 'factorSetupDone', invite: null });
    expect(await answered(w, 5)).toEqual({ t: 'ret', id: 5, ok: true, value: { communityId: toHex(COMMUNITY), joinError: null } });
    expect(w.account()).toMatchObject({ phase: 'ready', factorSetup: null });
    expect(factors(w)).toMatchObject({ owed: false, totp: { confirmedAt: 1_700_000_000 } });
    expect(w.routes.joinCommunity).toHaveBeenCalledWith(COMMUNITY, 'friends-code');
    expect(negotiation).toHaveBeenCalledTimes(1);
    expect(negotiation.mock.invocationCallOrder[0]).toBeLessThan(w.routes.joinCommunity.mock.invocationCallOrder[0]);
    // The KeyPackages (a V route, refused while owed) go up once the debt is paid, before the join.
    expect(w.routes.postKeyPackages).toHaveBeenCalledTimes(1);
    expect(w.routes.postKeyPackages.mock.invocationCallOrder[0]).toBeLessThan(w.routes.joinCommunity.mock.invocationCallOrder[0]);
    expect(w.gateway.starts).toBe(1);
    // A second factorSetupDone is not accepted in ready.
    w.call(6, { m: 'factorSetupDone', invite: null });
    expect(await answered(w, 6)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } });
  });

  it('a signup left at factor-setup keeps its invite across a reload: factorSetupDone joins it, then forgets it', async () => {
    // Web3.6 E3 finding: the invite lived in worker memory only, so the forced screen after a reload joined nothing.
    const w = world({ phase: 0, owed: true });
    w.routes.getInvite.mockImplementation(() => Promise.resolve({ communityId: COMMUNITY, communityName: 'friends', expires: 0n }));
    await toKeys(w);
    w.call(3, SUBMIT);
    expect(await answered(w, 3)).toMatchObject({ ok: true, value: { factorOwed: true } });
    // Kept in the account's store, never in the page's settings slice, and nothing sent while owed.
    expect(Object.values(w.state.settings).some((v) => v.includes('friends-code'))).toBe(true);
    expect(plain(w.posted)).not.toContain('friends-code');
    expect(w.routes.joinCommunity).not.toHaveBeenCalled();

    // The page closes; a new worker opens the same store and boots into the forced set-up.
    const r = world({ phase: 2, owed: true });
    r.state.settings = { ...w.state.settings };
    r.call(1, { m: 'start' });
    await vi.waitFor(() => expect(r.account()?.phase).toBe('factor-setup'));
    expect(r.account()?.factorSetup).toEqual({ origin: 'required' });
    r.call(2, { m: 'factorSetupDone', invite: null });
    expect(await answered(r, 2)).toMatchObject({ ok: false, error: { code: 'E_FACTOR_REQUIRED' } });
    expect(r.routes.joinCommunity).not.toHaveBeenCalled();
    expect(Object.values(r.state.settings).some((v) => v.includes('friends-code'))).toBe(true);
    r.state.owed = false;
    r.call(3, { m: 'factorSetupDone', invite: null });
    expect(await answered(r, 3)).toEqual({ t: 'ret', id: 3, ok: true, value: { communityId: toHex(COMMUNITY), joinError: null } });
    expect(r.routes.joinCommunity).toHaveBeenCalledWith(COMMUNITY, 'friends-code');
    expect(r.state.settings).toEqual({});
    expect(plain(r.last('settings'))).not.toContain('friends-code');
  });

  // Wave B task 3b: the page creates the account only at the key step's "I saved it". A refused creation leaves the keys
  // pending with no session; the page then discards them (signupReset), and its next Create account makes new ones. No
  // second key is ever shown for the same pending keys: signupBegin is refused while they are pending.
  it('a refused signupSubmit registers nothing; signupReset forgets the keys, and only then does signupBegin make new ones', async () => {
    const w = world({ phase: 0, owed: true });
    await toKeys(w);
    w.signup.submit.mockImplementationOnce(() => Promise.reject(refusal(409, 'E_INVALID_REQUEST')));
    w.call(3, SUBMIT);
    expect(await answered(w, 3)).toMatchObject({ ok: false, error: { code: 'E_INVALID_REQUEST', status: 409 } });
    expect(w.account()).toMatchObject({ phase: 'signup-keys', error: { code: 'E_INVALID_REQUEST' } });
    expect(w.state.phase).toBe(1);
    // A second begin over pending keys is not accepted in this phase.
    w.call(4, { m: 'signupBegin' });
    expect(await answered(w, 4)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } });
    w.call(5, { m: 'signupReset' });
    expect(await answered(w, 5)).toMatchObject({ ok: true });
    expect(w.account()).toMatchObject({ phase: 'needs-signup', recoveryKey: null });
    expect(w.state.resets).toBe(1);
    w.call(6, { m: 'signupBegin' });
    expect(await answered(w, 6)).toMatchObject({ ok: true });
    expect(w.account()?.phase).toBe('signup-keys');
    w.call(7, SUBMIT);
    expect(await answered(w, 7)).toMatchObject({ ok: true, value: { factorOwed: true } });
    expect(w.signup.submit).toHaveBeenCalledTimes(2);
    expect(w.account()).toMatchObject({ phase: 'factor-setup', factorSetup: { origin: 'signup' }, recoveryKey: null });
  });

  it('factorSetupDone without a pending join redeems the invite it is given', async () => {
    const w = world({ phase: 2, owed: true });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('factor-setup'));
    w.state.owed = false;
    w.routes.getInvite.mockImplementation(() => Promise.resolve({ communityId: COMMUNITY, communityName: 'friends', expires: 0n }));
    w.call(2, { m: 'factorSetupDone', invite: ' friends-code ' });
    expect(await answered(w, 2)).toEqual({ t: 'ret', id: 2, ok: true, value: { communityId: toHex(COMMUNITY), joinError: null } });
    expect(w.routes.getInvite).toHaveBeenCalledWith('friends-code');
    expect(w.routes.joinCommunity).toHaveBeenCalledWith(COMMUNITY, 'friends-code');
    // Not a fresh signup: the gateway's ready refills the KeyPackages from its count, enterReady does not.
    expect(w.routes.postKeyPackages).not.toHaveBeenCalled();
  });

  it('the forced set-up reads the factors once when it completes (web3.7 3g)', async () => {
    const w = world({ phase: 2, owed: true });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('factor-setup'));
    w.routes.getFactors.mockClear();
    w.state.owed = false;
    w.call(2, { m: 'factorSetupDone', invite: null });
    expect(await answered(w, 2)).toMatchObject({ ok: true });
    expect(w.account()?.phase).toBe('ready');
    expect(w.routes.getFactors).toHaveBeenCalledTimes(1);
  });

  it('the account state carries no needsTotp (web3.7 3g)', () => {
    const w = world();
    const signIn = w.account()?.signIn;
    // Compile-time pin: signIn.needsTotp is gone (web3.7 3g); this asserts nothing at runtime.
    // @ts-expect-error needsTotp was removed from the sign-in state
    void signIn?.needsTotp;
  });

  it('a reload at factor-setup boots into factor-setup with origin required, before any backup PUT or socket', async () => {
    const w = world({ phase: 2, owed: true });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('factor-setup'));
    expect(w.account()).toMatchObject({ factorSetup: { origin: 'required' }, user: { id: toHex(USER), username: 'web' } });
    expect(w.calls).toContain('publishDeviceList');
    expect(w.account.ensureBackups).not.toHaveBeenCalled();
    expect(w.routes.putBackup).not.toHaveBeenCalled();
    expect(w.account.refreshOwnDeviceList).not.toHaveBeenCalled();
    expect(w.gateway.starts).toBe(0);
    expect(w.sync.start).not.toHaveBeenCalled();
    w.call(2, { m: 'refreshDevices' });
    expect(await answered(w, 2)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } });
  });

  it('an account that owes nothing reads its factors and goes on to ready', async () => {
    const w = world();
    await toReady(w);
    expect(w.routes.getFactors).toHaveBeenCalledTimes(1);
    expect(w.account()).toMatchObject({ phase: 'ready', factorSetup: null });
    expect(factors(w)).toEqual({ owed: false, totp: { confirmedAt: 1_700_000_000 }, passkeys: [], backupCodesLeft: 10, hasPassword: true });
    expect(w.account.ensureBackups).toHaveBeenCalled();
  });

  it('factor-setup accepts factorSetupDone, forgetBrowser and the five enrol commands (C3)', () => {
    expect(ACCEPTS['factor-setup']).toEqual(['factorSetupDone', 'forgetBrowser', 'refreshFactors', 'factorTotpBegin', 'factorTotpConfirm', 'factorPasskeyBegin', 'factorPasskeyFinish', 'resign']);
    expect(ACCEPTS['signin-passkey']).toEqual(['signInPasskeyBegin', 'signInPasskey', 'signInBackupCode', 'signInCancel', 'signInBack']);
    // C2 review finding 8: the two sign-in rows the wave widened.
    expect(ACCEPTS['signin-login']).toEqual(['signInLogin', 'signInPasskeyBegin', 'signInPasskey', 'signInCancel']);
    expect(ACCEPTS['signin-totp']).toEqual(['signInTotp', 'signInPasskeyBegin', 'signInPasskey', 'signInBackupCode', 'signInCancel', 'signInBack']);
    // Wave B task 3: Back from the code and key steps; never from the login (nothing to go back to) or while enrolling.
    expect(ACCEPTS['signin-key']).toEqual(['signInKey', 'signInCancel', 'signInBack']);
    expect(ACCEPTS['signin-login']).not.toContain('signInBack');
    expect(ACCEPTS['enrolling']).not.toContain('signInBack');
  });
});

/** A WebSocket double: the test opens it, feeds it frames, and closes it from the server side. */
class Sock {
  static all: Sock[] = [];
  readyState = 0;
  binaryType = 'blob';
  private readonly listeners = new Map<string, ((event: unknown) => void)[]>();
  constructor(readonly url: string, readonly protocols: string[]) { Sock.all.push(this); }
  addEventListener(type: string, fn: (event: unknown) => void): void { this.listeners.set(type, [...(this.listeners.get(type) ?? []), fn]); }
  removeEventListener(): void {}
  readonly sent: Uint8Array[] = [];
  send(data: Uint8Array): void { this.sent.push(new Uint8Array(data)); }
  close(code?: number): void {
    if (this.readyState === 3) return;
    this.readyState = 3;
    this.emit('close', { code: code ?? 1005, reason: '', wasClean: true });
  }
  open(): void { this.readyState = 1; this.emit('open', {}); }
  receive(bytes: Uint8Array): void { this.emit('message', { data: bytes.slice().buffer }); }
  serverClose(code: number): void { this.readyState = 3; this.emit('close', { code, reason: '', wasClean: true }); }
  private emit(type: string, event: unknown): void { for (const fn of this.listeners.get(type) ?? []) fn(event); }
}

const HELLO = encodeFrame(Op.hello, 0, null, [[2], [1], [1], 30_000, 131_584, INSTANCE_ID, 1, 300, 300]);
const READY = encodeFrame(Op.ready, 1, null, [DEVICE, USER, 7, new Uint8Array(32).fill(0x0b), 2, 1, 1, 32, []]);

/** A world on the real Gateway, whose tickets go through the real HttpClient to `ticket` (the n-th request, 1-based). */
function socketWorld(ticket: (n: number) => Promise<Response>) {
  Sock.all = [];
  const tickets = { n: 0 };
  const fetch = ((input: RequestInfo | URL) => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    if (path !== '/v1/gateway/ticket') return Promise.resolve(cbor(['E_NOT_FOUND', 'not modelled', null], 404));
    tickets.n += 1;
    return ticket(tickets.n);
  }) as typeof globalThis.fetch;
  const w = world({ realGateway: true, httpTicket: true, fetch, WebSocket: Sock as unknown as typeof WebSocket });
  return { w, tickets };
}

const minted = (): Promise<Response> => Promise.resolve(cbor([`tkt-${String(Date.now())}`, 1_700_000_030], 201));
const owedTicket = (): Promise<Response> => Promise.resolve(cbor(['E_FACTOR_REQUIRED', '', null], 403));

async function until(check: () => boolean): Promise<void> {
  for (let i = 0; i < 200 && !check(); i++) await vi.advanceTimersByTimeAsync(10);
  expect(check()).toBe(true);
}

describe('the global E_FACTOR_REQUIRED hook (web3.6 C2)', () => {
  it('a ready worker whose ticket is refused E_FACTOR_REQUIRED stops the gateway and enters factor-setup; no ticket follows', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    let answer: (r: Response) => void = () => {};
    const { w, tickets } = socketWorld(() => new Promise<Response>((resolve) => { answer = resolve; }));
    w.call(1, { m: 'start' });
    await until(() => w.account()?.phase === 'ready');
    expect(tickets.n).toBe(1);
    w.state.owed = true;
    answer(await owedTicket());
    await until(() => w.account()?.phase === 'factor-setup');
    expect(w.account()?.factorSetup).toEqual({ origin: 'required' });
    expect(w.connection()?.status).toBe('offline');
    expect(w.sync.stop).toHaveBeenCalled();
    expect(factors(w)?.owed).toBe(true);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(tickets.n).toBe(1);
    expect(Sock.all).toHaveLength(0);
  });

  it('an operator reset never shows factor-setup over the old factors: the slice says owed, with nothing held, first', async () => {
    // Web3.6 E3 finding: the page took the ready account's TOTP for a factor already in place and skipped the choice.
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    let answer: (r: Response) => void = () => {};
    const { w } = socketWorld(() => new Promise<Response>((resolve) => { answer = resolve; }));
    w.call(1, { m: 'start' });
    await until(() => w.account()?.phase === 'ready');
    expect(factors(w)).toMatchObject({ owed: false, totp: { confirmedAt: 1_700_000_000 } });
    // The fresh read is held back, so what the page sees with the new phase is what the worker published before it.
    let release: () => void = () => {};
    w.routes.getFactors.mockImplementationOnce(() => new Promise((resolve) => {
      release = () => { resolve({ owed: true, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true }); };
    }));
    w.state.owed = true;
    answer(await owedTicket());
    await until(() => w.account()?.phase === 'factor-setup');
    const first = w.posted.findIndex((m) => m.t === 'slice' && m.name === 'account' && (m.value as AccountState).phase === 'factor-setup');
    const before = w.posted.slice(0, first).filter((m) => m.t === 'slice' && m.name === 'factors').at(-1);
    expect(before?.t === 'slice' ? before.value : undefined).toEqual({ owed: true, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true });
    release();
    await until(() => w.routes.getFactors.mock.calls.length >= 2);
    await vi.advanceTimersByTimeAsync(10);
    expect(factors(w)).toEqual({ owed: true, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true });
  });

  it('the reset chain: a socket closed 4003 reconnects once, the ticket answers E_FACTOR_REQUIRED, and the hook fires', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const { w, tickets } = socketWorld((n) => (n === 2 ? owedTicket() : minted()));
    w.call(1, { m: 'start' });
    await until(() => w.account()?.phase === 'ready' && Sock.all.length === 1);
    const socket = Sock.all[0];
    socket.open();
    socket.receive(HELLO);
    socket.receive(READY);
    await until(() => w.connection()?.status === 'online');
    // The operator resets the account's factors (S3): the instance marks it owed and closes its sockets with 4003.
    w.state.owed = true;
    socket.serverClose(4003);
    await until(() => w.account()?.phase === 'factor-setup');
    expect(tickets.n).toBe(2);
    expect(w.account()?.factorSetup).toEqual({ origin: 'required' });
    await vi.advanceTimersByTimeAsync(10_000);
    expect(tickets.n).toBe(2);
    expect(Sock.all).toHaveLength(1);
    // Paying the debt builds a fresh socket and engine.
    w.state.owed = false;
    w.call(2, { m: 'factorSetupDone', invite: null });
    await until(() => w.ret(2) !== undefined);
    expect(w.ret(2)).toMatchObject({ ok: true, value: { communityId: null, joinError: null } });
    expect(w.account()?.phase).toBe('ready');
    expect(tickets.n).toBe(3);
  });
});

// web3.8 (R2): an instance whose hello offers only wire 1 is one this client cannot speak to. The real gateway closes
// 4006 without an identify and the worker publishes reason 'version' (the reload banner), with no reconnect.
describe('wire version 2 (web3.8 C2)', () => {
  it('a hello offering only wire 1 leaves the connection offline with reason version and sends nothing', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const { w, tickets } = socketWorld(minted);
    w.call(1, { m: 'start' });
    await until(() => w.account()?.phase === 'ready' && Sock.all.length === 1);
    const socket = Sock.all[0];
    socket.open();
    socket.receive(encodeFrame(Op.hello, 0, null, [[1], [1], [1], 30_000, 131_584, INSTANCE_ID, 1, 300, 300]));
    await until(() => w.connection()?.reason === 'version');
    expect(w.connection()).toMatchObject({ status: 'offline', reason: 'version' });
    expect(socket.sent).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(600_000);
    expect(Sock.all).toHaveLength(1);
    expect(tickets.n).toBe(1);
  });

  // C2 review finding 1: a tab open across the upgrade meets the instance's own refusal, close 4006 after identify.
  it('a server close 4006 publishes offline with reason version and mints no second ticket', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const { w, tickets } = socketWorld(minted);
    w.call(1, { m: 'start' });
    await until(() => w.account()?.phase === 'ready' && Sock.all.length === 1);
    const socket = Sock.all[0];
    socket.open();
    socket.receive(HELLO);
    socket.serverClose(4006);
    await until(() => w.connection()?.reason === 'version');
    expect(w.connection()).toMatchObject({ status: 'offline', reason: 'version' });
    await vi.advanceTimersByTimeAsync(600_000);
    expect(Sock.all).toHaveLength(1);
    expect(tickets.n).toBe(1);
  });

  it('control: a hello offering wire 2 gets an identify with 2 and goes online', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const { w } = socketWorld(minted);
    w.call(1, { m: 'start' });
    await until(() => w.account()?.phase === 'ready' && Sock.all.length === 1);
    const socket = Sock.all[0];
    socket.open();
    socket.receive(HELLO);
    expect(socket.sent).toEqual([encodeFrame(Op.identify, 1, null, ['', 2, 1, 1, 0])]);
    socket.receive(READY);
    await until(() => w.connection()?.status === 'online');
    expect(w.connection()?.reason).toBeUndefined();
  });
});

describe('passkey and backup-code sign-in (web3.6 C2)', () => {
  it('a login owing needs 2 goes to signin-passkey; a passkey that did not verify the user goes on to the code step', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => Promise.resolve({ needs: 2 }));
    await toLoginStep(w);
    w.call(3, LOGIN);
    expect(await answered(w, 3)).toEqual({ t: 'ret', id: 3, ok: true, value: { needs: 2 } });
    expect(w.account()).toMatchObject({ phase: 'signin-passkey', signIn: { username: 'web', factor: 'passkey' }, error: null });
    expect(w.enrol.register).not.toHaveBeenCalled();
    w.call(4, { m: 'signInPasskeyBegin' });
    expect(await answered(w, 4)).toEqual({ t: 'ret', id: 4, ok: true, value: { ceremonyId: CEREMONY_HEX, options: '{"publicKey":{}}' } });
    w.enrol.passkey.mockImplementationOnce(() => Promise.resolve({ needs: 1 }));
    w.call(5, { m: 'signInPasskey', ceremonyId: CEREMONY_HEX, response: '{"id":"x"}' });
    expect(await answered(w, 5)).toEqual({ t: 'ret', id: 5, ok: true, value: { needs: 1 } });
    expect(w.enrol.passkey).toHaveBeenCalledWith(new Uint8Array(16).fill(0xce), '{"id":"x"}');
    expect(w.account()).toMatchObject({ phase: 'signin-totp', signIn: { factor: 'passkey' } });
    w.call(6, { m: 'signInTotp', code: '123456' });
    expect(await answered(w, 6)).toEqual({ t: 'ret', id: 6, ok: true, value: null });
    expect(w.account()?.phase).toBe('signin-key');
    expect(w.enrol.register).not.toHaveBeenCalled();
    w.call(7, KEY);
    expect(await answered(w, 7)).toEqual({ t: 'ret', id: 7, ok: true, value: null });
    expect(w.account()?.phase).toBe('ready');
  });

  it('a login owing needs 1 goes to signin-totp with factor totp', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => Promise.resolve({ needs: 1 }));
    await toLoginStep(w);
    w.call(3, LOGIN);
    expect(await answered(w, 3)).toEqual({ t: 'ret', id: 3, ok: true, value: { needs: 1 } });
    expect(w.account()).toMatchObject({ phase: 'signin-totp', signIn: { username: 'web', factor: 'totp' } });
  });

  it('wave B task 3 fix round 1: "Use a passkey" from signin-totp after a password login owing the code (needs 1)', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => Promise.resolve({ needs: 1, passkey: true }));
    await toLoginStep(w);
    w.call(3, LOGIN);
    await answered(w, 3);
    expect(w.account()).toMatchObject({ phase: 'signin-totp', signIn: { factor: 'totp', passkey: true } });
    w.call(4, { m: 'signInPasskeyBegin' });
    await answered(w, 4);
    w.call(5, { m: 'signInPasskey', ceremonyId: CEREMONY_HEX, response: '{"id":"x"}' });
    expect(await answered(w, 5)).toEqual({ t: 'ret', id: 5, ok: true, value: { needs: 0 } });
    // The passkey's own assertion replaces the password's: no code is asked, and the key step follows.
    expect(w.enrol.totp).not.toHaveBeenCalled();
    expect(w.account()?.phase).toBe('signin-key');
    w.call(6, KEY);
    expect(await answered(w, 6)).toEqual({ t: 'ret', id: 6, ok: true, value: null });
    expect(w.account()?.phase).toBe('ready');
  });

  it('a passkey on its own signs in from the login step', async () => {
    const w = world({ phase: 0 });
    await toLoginStep(w);
    w.call(3, { m: 'signInPasskeyBegin' });
    await answered(w, 3);
    w.call(4, { m: 'signInPasskey', ceremonyId: CEREMONY_HEX, response: '{"id":"x"}' });
    expect(await answered(w, 4)).toEqual({ t: 'ret', id: 4, ok: true, value: { needs: 0 } });
    expect(w.enrol.login).not.toHaveBeenCalled();
    expect(w.account()?.phase).toBe('signin-key');
    w.call(5, KEY);
    expect(await answered(w, 5)).toEqual({ t: 'ret', id: 5, ok: true, value: null });
    expect(w.calls.filter((c) => c.startsWith('enrol.'))).toEqual(['enrol.passkey', 'enrol.register', 'enrol.fetch', 'enrol.complete']);
    expect(w.account()?.phase).toBe('ready');
  });

  it('a refused passkey stays at its step with the error', async () => {
    const w = world({ phase: 0 });
    await toLoginStep(w);
    w.enrol.passkey.mockImplementationOnce(() => Promise.reject(refusal(401, 'E_PASSKEY_UNKNOWN')));
    w.call(3, { m: 'signInPasskey', ceremonyId: CEREMONY_HEX, response: 'r' });
    expect(await answered(w, 3)).toMatchObject({ ok: false, error: { code: 'E_PASSKEY_UNKNOWN', status: 401 } });
    expect(w.account()).toMatchObject({ phase: 'signin-login', error: { code: 'E_PASSKEY_UNKNOWN' } });
  });

  it('wave B task 3 (M2): another account\'s passkey on the code step is refused before this browser is enrolled', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => Promise.resolve({ needs: 1, passkey: true }));
    await toLoginStep(w);
    w.call(3, LOGIN);
    await answered(w, 3);
    w.enrol.passkey.mockImplementationOnce(() => Promise.reject(refusal(403, 'E_PASSKEY_OTHER_ACCOUNT')));
    w.call(4, { m: 'signInPasskey', ceremonyId: CEREMONY_HEX, response: 'r' });
    expect(await answered(w, 4)).toMatchObject({ ok: false, error: { code: 'E_PASSKEY_OTHER_ACCOUNT', status: 403 } });
    // Fail closed: the code step stays, nothing was registered, and the result was dropped.
    expect(w.account()).toMatchObject({ phase: 'signin-totp', error: { code: 'E_PASSKEY_OTHER_ACCOUNT' } });
    expect(w.enrol.register).not.toHaveBeenCalled();
    w.call(5, KEY);
    expect(await answered(w, 5)).toMatchObject({ ok: false });
    expect(w.enrol.register).not.toHaveBeenCalled();
  });

  it('a backup code after needs 2 signs in; it is sent normalised', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => Promise.resolve({ needs: 2 }));
    await toLoginStep(w);
    w.call(3, LOGIN);
    await answered(w, 3);
    w.call(4, { m: 'signInBackupCode', code: 'abcde-fghij klmno-pqrst uvwxyz', recoveryKey: RECOVERY_KEY });
    expect(await answered(w, 4)).toEqual({ t: 'ret', id: 4, ok: true, value: null });
    expect(w.enrol.backupCode).toHaveBeenCalledWith('ABCDEFGHIJKLMNOPQRSTUVWXYZ');
    expect(w.calls.filter((c) => c.startsWith('enrol.'))).toEqual(['enrol.backupCode', 'enrol.register', 'enrol.fetch', 'enrol.complete']);
    expect(w.account()?.phase).toBe('ready');
  });

  it('a wrong backup code spends the assertion: back to the login step with the error and the username kept', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => Promise.resolve({ needs: 2 }));
    w.enrol.backupCode.mockImplementationOnce(() => Promise.reject(refusal(401, 'E_UNAUTHENTICATED')));
    await toLoginStep(w);
    w.call(3, LOGIN);
    await answered(w, 3);
    w.call(4, { m: 'signInBackupCode', code: 'A'.repeat(26), recoveryKey: RECOVERY_KEY });
    expect(await answered(w, 4)).toMatchObject({ ok: false, error: { code: 'E_UNAUTHENTICATED', status: 401 } });
    expect(w.account()).toMatchObject({ phase: 'signin-login', signIn: { username: 'web', factor: null },
      error: { code: 'E_UNAUTHENTICATED' } });
    expect(w.enrol.register).not.toHaveBeenCalled();
  });

  it('checks the new commands\' input before anything is sent', async () => {
    const w = world({ phase: 0 });
    w.enrol.login.mockImplementationOnce(() => Promise.resolve({ needs: 2 }));
    await toLoginStep(w);
    w.call(3, LOGIN);
    await answered(w, 3);
    const bad = [
      { m: 'signInBackupCode', code: 'A'.repeat(25), recoveryKey: RECOVERY_KEY },
      { m: 'signInBackupCode', code: 'A'.repeat(25) + '1', recoveryKey: RECOVERY_KEY },
      { m: 'signInBackupCode', code: 'A'.repeat(26), recoveryKey: '' },
      { m: 'signInPasskey', ceremonyId: 'CE'.repeat(16), response: 'r' },
      { m: 'signInPasskey', ceremonyId: CEREMONY_HEX, response: '' },
      { m: 'signInPasskey', ceremonyId: CEREMONY_HEX, response: 'x'.repeat(65_537) },
      { m: 'factorSetupDone', invite: 7 },
    ];
    bad.forEach((command, i) => w.call(10 + i, command));
    for (let i = 0; i < bad.length; i++) expect(await answered(w, 10 + i)).toMatchObject({ ok: false, error: { code: 'E_BAD_INPUT' } });
    expect(w.enrol.backupCode).not.toHaveBeenCalled();
    expect(w.enrol.passkey).not.toHaveBeenCalled();
    // Control: 64 KiB of response is accepted.
    w.call(30, { m: 'signInPasskey', ceremonyId: CEREMONY_HEX, response: 'x'.repeat(65_536) });
    expect(await answered(w, 30)).toMatchObject({ ok: true });
  });

  it('the account slice says which factors the instance offers', async () => {
    const w = world();
    w.routes.getInstance.mockImplementation(() => Promise.resolve({ instanceId: INSTANCE_ID, generation: 7n, name: 'dilla.test',
      registrationMode: 0, authMethods: [0, 2], policyVersion: 1n, externalSenderPub: new Uint8Array(32).fill(2) }));
    await toReady(w);
    expect(w.account()?.instance).toMatchObject({ passwordSignup: true, passkeys: true, totp: false } satisfies Partial<NonNullable<AccountState['instance']>>);
  });
});

describe('walk M9: "Use this tab" while this tab is on the forced screen', () => {
  it('a worker at factor-setup resigns: it releases the store and the lock, says released, and waits as other-tab', async () => {
    const w = world({ phase: 2, owed: true });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('factor-setup'));
    // The tab that asked: it waits for the lock this worker holds.
    let releaseB: () => void = () => {};
    const grantedB = new Promise<void>((resolve) => {
      void w.locks.request(LOCK, {}, () => { resolve(); return new Promise<void>((done) => { releaseB = done; }); });
    });
    w.call(2, { m: 'resign', nonce: 'aa'.repeat(16) });
    expect(await answered(w, 2)).toEqual({ t: 'ret', id: 2, ok: true, value: { released: true } });
    expect(w.posted.some((m) => m.t === 'released')).toBe(true);
    expect(w.core.pause).toHaveBeenCalled();
    await grantedB;
    expect(w.account()?.phase).toBe('other-tab');
    // Once the other tab lets go, this one leads again and is back at the forced set-up.
    releaseB();
    await vi.waitFor(() => expect(w.account()?.phase).toBe('factor-setup'));
  });
});

// C2 review fix round 1.
describe('final review M5: an owed state met in the middle of enterReady', () => {
  it('a reset landing after the gate read routes to factor-setup (origin required) and stops the gateway, not the error screen', async () => {
    const w = world({ phase: 2 });
    w.routes.listCommunities.mockImplementationOnce(() => { w.state.owed = true; return Promise.reject(refusal(403, 'E_FACTOR_REQUIRED')); });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).not.toBe('loading'));
    await vi.waitFor(() => expect(w.gateway.stops).toBeGreaterThan(0));
    expect(w.account()).toMatchObject({ phase: 'factor-setup', factorSetup: { origin: 'required' }, error: null });
    expect(w.sync.stop).toHaveBeenCalled();
    expect(factors(w)?.owed).toBe(true);
  });
});

describe('C2 review: an owed account on a new browser, the double submit, the hook\'s filter', () => {
  it('an owed account signing in on a new browser (needs 0) whose state upload is refused stops at factor-setup with origin signin, then reaches ready', async () => {
    // D5: an operator reset keeps the password and the recovery key. The login owes nothing, the enrolment writes the core
    // (phase 2), and the state PUT inside Enrol.complete answers 403 E_FACTOR_REQUIRED (backups are not an F route).
    const w = world({ phase: 0, owed: true });
    w.enrol.complete.mockImplementationOnce(() => { w.state.phase = 2; return Promise.reject(refusal(403, 'E_FACTOR_REQUIRED')); });
    await toLoginStep(w);
    w.call(3, LOGIN);
    expect(await answered(w, 3)).toEqual({ t: 'ret', id: 3, ok: true, value: { needs: 0 } });
    expect(w.account()?.phase).toBe('signin-key');
    w.call(5, KEY);
    expect(await answered(w, 5)).toEqual({ t: 'ret', id: 5, ok: true, value: null });
    expect(w.account()).toMatchObject({ phase: 'factor-setup', factorSetup: { origin: 'signin' }, error: null, user: { id: toHex(USER) } });
    expect(w.gateway.starts).toBe(0);
    expect(w.account.ensureBackups).not.toHaveBeenCalled();
    w.state.owed = false;
    w.call(4, { m: 'factorSetupDone', invite: null });
    expect(await answered(w, 4)).toEqual({ t: 'ret', id: 4, ok: true, value: { communityId: null, joinError: null } });
    expect(w.account()).toMatchObject({ phase: 'ready', factorSetup: null });
    // (e) uploads what the refused PUT could not.
    expect(w.account.ensureBackups).toHaveBeenCalledTimes(1);
    expect(w.gateway.starts).toBe(1);
  });

  it('a second factorSetupDone while one runs is refused E_NOT_READY; the first reaches ready once', async () => {
    const w = world({ phase: 2, owed: true });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('factor-setup'));
    w.state.owed = false;
    let release: () => void = () => {};
    const held = new Promise<void>((resolve) => { release = resolve; });
    w.routes.getFactors.mockImplementationOnce(async () => {
      await held;
      return { owed: false, totp: 1_700_000_000, passkeys: [], backupCodesLeft: 10, hasPassword: true };
    });
    w.call(2, { m: 'factorSetupDone', invite: null });
    w.call(3, { m: 'factorSetupDone', invite: null });
    expect(await answered(w, 3)).toMatchObject({ ok: false, error: { code: 'E_NOT_READY' } });
    release();
    expect(await answered(w, 2)).toMatchObject({ ok: true, value: { communityId: null, joinError: null } });
    expect(w.account()?.phase).toBe('ready');
    expect(w.account.ensureBackups).toHaveBeenCalledTimes(1);
    expect(w.gateway.starts).toBe(1);
  });

  /** A world whose postTicket goes through the controller's HttpClient, answered 403 E_FACTOR_REQUIRED. */
  function refusingWorld(opts: { phase?: 0 | 2; owed?: boolean } = {}) {
    const fetch = (() => Promise.resolve(cbor(['E_FACTOR_REQUIRED', '', null], 403))) as typeof globalThis.fetch;
    return world({ ...opts, httpTicket: true, fetch });
  }
  const factorSetups = (w: World): number => w.posted.filter((m) => m.t === 'slice' && m.name === 'account'
    && (m.value as AccountState).phase === 'factor-setup').length;

  it('the hook runs once per transition: refusals that follow the first do not run it again', async () => {
    const w = refusingWorld();
    await toReady(w);
    const reads = w.routes.getFactors.mock.calls.length;
    await w.routes.postTicket().catch(() => undefined);
    await w.routes.postTicket().catch(() => undefined);
    await vi.waitFor(() => expect(w.routes.getFactors.mock.calls.length).toBe(reads + 1));
    expect(w.account()?.factorSetup).toEqual({ origin: 'required' });
    expect(factorSetups(w)).toBe(1);
    expect(w.sync.stop).toHaveBeenCalledTimes(1);
  });

  it('a refusal outside ready leaves the phase and its origin alone (a signup at factor-setup stays origin signup)', async () => {
    const w = refusingWorld({ phase: 0, owed: true });
    await toKeys(w);
    w.call(3, SUBMIT);
    await answered(w, 3);
    expect(w.account()?.factorSetup).toEqual({ origin: 'signup' });
    const published = factorSetups(w);
    await w.routes.postTicket().catch(() => undefined);
    expect(w.account()).toMatchObject({ phase: 'factor-setup', factorSetup: { origin: 'signup' } });
    expect(factorSetups(w)).toBe(published);
  });
});

// S2 review M5: under instance.discovery = "session", GET /v1/instance is E, F; an owed client reads auth_methods with its
// session. The start's read precedes any session (it names the store), so it carries no bearer.
// Ready-phase pin continuity adds a separate authenticated discovery read.
describe('the instance read with a session (S2 review M5)', () => {
  it('an owed client re-reads the instance with its bearer at factor-setup and publishes the methods it answers', async () => {
    const seen: (string | null)[] = [];
    const withSession = [...(INSTANCE_DOC as CborInput[])];
    withSession[7] = [0, 1, 2];
    const fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      const bearer = request.headers.get('authorization');
      seen.push(bearer);
      // A session-discovery instance answers the session; the anonymous answer stands in for the start's read.
      return Promise.resolve(cbor(bearer === null ? INSTANCE_DOC : withSession));
    }) as typeof globalThis.fetch;
    const w = world({ phase: 2, owed: true, httpInstance: true, fetch });
    w.call(1, { m: 'start' });
    await vi.waitFor(() => expect(w.account()?.phase).toBe('factor-setup'));
    await vi.waitFor(() => expect(w.account()?.instance).toMatchObject({ totp: true, passkeys: true }));
    expect(seen).toEqual([null, 'Bearer session-token', 'Bearer session-token']);
  });

  it('an account that owes nothing reads the instance only to check its signing-key pin', async () => {
    const seen: (string | null)[] = [];
    const fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
      seen.push(new Request(input, init).headers.get('authorization'));
      return Promise.resolve(cbor(INSTANCE_DOC));
    }) as typeof globalThis.fetch;
    const w = world({ httpInstance: true, fetch });
    await toReady(w);
    expect(seen).toEqual([null, 'Bearer session-token']);
    expect(w.account()?.instance).toMatchObject({ totp: false, passkeys: false });
  });
});
