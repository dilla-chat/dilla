import { createHash } from 'node:crypto';
import { arr, bin, decode, encode, str, u64, type CborInput, type CborValue } from '../cbor';
import { Session } from '../account/session';
import type { CorePort } from '../core-port';
import { fromHex, toHex } from '../hex';
import { HttpClient } from '../http/client';
import type { SocketTransport } from '../gateway/requests';
import { Routes } from '../http/routes';
import { fakeDskPub, fakeListBlob, fakeListNames, readFakeList } from './fake-list';

/** The fixed clock of every account test, in unix seconds. */
export const NOW_S = 1_800_000_000;
export const SESSION_S = 604_800;
export const IDLE_S = 43_200;
/** dillad's AssertionTTL (internal/api/auth.go): an assertion spent after it is refused 401, as an unknown one is. */
export const ASSERTION_TTL_S = 300;
/** The nonce of the first challenge; challenge k (1-based) answers nonceAt(k), so every challenge is fresh. */
export const NONCE = new Uint8Array(32).fill(0x6e);
export function nonceAt(k: number): Uint8Array {
  const nonce = NONCE.slice();
  nonce[0] = 0x6e + k - 1;
  return nonce;
}

/** The fake's stand-in for Ed25519(DSK, SessionPreimage(instance, device_id, nonce, purpose 0)): only the holder of
 *  `dskPub` is taken to produce it, so a body signed for another key or nonce fails as a real signature would. */
export function fakeDeviceProof(dskPub: Uint8Array, deviceId: Uint8Array, nonce: Uint8Array): Uint8Array {
  return new Uint8Array(createHash('sha512').update('fake.proof').update(dskPub).update(deviceId).update(nonce).update(new Uint8Array([0])).digest());
}

export interface LoggedRequest { method: string; path: string; query: string; auth: string | null; body: Uint8Array; contentType: string | null }
export type Reply = { status: number; body?: CborInput } | 'network' | 'lose';
interface Account { userId: Uint8Array; username: string; display: string }

// ---- web3.6: second factors (flow 10) ----
/** The code the fake accepts for a TOTP enrolment, a TOTP login and a TOTP step-up. */
export const FAKE_TOTP_CODE = '123456';
/** The setup key the fake's enrol answers. */
export const FAKE_TOTP_SECRET = 'JBSWY3DPEHPK3PXP';
export const FAKE_STEP_UP_EXPIRES_S = 300;
/** dillad's lockout (auth.lockout defaults): four free failures, the fifth is itself 429, and a failure counts for the
 *  observation window of 15 minutes. The fake holds a lockout until the failures age out of the window, which is stricter
 *  than dillad's growing lockouts (one attempt passes each time a lockout runs out). It has one client address, so dillad's
 *  per-address budgets (the login_failed bucket the step-up gate peeks) are not modelled. */
const FREE_ATTEMPTS = 4;
const OBSERVATION_WINDOW_S = 900;
/** The passkeys one account may hold (dillad MaxPasskeys, the client's PASSKEYS_MAX). */
const MAX_PASSKEYS = 1024;
const BACKUP_CODES = 10;
const BASE32 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
/** What one account holds: an app (totpCode), passkeys, unused backup codes, and the owed flag. */
export interface FakeFactorState {
  owed: boolean;
  totpCode: string | null;
  /** How many app codes were accepted: each is burnt, as dillad's last_counter is, so the app's next code is another. */
  totpUsed: number;
  /** An enrolment not yet confirmed, and whether its enroll spent a step-up (dillad keeps the same record per user). */
  totpEnrol: { steppedUp: boolean } | null;
  passkeys: { credId: Uint8Array; name: string; created: number; lastUsed: number | null }[];
  codes: string[];
}
/** Routes an owed account may still call (protocol/09 scope F, and the P routes that pass by their scope). */
const OWED_OK = [
  /^GET \/v1\/accounts\/me$/, /^GET \/v1\/auth\/factors$/, /^POST \/v1\/auth\/totp\/(enroll|confirm)$/,
  /^POST \/v1\/auth\/passkey\/register\/(begin|finish)$/, /^DELETE \/v1\/devices\/[0-9a-f]{32}\/sessions$/,
  /^(PUT|GET) \/v1\/users\/[0-9a-f]{32}\/device-list$/, /^GET \/v1\/backups(\/[01]\/0)?$/,
];

/** The "E or V" routes a provisional session reaches (keypackages.go: orProvisional). */
const E_OR_V = [/^POST \/v1\/keypackages$/, /^(GET|DELETE) \/v1\/welcomes(\/[0-9]+)?$/];
/** dillad's passkey name rule (validPasskeyName): 1-64 bytes of UTF-8 without control characters. */
const validPasskeyName = (name: CborValue): boolean => typeof name === 'string' && name.length > 0
  && new TextEncoder().encode(name).length <= 64 && !/\p{Cc}/u.test(name);

function reply(status: number, body?: CborInput): Response {
  if (body === undefined) return new Response(null, { status, headers: { 'X-Dilla-Generation': '1' } });
  return new Response(new Uint8Array(encode(body)), {
    status,
    headers: { 'Content-Type': 'application/cbor', 'X-Dilla-Generation': '1' },
  });
}

function refuse(status: number, code: string, detail = '', retryAfterMs: number | null = null): Response {
  return reply(status, [code, detail, retryAfterMs]);
}
const same = (a: Uint8Array, b: Uint8Array): boolean => a.length === b.length && a.every((x, i) => x === b[i]);

/** An in-memory model of the account routes of dillad, answering through fetch. */
/** dillad's default auth.session.reauth_window. */
const REAUTH_WINDOW_S = 300;

export class FakeServer {
  readonly instanceId = new Uint8Array(16).fill(0xab);
  readonly externalSenderPub = fromHex('db995fe25169d141cab9bbba92baa01f9f2e1ece7df4cb2ac05190f37fcc1f9d');
  readonly log: LoggedRequest[] = [];
  readonly invites = new Set<string>(['INVITE']);
  readonly accounts = new Map<string, Account>();
  readonly devices = new Map<string, string>();
  readonly tokens = new Map<string, string>();
  /** When each session token was minted: a first password wants one younger than REAUTH_WINDOW_S (dillad auth.session.reauth_window). */
  private readonly tokenCreated = new Map<string, number>();
  readonly deviceLists = new Map<string, { version: bigint; blob: Uint8Array }>();
  readonly keyPackages: { device: string; count: number; lastResort: boolean }[] = [];
  scope: 0 | 1 | null = null;
  readonly passwords = new Map<string, { password: string; totp: string | null }>();
  /** Every assertion issued, including ones subsequently spent. */
  readonly assertions: string[] = [];
  private readonly activeAssertions = new Map<string, { user: string; needs: 0 | 1 | 2; expires: number; firstFactor?: true }>();
  /** When true, an account created through POST /v1/accounts is owed a second factor, as dillad does. */
  factorRequired = false;
  private readonly factorStates = new Map<string, FakeFactorState>();
  /** Step-up tokens: token → the username and the session token they were issued to. Single use. */
  private readonly stepUps = new Map<string, { username: string; session: string }>();
  /** Step-up failures of this (the one) client address per account: a success clears them. */
  private readonly stepUpFailures = new Map<string, number>();
  /** dillad's account-wide step-up ledger: failure times from every address; a success never clears it. */
  private readonly stepUpAccount = new Map<string, number[]>();
  /** dillad's account login ledger (the lockout): failure times of password logins and second-factor codes. */
  private readonly loginFailures = new Map<string, number[]>();
  /** Open WebAuthn ceremonies: the id (hex) → what it is for and for whom. */
  private readonly ceremonies = new Map<string, { kind: 'register' | 'login' | 'stepup'; username: string | null; steppedUp: boolean }>();
  private stepUpN = 0;
  private ceremonyN = 0;
  private credN = 0;
  private codeN = 0;
  readonly backups = new Map<string, { object: Uint8Array; created: bigint }>();
  readonly listHistory = new Map<string, { version: bigint; blob: Uint8Array; sig: Uint8Array; prev: Uint8Array }[]>();
  readonly revoked = new Map<string, number>();
  readonly createdAt = new Map<string, number>();
  /** Device labels (PATCH /v1/devices/{id}): device hex → the owner's name for it; absent is unnamed. */
  readonly labels = new Map<string, string>();
  readonly tiers = new Map<string, 0 | 1>();
  /** token → protocol/02 item 4's scope: 0 enrolled, 1 pending, 2 provisional (the fake mints only 0 and 1; a test
   *  plants a provisional token directly). */
  readonly tokenScope = new Map<string, 0 | 1 | 2>();
  /** device hex → the hex of the dsk_pub its row holds; a row planted without one holds fakeDskPub(device). */
  readonly keys = new Map<string, string>();
  /** Challenge nonces not yet spent by POST /v1/devices: nonce hex → the device id it was issued for, its expiry. */
  private readonly nonces = new Map<string, { device: string; expires: number }>();
  private readonly tickets = new Map<string, { token: string; expires: number }>();
  private readonly uploadBuckets = new Map<string, { requests: number; bytes: number; at: number }>();
  nowS = NOW_S;
  maxDevices = 8;
  enrolmentsPerHour = 3;
  /** Hex SHA-256 of bytes an attachment references (blob_refs): a backup PUT of them is 409, since backup objects
   *  and attachments never share bytes (branch review BACKUPS-RECOVERY-05). */
  readonly attachmentBytes = new Set<string>();
  readonly blobs = new Map<string, Uint8Array>();
  readonly blobRefs = new Map<string, { uploaderUser: string; confirmed: boolean; created: number }>();
  readonly channelViewers = new Map<string, Set<string>>();
  maxBlobBytes = 104_857_600;
  groupDelete: ((groupHex: string, deviceHex: string, seq: bigint) => 204 | 403 | 404) | null = null;
  private nextBlobFailure: { status: number; code: string; retryAfterMs: number | null } | null = null;
  /** Hex SHA-256 of bytes an operator purged: tombstoned, so a PUT of them is 410 E_PRUNED, while a backup row that
   *  names them is still served (protocol/09 Admin). */
  readonly prunedBytes = new Set<string>();
  /** Hex SHA-256 of bytes a replaced state object's delete unlinks under the next upload of them: that PUT answers
   *  503 E_UNAVAILABLE (retry_after_ms 100) and records nothing, and the retry stores the bytes anew. One shot. */
  readonly bytesDeletedUnderNextUpload = new Set<string>();
  /** blobs.uploads_per_minute and blobs.upload_bytes_per_day, the defaults of internal/config/defaults.go. */
  uploadsPerMinute = 20;
  uploadBytesPerDay = 5_368_709_120;
  private users = 0;
  private sessions = 0;
  private challenges = 0;
  private assertionN = 0;
  private ticketN = 0;
  private readonly overrides: { method: string; path: string; reply: Reply }[] = [];

  /** The next request with this method and path gets `answer` instead of the model's. */
  once(method: string, path: string, answer: Reply): void {
    this.overrides.push({ method, path, reply: answer });
  }
  failNextBlob(status: number, code: string, retryAfterMs?: number): void {
    this.nextBlobFailure = { status, code, retryAfterMs: retryAfterMs ?? null };
  }
  sweepPending(nowS: number, ttlS: number): number {
    if (ttlS <= 0) return 0;
    let dropped = 0;
    for (const [key, ref] of this.blobRefs) if (!ref.confirmed && ref.created < nowS - ttlS) {
      this.blobRefs.delete(key); dropped++;
    }
    return dropped;
  }

  count(method: string, path: string): number {
    return this.log.filter((r) => r.method === method && r.path === path).length;
  }

  paths(): string[] {
    return this.log.map((r) => `${r.method} ${r.path}`);
  }
  setPassword(username: string, password: string, totp: string | null = null): void {
    this.passwords.set(username, { password, totp });
    // An app planted here shows `totp` as its first code (nothing burnt yet).
    if (totp !== null) this.factorsOf(username).totpUsed = 0;
  }
  /** What the account holds; created empty. */
  factorsOf(username: string): FakeFactorState {
    let state = this.factorStates.get(username);
    if (state === undefined) {
      state = { owed: false, totpCode: null, totpUsed: 0, totpEnrol: null, passkeys: [], codes: [] };
      this.factorStates.set(username, state);
    }
    return state;
  }
  /** Marks the account as owing a second factor (a legacy account, an abandoned sign-up, an operator reset). */
  setOwed(username: string, owed = true): void {
    this.factorsOf(username).owed = owed;
  }
  /** A passkey the account already holds; its response string for the fake's login is `credIdHex(credId)`. */
  addPasskey(username: string, name: string): Uint8Array {
    const credId = new Uint8Array(32).fill(0xc0);
    credId[0] = ++this.credN;
    this.factorsOf(username).passkeys.push({ credId, name, created: this.nowS, lastUsed: null });
    return credId;
  }
  /** Unused backup codes the account holds (the fake keeps them in the clear). */
  backupCodesOf(username: string): readonly string[] {
    return this.factorsOf(username).codes;
  }
  private totpOf(username: string): string | null {
    return this.factorsOf(username).totpCode ?? this.passwords.get(username)?.totp ?? null;
  }
  /** The code the account's app shows now: its first code until one is accepted, then the next (one code per use, so a
   *  replayed code is refused as dillad refuses a counter at or below last_counter). null without an app. */
  totpCodeNow(username: string): string | null {
    const base = this.totpOf(username);
    if (base === null) return null;
    const used = this.factorsOf(username).totpUsed;
    return used === 0 ? base : String((Number.parseInt(base, 10) + used) % 1_000_000).padStart(6, '0');
  }
  /** Accepts the app's current code once. */
  private spendTotp(username: string, code: CborValue): boolean {
    const now = this.totpCodeNow(username);
    if (now === null || code !== now) return false;
    this.factorsOf(username).totpUsed += 1;
    return true;
  }
  private needsOf(username: string): 0 | 1 | 2 {
    if (this.totpOf(username) !== null) return 1;
    return this.factorsOf(username).passkeys.length > 0 ? 2 : 0;
  }
  private holdsFactor(username: string): boolean {
    return this.needsOf(username) !== 0;
  }
  private mintCodes(username: string): string[] {
    const codes = Array.from({ length: BACKUP_CODES }, () => {
      let n = ++this.codeN;
      let tail = '';
      while (n > 0) { tail = BASE32[26 + (n % 6)] + tail; n = Math.floor(n / 6); }
      return tail.padStart(26, 'A');
    });
    this.factorsOf(username).codes = codes;
    return codes;
  }
  publishList(userId: Uint8Array, entries: readonly { deviceId: Uint8Array; revokedAt?: bigint | null }[], at: bigint = BigInt(this.nowS)): bigint {
    const user = toHex(userId);
    const version = (this.deviceLists.get(user)?.version ?? 0n) + 1n;
    this.storeList(user, version, fakeListBlob(userId, entries.map((entry) => ({ deviceId: entry.deviceId, revokedAt: entry.revokedAt ?? null })), at),
      new Uint8Array(64), new Uint8Array(32));
    return version;
  }
  putBackupObject(userId: Uint8Array, kind: 0 | 1, object: Uint8Array): void {
    this.backups.set(`${toHex(userId)}/${kind}`, { object, created: BigInt(this.nowS) });
  }

  /** An account and device as if an earlier tab's registration had succeeded. */
  register(username: string, deviceId: Uint8Array, dskPub: Uint8Array = fakeDskPub(deviceId)): Uint8Array {
    this.users += 1;
    const userId = new Uint8Array(16).fill(0x30 + this.users);
    this.accounts.set(toHex(userId), { userId, username, display: username });
    this.devices.set(toHex(deviceId), toHex(userId));
    this.createdAt.set(toHex(deviceId), this.nowS);
    this.tiers.set(toHex(deviceId), 1);
    this.keys.set(toHex(deviceId), toHex(dskPub));
    return userId;
  }

  /** What the gateway's identify answers for a session token or a ticket (protocol/02 item 4, security review F4,
   *  branch review REGISTRATION-DEVICES-01): `ready` only for an enrolled session; a pending, provisional, unknown,
   *  revoked or spent one is an E_UNAUTHENTICATED error frame and close 4003. A ticket is single use and lives 30 s. */
  identify(credential: string): { op: 'ready' } | { op: 'error'; code: 'E_UNAUTHENTICATED'; close: 4003 } {
    const refused = { op: 'error', code: 'E_UNAUTHENTICATED', close: 4003 } as const;
    let token = credential;
    const ticket = this.tickets.get(credential);
    if (ticket !== undefined) {
      this.tickets.delete(credential);
      if (this.nowS > ticket.expires) return refused;
      token = ticket.token;
    }
    const device = this.tokens.get(token);
    if (device === undefined || this.revoked.has(device) || this.tokenScope.get(token) !== 0) return refused;
    return { op: 'ready' };
  }

  private keyOf(device: string): string {
    return this.keys.get(device) ?? toHex(fakeDskPub(fromHex(device)));
  }

  /** Whether the user's newest list names the row by its (device_id, dsk_pub) pair; with no list every row counts. */
  private listed(user: string, device: string): boolean {
    const newest = this.deviceLists.get(user);
    if (newest === undefined) return true;
    const list = readFakeList(newest.blob);
    return list !== null && fakeListNames(list, fromHex(device), fromHex(this.keyOf(device)));
  }

  /** auth.Sessions.AdmitDevice (`evict`, assertion registration): the 24-hour sweep, one live row per key (409), then
   *  the cap and the hourly rate, which decide which row the registration replaces (the oldest live unlisted one,
   *  whatever its age, ties by id), never whether it is admitted; only a cap of listed rows refuses (403).
   *  AdmitEnrolledDevice (not `evict`, POST /v1/devices, branch review REGISTRATION-DEVICES-03): the same sweep, key
   *  rule, cap and rate, but past the cap 403 "device cap reached" and past the rate 429 with the wait until enough
   *  creations leave the hour; it never evicts. Nothing is written on a refusal (the real one runs in the
   *  registration's transaction). */
  private admit(user: string, keyHex: string, evict: boolean): Response | { apply(): void } {
    const rows = [...this.devices].filter(([, owner]) => owner === user).map(([id]) => id);
    const cutoff = this.nowS - 86_399;
    const created = (id: string): number => this.createdAt.get(id) ?? this.nowS;
    const listed = new Set(rows.filter((id) => this.listed(user, id)));
    const swept = rows.filter((id) => !this.revoked.has(id) && created(id) < cutoff && !listed.has(id));
    const live = rows.filter((id) => !this.revoked.has(id) && !swept.includes(id));
    if (live.some((id) => this.keyOf(id) === keyHex))
      return refuse(409, 'E_INVALID_REQUEST', 'dsk_pub is already registered to a live device');
    const creations = live.map(created).filter((at) => at >= this.nowS - 3599);
    const atCap = live.length >= this.maxDevices;
    let evicted: string | undefined;
    if (!evict && atCap) return refuse(403, 'E_FORBIDDEN', 'device cap reached');
    if (!evict && creations.length >= this.enrolmentsPerHour)
      return refuse(429, 'E_RATE_LIMITED', 'rate limited', this.rateWait(creations) * 1000);
    if (atCap || creations.length >= this.enrolmentsPerHour) {
      evicted = live.filter((id) => !listed.has(id))
        .sort((a, b) => created(a) - created(b) || (a < b ? -1 : a > b ? 1 : 0))[0];
      if (evicted === undefined && atCap) return refuse(403, 'E_FORBIDDEN', 'device cap reached');
    }
    return {
      apply: () => {
        for (const id of evicted === undefined ? swept : [...swept, evicted]) {
          this.revoked.set(id, this.nowS);
          this.dropTokens(id);
        }
      },
    };
  }

  /** auth.rateWait: the seconds until enough of the hour's live creations leave the window that one more is under
   *  the rate; a creation at c counts while c >= now-3599, so it leaves at c+3600. */
  private rateWait(creations: readonly number[]): number {
    const sorted = [...creations].sort((a, b) => a - b);
    if (sorted.length === 0) return 3600;
    const i = Math.min(Math.max(sorted.length - this.enrolmentsPerHour, 0), sorted.length - 1);
    const wait = sorted[i] + 3600 - this.nowS;
    return wait > 0 ? wait : 1;
  }

  readonly fetch =async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const request = new Request(input, init);
    const header = request.headers.get('Authorization');
    const entry: LoggedRequest = {
      method: request.method,
      path: new URL(request.url).pathname,
      query: new URL(request.url).search,
      auth: header !== null && header.startsWith('Bearer ') ? header.slice(7) : null,
      contentType: request.headers.get('Content-Type'),
      body: new Uint8Array(await request.arrayBuffer()),
    };
    this.log.push(entry);
    const i = this.overrides.findIndex((o) => o.method === entry.method && o.path === entry.path);
    const override = i >= 0 ? this.overrides.splice(i, 1)[0] : undefined;
    if (override !== undefined) {
      if (override.reply === 'network') throw new TypeError('fetch failed');
      if (override.reply === 'lose') { this.route(entry); throw new TypeError('fetch failed'); }
      return reply(override.reply.status, override.reply.body);
    }
    return this.route(entry);
  };

  // ---- web-3 calls (09 § Voice, L-HTTP-100..104): the routes' shapes and refusals; the call group itself is ModelDs's ----
  /** channel hex → the channel's call as L-HTTP-100 answers it; `dm` marks a DM or group DM (decline is theirs only). */
  readonly calls = new Map<string, { groupId: Uint8Array | null; callId: Uint8Array | null; dm: boolean;
    participants: [Uint8Array, Uint8Array, number][] }>();
  maxVoiceParticipants = 25;
  private proposalSeq = 0;

  private callRoute(r: LoggedRequest, line: string): Response | null {
    const snapshot = /^GET \/v1\/channels\/([0-9a-f]{32})\/call$/.exec(line);
    if (snapshot?.[1] !== undefined) {
      const call = this.calls.get(snapshot[1]);
      return call === undefined ? refuse(404, 'E_NOT_FOUND') : reply(200, [call.groupId, call.callId, call.participants]);
    }
    const start = /^POST \/v1\/channels\/([0-9a-f]{32})\/calls$/.exec(line);
    if (start?.[1] !== undefined) {
      const call = this.calls.get(start[1]);
      if (call === undefined) return refuse(404, 'E_NOT_FOUND');
      if (call.groupId === null) return refuse(403, 'E_LEAF_NOT_CURRENT');
      if (call.participants.length >= this.maxVoiceParticipants) return refuse(409, 'E_CALL_FULL');
      const created = call.callId === null;
      call.callId = fromHex(start[1]);
      return reply(created ? 201 : 200, [call.callId, call.groupId, 'wss://dilla.test/rtc', 'token', [], [64_000, 2_500_000, 1]]);
    }
    const decline = /^POST \/v1\/calls\/([0-9a-f]{32})\/decline$/.exec(line);
    if (decline?.[1] !== undefined) {
      let body: CborValue;
      try { body = decode(r.body); } catch { return refuse(400, 'E_INVALID_REQUEST'); }
      if (!Array.isArray(body) || body.length !== 1 || typeof body[0] !== 'bigint' || body[0] < 0n) return refuse(400, 'E_INVALID_REQUEST');
      const call = [...this.calls.values()].find((c) => c.callId !== null && toHex(c.callId) === decline[1]);
      if (call === undefined) return refuse(404, 'E_NOT_FOUND');
      return call.dm ? reply(204) : refuse(400, 'E_INVALID_REQUEST');
    }
    if (/^POST \/v1\/groups\/[0-9a-f]{32}\/proposal$/.test(line)) { this.proposalSeq += 1; return reply(201, [this.proposalSeq]); }
    const callOp = /^(POST|DELETE) \/v1\/calls\/([0-9a-f]{32})\/(share|stats)$/.exec(line);
    if (callOp?.[2] !== undefined) {
      const live = [...this.calls.values()].some((c) => c.callId !== null && toHex(c.callId) === callOp[2]);
      return live || callOp[1] === 'DELETE' ? reply(204) : refuse(404, 'E_NOT_FOUND');
    }
    return null;
  }

  private route(r: LoggedRequest): Response {
    const line = `${r.method} ${r.path}`;
    if (line === 'GET /v1/instance') {
      return reply(200, [[3, 2], [1], [1], this.instanceId, 1n, 'dilla.test', 0, [0], 1n,
        new Uint8Array(16).fill(0x17), this.externalSenderPub]);
    }
    if (line === 'POST /v1/accounts') return this.createAccount(r.body);
    if (line === 'POST /v1/auth/password/login') {
      const body = arr(decode(r.body), 2);
      const username = str(body[0] ?? null);
      const password = str(body[1] ?? null);
      // The standing lockout gate before the password is looked at; the ledger is cleared only by a login owing nothing.
      if (this.loginLocked(username)) return refuse(429, 'E_RATE_LIMITED', '', 30_000);
      const stored = this.passwords.get(username);
      if (stored === undefined || stored.password !== password) return this.loginFailure(username);
      const needs = this.needsOf(username);
      if (needs === 0) this.loginFailures.delete(username);
      // Wave B task 3 (protocol/09): a login owing a factor says whether the account holds a passkey.
      if (needs === 0) return reply(200, [this.assertion(username, needs, true), needs]);
      return reply(200, [this.assertion(username, needs, true), needs, this.factorsOf(username).passkeys.length > 0 ? 1 : 0]);
    }
    if (line === 'POST /v1/auth/totp/verify') {
      const body = arr(decode(r.body), 2);
      const value = str(body[0] ?? null);
      const code = str(body[1] ?? null);
      const assertion = this.spendAssertion(value);
      if (assertion === undefined || assertion.needs !== 1) return refuse(401, 'E_UNAUTHENTICATED');
      // dillad: a locked account is refused before its code is looked at; the assertion is already spent.
      if (this.loginLocked(assertion.user)) return refuse(429, 'E_RATE_LIMITED', '', 30_000);
      if (!this.spendTotp(assertion.user, code)) return this.loginFailure(assertion.user);
      this.loginFailures.delete(assertion.user);
      return reply(200, [this.assertion(assertion.user, 0)]);
    }
    if (line === 'POST /v1/auth/recovery/verify') {
      const body = arr(decode(r.body), 2);
      const value = str(body[0] ?? null);
      const code = str(body[1] ?? null);
      const assertion = this.spendAssertion(value);
      if (assertion === undefined || assertion.needs === 0) return refuse(401, 'E_UNAUTHENTICATED');
      if (this.loginLocked(assertion.user)) return refuse(429, 'E_RATE_LIMITED', '', 30_000);
      const state = this.factorsOf(assertion.user);
      const at = state.codes.indexOf(code);
      if (at < 0) return this.loginFailure(assertion.user);
      state.codes.splice(at, 1);
      this.loginFailures.delete(assertion.user);
      return reply(200, [this.assertion(assertion.user, 0)]);
    }
    if (line === 'POST /v1/auth/passkey/login/begin') {
      const id = `c-${++this.ceremonyN}`;
      this.ceremonies.set(id, { kind: 'login', username: null, steppedUp: false });
      return reply(200, [this.ceremonyBytes(id), '{"publicKey":{"challenge":"fake"}}']);
    }
    if (line === 'POST /v1/auth/passkey/login/finish') {
      const body = arr(decode(r.body));
      if (body.length !== 2 && body.length !== 3) return refuse(400, 'E_INVALID_REQUEST');
      // Wave B task 3: an optional third element binds the passkey to a live first-factor (password or OIDC) login owing a factor.
      let boundUser: string | null = null;
      if (body.length === 3) {
        const held = this.activeAssertions.get(str(body[2] ?? null));
        if (held === undefined || held.firstFactor !== true || held.needs === 0 || this.nowS > held.expires) {
          return refuse(401, 'E_UNAUTHENTICATED');
        }
        boundUser = held.user;
      }
      const ceremony = this.takeCeremony(body[0] ?? null, 'login');
      const response = str(body[1] ?? null);
      if (ceremony === null) return refuse(401, 'E_UNAUTHENTICATED');
      // The fake's authenticator answers with the credential id in hex, and ":nouv" when it did not verify the user.
      const [credHex, flagText] = response.split(':');
      for (const account of this.accounts.values()) {
        const held = this.factorsOf(account.username).passkeys.find((k) => toHex(k.credId) === credHex);
        if (held === undefined) continue;
        const needs = flagText === 'nouv' ? 1 : 0;
        // dillad: a login that would owe a code the lockout forbids guessing gets no assertion; a complete one clears it.
        if (needs === 1 && this.loginLocked(account.username)) return refuse(429, 'E_RATE_LIMITED', '', 30_000);
        if (needs === 0) this.loginFailures.delete(account.username);
        if (boundUser !== null && boundUser !== account.username) return refuse(403, 'E_PASSKEY_OTHER_ACCOUNT');
        if (boundUser !== null) this.activeAssertions.delete(str(body[2] ?? null));
        held.lastUsed = this.nowS;
        return reply(200, [this.assertion(account.username, needs), needs]);
      }
      return refuse(401, 'E_PASSKEY_UNKNOWN');
    }
    if (line === 'GET /v1/instance/limits') {
      return reply(200, [131072, 104857600, 10, 2, 32, 8, 1073741824, 30000, 131584, 30, 30]);
    }
    const challenge = /^POST \/v1\/devices\/([0-9a-f]{32})\/sessions\/challenge$/.exec(line);
    if (challenge?.[1] !== undefined) {
      this.challenges += 1;
      const nonce = nonceAt(this.challenges);
      this.nonces.set(toHex(nonce), { device: challenge[1], expires: this.nowS + 60 });
      return reply(201, [nonce, this.nowS + 60]);
    }
    const establish = /^POST \/v1\/devices\/([0-9a-f]{32})\/sessions$/.exec(line);
    if (establish?.[1] !== undefined) return this.establish(establish[1], r.body);
    const device = r.auth === null ? undefined : this.tokens.get(r.auth);
    const user = device === undefined ? undefined : this.devices.get(device);
    if (device === undefined || user === undefined || this.revoked.has(device)) return refuse(401, 'E_UNAUTHENTICATED');
    const username = this.accounts.get(user)?.username;
    if (username !== undefined) {
      // dillad's order (session.go middleware): the scope check first, then the owed rule for every scope. The session
      // routes of § Second factors are E (enrolled only): a pending token is E_FORBIDDEN there, a provisional one
      // E_PROVISIONAL_OUTSIDE_PAIRING. An owed account is refused on every route but the F allow-list and the P routes,
      // enrolled sessions and provisional ones on the "E or V" routes alike; a pending one never passes the scope check
      // to a non-P route (the fake's later pending check answers that).
      const scope = this.tokenScope.get(r.auth!) ?? 0;
      const factorLine = line.startsWith('GET /v1/auth/factors') || line.startsWith('POST /v1/auth/');
      if (factorLine && scope === 1) return refuse(403, 'E_FORBIDDEN');
      if (factorLine && scope === 2) return refuse(403, 'E_PROVISIONAL_OUTSIDE_PAIRING');
      const reachesOwedRule = scope === 0 || (scope === 2 && E_OR_V.some((re) => re.test(line)));
      if (reachesOwedRule && this.factorsOf(username).owed && !OWED_OK.some((re) => re.test(line))) return refuse(403, 'E_FACTOR_REQUIRED');
      const factorRoute = this.factorRoute(r, line, username);
      if (factorRoute !== null) return factorRoute;
    }
    const callRoute = this.callRoute(r, line);
    if (callRoute !== null) return callRoute;
    const list =/^(PUT|GET) \/v1\/users\/([0-9a-f]{32})\/device-list$/.exec(line);
    if (list?.[2] !== undefined) {
      if (list[1] === 'PUT') {
        if (user !== list[2]) return refuse(403, 'E_FORBIDDEN');
        return this.putList(user, list[2], r.body);
      }
      if (user !== list[2] && this.tokenScope.get(r.auth!) === 1) return refuse(403, 'E_FORBIDDEN');
      return this.getList(list[2], r.query);
    }
    if (this.tokenScope.get(r.auth!) === 1 && line !== 'GET /v1/backups' &&
        !/^GET \/v1\/backups\/[01]\/0$/.test(line)) return refuse(403, 'E_FORBIDDEN');
    const messageDelete = /^DELETE \/v1\/groups\/([0-9a-f]{32})\/messages\/([0-9]{1,20})$/.exec(line);
    if (messageDelete) {
      const result = this.groupDelete?.(messageDelete[1], device, BigInt(messageDelete[2])) ?? 404;
      if (result === 204) return reply(204);
      if (result === 403) return refuse(403, 'E_NOT_UPLOADER', 'only the uploading user may delete this message');
      return refuse(404, 'E_NOT_FOUND', 'no such object');
    }
    const blob = /^(PUT|GET|DELETE) \/v1\/channels\/([0-9a-f]{32})\/blobs\/([0-9a-f]{64})$/.exec(line);
    if (blob) return this.blob(user, blob[1], blob[2], blob[3], r);
    const confirm = /^POST \/v1\/channels\/([0-9a-f]{32})\/blobs\/([0-9a-f]{64})\/confirm$/.exec(line);
    if (confirm) return this.blob(user, 'POST', confirm[1], confirm[2], r);
    if (line === 'GET /v1/backups') return reply(200, ([0, 1] as const).flatMap((kind) => {
      const item = this.backups.get(`${user}/${kind}`);
      return item === undefined ? [] : [[kind, 0, item.object.length, item.created]];
    }));
    const backup = /^(GET|PUT|DELETE) \/v1\/backups\/([0-9]+)\/([0-9]+)$/.exec(line);
    if (backup !== null) return this.backup(user, backup[1], Number(backup[2]), Number(backup[3]), r.body);
    if (line === 'DELETE /v1/backups') return refuse(501, 'E_INTERNAL');
    if (line === 'GET /v1/devices') return reply(200, [...this.devices].filter(([, owner]) => owner === user).map(([id]) => [
      fromHex(id), this.tiers.get(id) ?? 1, this.tiers.get(id) ?? 1, null, this.revoked.has(id) ? this.revoked.get(id)! : null,
      this.createdAt.get(id) ?? this.nowS, this.labels.get(id) ?? null,
    ]));
    const patchDevice = /^PATCH \/v1\/devices\/([0-9a-f]{32})$/.exec(line);
    if (patchDevice !== null) {
      const target = patchDevice[1];
      let label: string | null;
      try {
        const b = arr(decode(r.body), 1);
        if (b.length !== 1 || (b[0] !== null && typeof b[0] !== 'string')) throw new Error('shape');
        label = b[0] ?? null;
        if (label !== null && (label.length < 1 || [...label].length > 64)) throw new Error('length');
      } catch { return refuse(400, 'E_INVALID_REQUEST'); }
      if (this.devices.get(target) !== user) return refuse(404, 'E_NOT_FOUND');
      if (label === null) this.labels.delete(target); else this.labels.set(target, label);
      return reply(204);
    }
    const deleteSessions = /^DELETE \/v1\/devices\/([0-9a-f]{32})\/sessions$/.exec(line);
    if (deleteSessions !== null) {
      const target = deleteSessions[1];
      if (this.devices.get(target) !== user) return refuse(404, 'E_NOT_FOUND');
      // The same grace for another device's young unlisted row (fix-wave review NEW-1).
      const listed = this.deviceLists.has(user) && this.listed(user, target);
      if (target !== device && !listed && (this.createdAt.get(target) ?? this.nowS) > this.nowS - 600)
        return refuse(409, 'E_INVALID_REQUEST', 'the device registered less than 10 minutes ago and is too new to remove ' +
          'its sessions; its own session may remove them, or it expires unlisted after 24 hours');
      this.dropTokens(target);
      return reply(204);
    }
    const deleteDevice = /^DELETE \/v1\/devices\/([0-9a-f]{32})$/.exec(line);
    if (deleteDevice !== null) {
      const target = deleteDevice[1];
      if (this.devices.get(target) !== user) return refuse(404, 'E_NOT_FOUND');
      // Listed is the pair: a row that only copies a listed key, or a listed id under another key, is removed here.
      if (this.deviceLists.has(user) && this.listed(user, target))
        return refuse(409, 'E_INVALID_REQUEST', 'a listed device is revoked by a signed device list');
      // The grace (branch review REGISTRATION-DEVICES-03): an unlisted row younger than 10 minutes is removed only
      // by its own session, so a stolen enrolled session cannot remove a recovering owner's new row on sight.
      if (target !== device && (this.createdAt.get(target) ?? this.nowS) > this.nowS - 600)
        return refuse(409, 'E_INVALID_REQUEST', 'the device registered less than 10 minutes ago and is too new to remove; ' +
          'its own session may remove it, or it expires unlisted after 24 hours');
      this.revoked.set(target, this.nowS);
      this.dropTokens(target);
      return reply(204);
    }
    if (line === 'POST /v1/devices') return this.createDevice(user, r.body);
    if (line === 'POST /v1/gateway/ticket') {
      const ticket = `tkt-${++this.ticketN}`;
      this.tickets.set(ticket, { token: r.auth!, expires: this.nowS + 30 });
      return reply(201, [ticket, this.nowS + 30]);
    }
    if (line === 'POST /v1/keypackages') {
      const b = arr(decode(r.body), 2);
      const count = arr(b[0] ?? null).length;
      this.keyPackages.push({ device, count, lastResort: b[1] !== null });
      return reply(201, [count]);
    }
    if (line === 'GET /v1/accounts/me') {
      const a = this.accounts.get(user);
      if (a === undefined) return refuse(404, 'E_NOT_FOUND');
      return reply(200, [a.userId, a.username, a.display, 0, 0, this.nowS]);
    }
    return new Response('404 page not found\n', { status: 404, headers: { 'Content-Type': 'text/plain; charset=utf-8' } });
  }

  private blob(user: string, method: string, channel: string, blobId: string, r: LoggedRequest): Response {
    const failure = this.nextBlobFailure;
    this.nextBlobFailure = null;
    if (failure) return refuse(failure.status, failure.code, '', failure.retryAfterMs);
    const viewers = this.channelViewers.get(channel);
    if (viewers && !viewers.has(user)) return refuse(404, 'E_NOT_FOUND', 'no such object');
    const key = `${channel}:${blobId}`;
    const ref = this.blobRefs.get(key);
    if (method === 'PUT') {
      if (r.contentType?.split(';', 1)[0].trim().toLowerCase() !== 'application/octet-stream')
        return refuse(415, 'E_INVALID_REQUEST', 'Content-Type must be application/octet-stream');
      if (this.prunedBytes.has(blobId)) return refuse(410, 'E_PRUNED');
      const metered = this.meter(user, Math.min(r.body.length, this.maxBlobBytes));
      if (metered) return metered;
      if (r.body.length > this.maxBlobBytes) return refuse(413, 'E_TOO_LARGE', `at most ${this.maxBlobBytes} bytes`);
      if (createHash('sha256').update(r.body).digest('hex') !== blobId)
        return refuse(422, 'E_INVALID_REQUEST', 'the body does not hash to the requested blob_id');
      const created = !this.blobs.has(blobId);
      if (created) this.blobs.set(blobId, r.body.slice());
      this.attachmentBytes.add(blobId);
      if (!ref) this.blobRefs.set(key, { uploaderUser: user, confirmed: false, created: this.nowS });
      return reply(created ? 201 : 200, [fromHex(blobId), r.body.length]);
    }
    if (method === 'DELETE') {
      if (!ref) return reply(204);
      if (ref.uploaderUser !== user) return refuse(403, 'E_NOT_UPLOADER', 'only the uploading user may delete this object');
      this.blobRefs.delete(key);
      return reply(204);
    }
    if (this.prunedBytes.has(blobId)) return refuse(410, 'E_PRUNED');
    if (!ref) return refuse(404, 'E_NOT_FOUND', 'no such object');
    if (method === 'POST') {
      if (ref.uploaderUser !== user) return refuse(403, 'E_NOT_UPLOADER');
      ref.confirmed = true;
      return reply(204);
    }
    const bytes = this.blobs.get(blobId);
    if (!bytes) return refuse(404, 'E_NOT_FOUND', 'no such object');
    return new Response(bytes.slice(), { status: 200, headers: { 'Content-Type': 'application/octet-stream', 'X-Dilla-Generation': '1' } });
  }

  private createAccount(body: Uint8Array): Response {
    const b = arr(decode(body), 8);
    const invite = str(b[0] ?? null);
    const username = str(b[1] ?? null);
    const display = str(b[2] ?? null);
    const deviceId = bin(arr(b[7] ?? null, 5)[0] ?? null, 16);
    const dskPub = bin(arr(b[7] ?? null, 5)[1] ?? null, 32);
    if (!this.invites.has(invite)) return refuse(410, 'E_INVITE_INVALID');
    if ([...this.accounts.values()].some((a) => a.username === username)) return refuse(409, 'E_INVALID_REQUEST', 'username taken');
    const userId = this.register(username, deviceId, dskPub);
    this.tiers.set(toHex(deviceId), Number(arr(b[7] ?? null, 5)[2]) as 0 | 1);
    const account = this.accounts.get(toHex(userId));
    if (account !== undefined) {
      account.display = display;
      if (this.factorRequired) this.factorsOf(account.username).owed = true;
    }
    return reply(200, [userId, deviceId, this.mint(toHex(deviceId), 0), this.nowS + SESSION_S]);
  }

  private assertion(user: string, needs: 0 | 1 | 2, firstFactor = false): string {
    const value = `asrt-${++this.assertionN}`;
    this.assertions.push(value);
    this.activeAssertions.set(value, { user, needs, expires: this.nowS + ASSERTION_TTL_S, ...(firstFactor ? { firstFactor: true as const } : {}) });
    return value;
  }
  /** auth.Assertions.SpendNeeds: single use, and undefined once past its expiry (dillad: now after expires). */
  private spendAssertion(value: string): { user: string; needs: 0 | 1 | 2 } | undefined {
    const held = this.activeAssertions.get(value);
    this.activeAssertions.delete(value);
    return held === undefined || this.nowS > held.expires ? undefined : held;
  }
  private ceremonyBytes(id: string): Uint8Array {
    const out = new Uint8Array(16);
    out.set(new TextEncoder().encode(id.slice(0, 16)));
    return out;
  }
  private peekCeremony(raw: CborValue): { kind: 'register' | 'login' | 'stepup'; username: string | null } | undefined {
    if (!(raw instanceof Uint8Array)) return undefined;
    return this.ceremonies.get(new TextDecoder().decode(raw).replace(/\0+$/, ''));
  }
  private takeCeremony(raw: CborValue, kind: 'register' | 'login' | 'stepup'): { username: string | null; steppedUp: boolean } | null {
    if (!(raw instanceof Uint8Array)) return null;
    const id = new TextDecoder().decode(raw).replace(/\0+$/, '');
    const open = this.ceremonies.get(id);
    this.ceremonies.delete(id);
    return open !== undefined && open.kind === kind ? open : null;
  }
  private spendStepUp(token: CborValue, username: string, session: string | null): boolean {
    if (typeof token !== 'string') return false;
    const held = this.stepUps.get(token);
    // Only its own session spends it: another session's attempt is refused and does not burn the owner's token.
    if (held === undefined || held.username !== username || held.session !== session) return false;
    this.stepUps.delete(token);
    return true;
  }
  private recent(ledger: Map<string, number[]>, username: string): number[] {
    const kept = (ledger.get(username) ?? []).filter((at) => at > this.nowS - OBSERVATION_WINDOW_S);
    ledger.set(username, kept);
    return kept;
  }
  /** Step-up failures made at other client addresses: they count on the account-wide ledger only. */
  stepUpFailuresElsewhere(username: string, count: number): void {
    for (let i = 0; i < count; i += 1) this.recent(this.stepUpAccount, username).push(this.nowS);
  }
  private stepUpLocked(username: string): boolean {
    return (this.stepUpFailures.get(username) ?? 0) > FREE_ATTEMPTS || this.recent(this.stepUpAccount, username).length > FREE_ATTEMPTS;
  }
  /** A failed step-up guess (a secret, a passkey or an old password): counted on both ledgers; true when it locks them. */
  private stepUpFail(username: string): boolean {
    const here = (this.stepUpFailures.get(username) ?? 0) + 1;
    this.stepUpFailures.set(username, here);
    const all = this.recent(this.stepUpAccount, username);
    all.push(this.nowS);
    return here > FREE_ATTEMPTS || all.length > FREE_ATTEMPTS;
  }
  /** 403 E_STEP_UP, or 429 on the failure that passes the free attempts. */
  private stepUpFailure(username: string): Response {
    return this.stepUpFail(username) ? refuse(429, 'E_RATE_LIMITED', '', 60_000) : refuse(403, 'E_STEP_UP');
  }
  /** A success clears this address's count only; the account-wide one ages out. */
  private stepUpPass(username: string): void {
    this.stepUpFailures.delete(username);
  }
  private loginLocked(username: string): boolean {
    return this.recent(this.loginFailures, username).length > FREE_ATTEMPTS;
  }
  /** A wrong password or second-factor code: 401, or 429 on the failure that locks the account. */
  private loginFailure(username: string): Response {
    const all = this.recent(this.loginFailures, username);
    all.push(this.nowS);
    return all.length > FREE_ATTEMPTS ? refuse(429, 'E_RATE_LIMITED', '', 30_000) : refuse(401, 'E_UNAUTHENTICATED');
  }
  private issueStepUp(username: string, session: string | null): Response {
    const token = `su-${++this.stepUpN}`;
    this.stepUps.set(token, { username, session: session ?? '' });
    return reply(201, [token, this.nowS + FAKE_STEP_UP_EXPIRES_S]);
  }

  /** The second-factor routes of protocol/09 § Second factors that need a session. */
  private factorRoute(r: LoggedRequest, line: string, username: string): Response | null {
    if (!line.startsWith('GET /v1/auth/factors') && !line.startsWith('POST /v1/auth/')) return null;
    const state = this.factorsOf(username);
    const session = r.auth;
    let body: CborValue[] = [];
    if (r.method === 'POST') {
      try { body = arr(decode(r.body)); } catch { return refuse(400, 'E_INVALID_REQUEST'); }
    }
    const held = this.holdsFactor(username);
    // [stepup|null] exactly (dillad's stepUpRequest): any other arity does not decode.
    if ((line === 'POST /v1/auth/totp/enroll' || line === 'POST /v1/auth/passkey/register/begin' || line === 'POST /v1/auth/recovery/codes')
      && (body.length !== 1 || (body[0] !== null && typeof body[0] !== 'string'))) return refuse(400, 'E_INVALID_REQUEST');
    switch (line) {
      case 'GET /v1/auth/factors':
        return reply(200, [state.owed ? 1 : 0, this.totpOf(username) === null ? null : NOW_S,
          state.passkeys.map((k) => [k.credId, k.name, k.created, k.lastUsed]), state.codes.length, this.passwords.has(username) ? 1 : 0]);
      case 'POST /v1/auth/totp/enroll':
        if (this.totpOf(username) !== null) return refuse(403, 'E_FORBIDDEN');
        if (held && !this.spendStepUp(body[0] ?? null, username, session)) return refuse(403, 'E_STEP_UP');
        // Each enroll replaces the unconfirmed secret and records whether it spent a step-up (S2's in-memory record).
        state.totpEnrol = { steppedUp: held };
        return reply(200, [FAKE_TOTP_SECRET, `otpauth://totp/dilla:${username}?secret=${FAKE_TOTP_SECRET}&issuer=dilla`]);
      case 'POST /v1/auth/totp/confirm': {
        // dillad reads the enrolled secret row (none: 404), refuses a code that does not match it or was used
        // (400 E_INVALID_REQUEST), then re-checks the step-up rule: a factor held now that the enroll did not step up
        // for is 403 E_STEP_UP, and nothing is written.
        const enrol = state.totpEnrol;
        if (enrol === null || this.totpOf(username) !== null) return refuse(404, 'E_NOT_FOUND');
        if (body[0] !== FAKE_TOTP_CODE) return refuse(400, 'E_INVALID_REQUEST', 'the code does not match this secret');
        if (held && !enrol.steppedUp) return refuse(403, 'E_STEP_UP');
        state.totpEnrol = null;
        state.totpCode = FAKE_TOTP_CODE;
        state.totpUsed = 1;
        state.owed = false;
        return reply(200, [state.codes.length === 0 ? this.mintCodes(username) : []]);
      }
      case 'POST /v1/auth/passkey/register/begin': {
        // dillad: the cap before any token is spent.
        if (state.passkeys.length >= MAX_PASSKEYS) return refuse(409, 'E_INVALID_REQUEST', 'this account holds the most passkeys it may');
        if (held && !this.spendStepUp(body[0] ?? null, username, session)) return refuse(403, 'E_STEP_UP');
        const id = `r-${++this.ceremonyN}`;
        this.ceremonies.set(id, { kind: 'register', username, steppedUp: held });
        return reply(200, [this.ceremonyBytes(id), '{"publicKey":{"challenge":"fake"}}']);
      }
      case 'POST /v1/auth/passkey/register/finish': {
        // dillad checks the name before it touches the ceremony, which a refused name leaves open; an unknown, spent or
        // other account's ceremony is one answer, 400 E_INVALID_REQUEST.
        const name = body[2] ?? null;
        if (!validPasskeyName(name) || typeof name !== 'string') return refuse(400, 'E_INVALID_REQUEST');
        // Another account's ceremony is refused before it is taken, so the account that began it can still finish it.
        const open = this.peekCeremony(body[0] ?? null);
        if (open !== undefined && open.kind === 'register' && open.username !== username)
          return refuse(400, 'E_INVALID_REQUEST', 'the registration response was not accepted');
        const ceremony = this.takeCeremony(body[0] ?? null, 'register');
        if (ceremony === null || ceremony.username !== username) return refuse(400, 'E_INVALID_REQUEST', 'the registration response was not accepted');
        if (held && !ceremony.steppedUp) return refuse(403, 'E_STEP_UP');
        if (state.passkeys.length >= MAX_PASSKEYS) return refuse(409, 'E_INVALID_REQUEST', 'this account holds the most passkeys it may');
        const credId = this.addPasskey(username, name);
        state.owed = false;
        return reply(200, [credId, state.codes.length === 0 ? this.mintCodes(username) : []]);
      }
      case 'POST /v1/auth/stepup': {
        const secret = body[1];
        if ((body[0] !== 0n && body[0] !== 1n && body[0] !== 2n) || typeof secret !== 'string') return refuse(400, 'E_INVALID_REQUEST');
        if (this.stepUpLocked(username)) return refuse(429, 'E_RATE_LIMITED', '', 60_000);
        let ok = false;
        if (body[0] === 0n) ok = this.passwords.get(username)?.password === secret;
        else if (body[0] === 1n) ok = this.spendTotp(username, secret);
        else {
          const at = state.codes.indexOf(secret);
          if (at >= 0) { state.codes.splice(at, 1); ok = true; }
        }
        if (!ok) return this.stepUpFailure(username);
        this.stepUpPass(username);
        return this.issueStepUp(username, session);
      }
      case 'POST /v1/auth/stepup/passkey/begin': {
        if (state.passkeys.length === 0) return refuse(400, 'E_INVALID_REQUEST', 'this account holds no passkey');
        const id = `s-${++this.ceremonyN}`;
        this.ceremonies.set(id, { kind: 'stepup', username, steppedUp: false });
        return reply(200, [this.ceremonyBytes(id), '{"publicKey":{"challenge":"fake"}}']);
      }
      case 'POST /v1/auth/stepup/passkey/finish': {
        // The same ledger as POST /v1/auth/stepup (dillad: stepUpFailure).
        if (this.stepUpLocked(username)) return refuse(429, 'E_RATE_LIMITED', '', 60_000);
        const ceremony = this.takeCeremony(body[0] ?? null, 'stepup');
        if (ceremony === null || ceremony.username !== username) return this.stepUpFailure(username);
        const credHex = typeof body[1] === 'string' ? body[1].split(':')[0] : null;
        if (!state.passkeys.some((k) => toHex(k.credId) === credHex)) return this.stepUpFailure(username);
        this.stepUpPass(username);
        return this.issueStepUp(username, session);
      }
      case 'POST /v1/auth/password': {
        // [old|null, new, stepup?] (E). The old password is this route's step-up: guesses go on the step-up ledgers,
        // never on the login ledger, and a stepup element is then ignored. A first password (no old one) needs a session
        // younger than the reauth window (modelled: 403 E_FORBIDDEN, checked before the step-up), and on an account holding a factor a step-up token, spent.
        const old = body[0] ?? null;
        const next = body[1];
        const stepup = body[2] ?? null;
        if ((body.length !== 2 && body.length !== 3) || (old !== null && typeof old !== 'string') || typeof next !== 'string'
          || (stepup !== null && typeof stepup !== 'string')) return refuse(400, 'E_INVALID_REQUEST');
        if (next === '') return refuse(400, 'E_INVALID_REQUEST', 'the new password is empty');
        const stored = this.passwords.get(username);
        if (stored !== undefined) {
          if (old === null) return refuse(403, 'E_FORBIDDEN', 'changing a password needs the old one');
          if (this.stepUpLocked(username)) return refuse(429, 'E_RATE_LIMITED', '', 60_000);
          if (stored.password !== old) {
            return this.stepUpFail(username) ? refuse(429, 'E_RATE_LIMITED', '', 60_000) : refuse(403, 'E_FORBIDDEN', 'the old password is wrong');
          }
          this.stepUpPass(username);
        } else if (session !== null && this.nowS - (this.tokenCreated.get(session) ?? this.nowS) > REAUTH_WINDOW_S) {
          return refuse(403, 'E_FORBIDDEN', 'setting a first password needs a session established in the last 300 seconds');
        } else if (held && !this.spendStepUp(stepup, username, session)) {
          return refuse(403, 'E_STEP_UP');
        }
        this.passwords.set(username, { password: next, totp: stored?.totp ?? null });
        this.loginFailures.delete(username);
        return reply(204);
      }
      case 'POST /v1/auth/factors/remove': {
        // dillad validates kind and cred_id before it spends the token, which a malformed body leaves unspent.
        const kind = body[1];
        const cred = body[2] ?? null;
        if (kind !== 0n && kind !== 1n) return refuse(400, 'E_INVALID_REQUEST', 'kind is 0 (totp) or 1 (passkey)');
        if (kind === 1n && !(cred instanceof Uint8Array && cred.length > 0)) return refuse(400, 'E_INVALID_REQUEST', 'a passkey removal names its cred_id');
        if (!this.spendStepUp(body[0] ?? null, username, session)) return refuse(403, 'E_STEP_UP');
        if (kind === 0n) {
          // The app row, confirmed or not (an unconfirmed enrolment is deleted too), then the recount.
          if (this.totpOf(username) === null && state.totpEnrol === null) return refuse(404, 'E_NOT_FOUND');
          const keep = state.passkeys.length > 0;
          if (!keep) return refuse(409, 'E_INVALID_REQUEST', 'it is the last factor');
          state.totpEnrol = null;
          state.totpCode = null;
          state.totpUsed = 0;
          const stored = this.passwords.get(username);
          if (stored !== undefined) stored.totp = null;
          return reply(204);
        }
        if (cred instanceof Uint8Array) {
          const at = state.passkeys.findIndex((k) => same(k.credId, cred));
          if (at < 0) return refuse(404, 'E_NOT_FOUND');
          if (state.passkeys.length === 1 && this.totpOf(username) === null) return refuse(409, 'E_INVALID_REQUEST', 'it is the last factor');
          state.passkeys.splice(at, 1);
          return reply(204);
        }
        return refuse(400, 'E_INVALID_REQUEST');
      }
      case 'POST /v1/auth/recovery/codes':
        if (!this.spendStepUp(body[0] ?? null, username, session)) return refuse(403, 'E_STEP_UP');
        return reply(200, [this.mintCodes(username)]);
      default:
        return null;
    }
  }
  private mint(deviceHex: string, scope: 0 | 1): string {
    this.sessions += 1;
    const token = `tok-${this.sessions}`;
    this.tokens.set(token, deviceHex);
    this.tokenCreated.set(token, this.nowS);
    this.tokenScope.set(token, scope);
    return token;
  }

  private establish(deviceHex: string, body: Uint8Array): Response {
    const b = arr(decode(body), 5);
    const loginBytes = b[4];
    const login = loginBytes instanceof Uint8Array ? new TextDecoder().decode(loginBytes) : null;
    let user = this.devices.get(deviceHex);
    let scope: 0 | 1;
    if (login !== null) {
      if (user === undefined) {
        if (!Array.isArray(b[3]) || b[3].length !== 5) return refuse(400, 'E_INVALID_REQUEST', 'registration array required');
        const registration = b[3];
        if (!(registration[0] instanceof Uint8Array) || registration[0].length !== 16 ||
            !same(registration[0], fromHex(deviceHex))) return refuse(400, 'E_INVALID_REQUEST', 'device_id does not match');
        if (registration[2] !== 1n || registration[3] !== 1n)
          return refuse(400, 'E_INVALID_REQUEST', 'assertion registration is for browser devices');
        if (!(registration[1] instanceof Uint8Array) || registration[1].length !== 32)
          return refuse(400, 'E_INVALID_REQUEST');
      }
      const assertion = this.spendAssertion(login);
      if (assertion === undefined || assertion.needs !== 0) return refuse(401, 'E_UNAUTHENTICATED');
      const account = [...this.accounts].find(([, value]) => value.username === assertion.user);
      if (account === undefined) return refuse(401, 'E_UNAUTHENTICATED');
      if (user !== undefined) {
        if (user !== account[0] || this.revoked.has(deviceHex)) return refuse(401, 'E_UNAUTHENTICATED');
      } else {
        user = account[0];
        if (!this.deviceLists.has(user)) return refuse(401, 'E_UNAUTHENTICATED');
        const key = toHex(bin(arr(b[3] ?? null, 5)[1] ?? null, 32));
        const admitted = this.admit(user, key, true);
        if (admitted instanceof Response) return admitted;
        admitted.apply();
        this.devices.set(deviceHex, user);
        this.createdAt.set(deviceHex, this.nowS);
        this.tiers.set(deviceHex, 1);
        this.keys.set(deviceHex, key);
      }
      scope = 1;
    } else {
      if (user === undefined || this.revoked.has(deviceHex)) return refuse(401, 'E_UNAUTHENTICATED');
      if (this.scope !== null) scope = this.scope;
      else {
        const newest = this.deviceLists.get(user);
        if (newest === undefined) scope = 0;
        else {
          const list = readFakeList(newest.blob);
          if (list === null || toHex(list.userId) !== user) return refuse(401, 'E_UNAUTHENTICATED');
          scope = fakeListNames(list, fromHex(deviceHex), fromHex(this.keyOf(deviceHex))) ? 0 : 1;
        }
      }
    }
    if (this.scope !== null) scope = this.scope;
    return reply(201, [this.mint(deviceHex, scope), scope, fromHex(user), fromHex(deviceHex),
      this.nowS + SESSION_S, this.nowS + IDLE_S, 1]);
  }

  /** POST /v1/devices (enrolled): [device_id, dsk_pub, tier, signer_tier, credential, nonce b32, sig b64] → 200
   *  [device_id]. The nonce is a challenge for the new device_id, spent here; sig is the purpose-0 proof by dsk_pub
   *  (403 "possession of dsk_pub is not proven"); then the enrolled admission (409 for a held key, the cap's 403, the
   *  rate's 429, never an eviction); a device_id that exists is 409. */
  private createDevice(user: string, body: Uint8Array): Response {
    let device: Uint8Array, key: Uint8Array, tier: bigint, nonce: CborValue, sig: CborValue;
    try {
      const b = arr(decode(body), 7);
      device = bin(b[0] ?? null, 16);
      key = bin(b[1] ?? null);
      tier = u64(b[2] ?? null);
      const signer = u64(b[3] ?? null);
      const credential = bin(b[4] ?? null);
      nonce = b[5] ?? null;
      sig = b[6] ?? null;
      if (device.every((x) => x === 0) || key.length !== 32 || tier > 1n || signer > 1n || credential.length < 1 ||
          credential.length > 8192) throw new Error('shape');
    } catch { return refuse(400, 'E_INVALID_REQUEST'); }
    const deviceHex = toHex(device);
    const notProven = refuse(403, 'E_FORBIDDEN', 'possession of dsk_pub is not proven');
    if (!(nonce instanceof Uint8Array) || nonce.length !== 32 || !(sig instanceof Uint8Array) || sig.length !== 64) return notProven;
    const issued = this.nonces.get(toHex(nonce));
    this.nonces.delete(toHex(nonce));
    if (issued === undefined || issued.device !== deviceHex || this.nowS > issued.expires ||
        !same(sig, fakeDeviceProof(key, device, nonce))) return notProven;
    const admitted = this.admit(user, toHex(key), false);
    if (admitted instanceof Response) return admitted;
    if (this.devices.has(deviceHex)) return refuse(409, 'E_INVALID_REQUEST', 'device_id already exists');
    admitted.apply();
    this.devices.set(deviceHex, user);
    this.createdAt.set(deviceHex, this.nowS);
    this.tiers.set(deviceHex, Number(tier) as 0 | 1);
    this.keys.set(deviceHex, toHex(key));
    return reply(200, [device]);
  }

  /** The blob upload meter PUT /v1/backups shares with the attachment routes (api.UploadMeter): two token buckets
   *  per user, refilled continuously from full. Answers the refusal, or null having spent one request and `bytes`. */
  private meter(user: string, bytes: number): Response | null {
    const perMinute = this.uploadsPerMinute;
    const perDay = this.uploadBytesPerDay;
    if (perMinute <= 0 && perDay <= 0) return null;
    let u = this.uploadBuckets.get(user);
    if (u === undefined) {
      u = { requests: perMinute, bytes: perDay, at: this.nowS };
      this.uploadBuckets.set(user, u);
    } else if (this.nowS > u.at) {
      const elapsed = this.nowS - u.at;
      u.requests = Math.min(perMinute, u.requests + elapsed * perMinute / 60);
      u.bytes = Math.min(perDay, u.bytes + elapsed * perDay / 86_400);
      u.at = this.nowS;
    }
    const after = (seconds: number): Response => refuse(429, 'E_RATE_LIMITED', 'rate limited', Math.floor(seconds * 1000));
    if (perMinute > 0 && u.requests < 1) return after((1 - u.requests) * 60 / perMinute);
    const held = perDay <= 0 ? 0 : Math.min(bytes, perDay);
    if (held > 0 && u.bytes < held) return after((held - u.bytes) * 86_400 / perDay);
    if (perMinute > 0) u.requests -= 1;
    u.bytes -= held;
    return null;
  }

  private putList(user: string, target: string, body: Uint8Array): Response {
    if (user !== target) return refuse(403, 'E_FORBIDDEN');
    const b = arr(decode(body), 4);
    const version = u64(b[0] ?? null);
    const expected = (this.deviceLists.get(target)?.version ?? 0n) + 1n;
    if (version !== expected) return refuse(409, 'E_INVALID_REQUEST', `device-list version ${version} is not the next version`);
    this.storeList(target, version, bin(b[1] ?? null), bin(b[2] ?? null), bin(b[3] ?? null));
    return reply(204);
  }

  private getList(target: string, query: string): Response {
    if (query.startsWith('?after=')) {
      const after = BigInt(query.slice(7));
      const rows = (this.listHistory.get(target) ?? []).filter((row) => row.version > after).slice(0, 64);
      return reply(200, rows.map((row) => [row.version, row.blob, row.sig, row.prev]));
    }
    const l = this.deviceLists.get(target);
    if (l === undefined) return refuse(404, 'E_NOT_FOUND');
    const row = this.listHistory.get(target)?.find((item) => item.version === l.version);
    return reply(200, [l.version, l.blob, row?.sig ?? new Uint8Array(64), row?.prev ?? new Uint8Array(32)]);
  }
  private storeList(user: string, version: bigint, blob: Uint8Array, sig: Uint8Array, prev: Uint8Array): void {
    this.deviceLists.set(user, { version, blob });
    const rows = this.listHistory.get(user) ?? [];
    rows.push({ version, blob, sig, prev });
    this.listHistory.set(user, rows);
    const list = readFakeList(blob);
    if (list === null) return;
    for (const entry of list.entries) {
      const device = toHex(entry.deviceId);
      if (entry.revokedAt !== null && this.devices.get(device) === user && !this.revoked.has(device)) {
        this.revoked.set(device, this.nowS);
        this.dropTokens(device);
      }
    }
  }
  private dropTokens(device: string): void {
    for (const [token, id] of this.tokens) if (id === device) {
      this.tokens.delete(token);
      this.tokenScope.delete(token);
    }
  }
  private backup(user: string, method: string, kind: number, chunk: number, body: Uint8Array): Response {
    if (method === 'DELETE') return refuse(501, 'E_INTERNAL');
    if ((kind !== 0 && kind !== 1) || chunk !== 0) return refuse(404, 'E_NOT_FOUND');
    const key = `${user}/${kind}`;
    const existing = this.backups.get(key);
    if (method === 'GET') return existing === undefined ? refuse(404, 'E_NOT_FOUND') : reply(200, [existing.object, existing.created]);
    // The upload meter before a byte is read: the reservation is the body cap or the body's length, whichever is
    // smaller, and every byte read spends the day's budget whatever happens to the object (security review F3).
    const metered = this.meter(user, Math.min(body.length, kind === 0 ? 256 : 1_048_640));
    if (metered !== null) return metered;
    let object: Uint8Array;
    try {
      const a = arr(decode(body), 1);
      object = bin(a[0] ?? null);
      const header = arr(decode(object), 3);
      if (header[0] !== 1n || bin(header[1] ?? null, 12).length !== 12 || !(header[2] instanceof Uint8Array) ||
          (kind === 0 && object.length !== 103)) throw new Error('shape');
    } catch { return refuse(400, 'E_INVALID_REQUEST', 'object is not a stored header object'); }
    // The server's order: the tombstone gate, then inside the recording transaction the file check, the
    // attachment overlap and the root's conflict.
    const blobHex = createHash('sha256').update(object).digest('hex');
    if (this.prunedBytes.has(blobHex)) return refuse(410, 'E_PRUNED', 'these bytes were removed by the server operator');
    if (this.bytesDeletedUnderNextUpload.delete(blobHex))
      return refuse(503, 'E_UNAVAILABLE', 'the stored bytes were removed during the upload; send them again', 100);
    if (this.attachmentBytes.has(blobHex))
      return refuse(409, 'E_INVALID_REQUEST', 'these bytes are an attachment; a backup object cannot share them');
    if (kind === 0 && existing !== undefined && !same(existing.object, object))
      return refuse(409, 'E_INVALID_REQUEST', 'root object already stored');
    const created = existing === undefined;
    if (created || kind === 1) this.backups.set(key, { object, created: BigInt(this.nowS) });
    return reply(created ? 201 : 200, [new Uint8Array(createHash('sha256').update(object).digest()), object.length]);
  }
}

/** The fake gateway socket (Bot v2): a request frame for a route in SOCKET_ROUTES is answered by the model as the
 *  session the connection identified with answers it (the token the connection holds), and the answer carries a status
 *  and a body only. There is no reauthentication on a socket: a 401 is the revocation, and reaches the caller. */
export function fakeSocketFor(server: FakeServer, auth: { token(): string | null }): SocketTransport {
  return {
    up: true,
    request: async (r) => {
      const token = auth.token();
      const headers = new Headers();
      if (token !== null) headers.set('Authorization', `Bearer ${token}`);
      if (r.body !== undefined) headers.set('Content-Type', 'application/cbor');
      const response = await server.fetch('http://127.0.0.1:8453' + r.path, { method: r.method, headers, body: r.body?.slice() });
      return { status: response.status, body: new Uint8Array(await response.arrayBuffer()) };
    },
  };
}

export function routesFor(server: FakeServer, auth: { token(): string | null; reauthenticate(): Promise<boolean> }): Routes {
  const socket = fakeSocketFor(server, auth);
  return new Routes(new HttpClient({
    baseUrl: 'http://127.0.0.1:8453',
    fetch: server.fetch,
    socket: () => socket,
    now: () => NOW_S * 1000,
    sleep: () => Promise.resolve(),
    random: () => 0,
    token: () => auth.token(),
    reauthenticate: () => auth.reauthenticate(),
    onGeneration: () => undefined,
  }));
}

/** Routes whose token and 401 recovery come from a Session over `core`, as the worker wires them. */
export function sessionFor(server: FakeServer, core: CorePort): { routes: Routes; session: Session } {
  const holder: { session: Session | null } = { session: null };
  const routes = routesFor(server, {
    token: () => holder.session?.token() ?? null,
    reauthenticate: () => holder.session?.establish() ?? Promise.resolve(false),
  });
  const session = new Session({ core, routes, now: () => NOW_S * 1000 });
  holder.session = session;
  return { routes, session };
}
