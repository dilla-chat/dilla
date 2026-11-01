/** The account half of CorePort for unit tests: identity phases, signup, the session record, the device list flags, KeyPackages, pause/resume/close. Every group, commit, send, outbox and timeline method throws CoreError('E_CORE_STATE', 'not modelled'); the group half is ModelCore (src/sync/testing/model.ts), held to the Rust core by L-CORE-10. Not exported from src/index.ts. */
import { arr, bin, decode, encode, u64 } from '../cbor';
import { CoreError, type CorePort, type Id, type IdentityInfo, type SessionRecord, type GroupInfo, type ExpectedGroup,
  type WelcomeOutcome, type ApplyResult, type OutboxRow, type TimelineRow, type ActivityRow, type OwnDeviceList,
  type SealedObjects, type SignedLists, type AttachmentDescriptor, type PinRow, type PurgeRow, type SendRequest, type ThreadRow,
  type PendingLeaf, type TrustRecord, type SearchHit, type CallMediaEpoch, type HumanInteractionEventPage,
  type HumanInteractionEvent } from '../core-port';
import { toHex } from '../hex';
import { entryKey, fakeDskPub, fakeListBlob, fakeListNames, readFakeList, type FakeList } from './fake-list';

export const FAKE_RECOVERY_KEY = '0123456789ABCDEFGHJKMNPQRSTVWXYZ0123456789ABCDEFGHJK';
export const FAKE_ROOT_SEALED = encode([1, new Uint8Array(12).fill(0x31), new Uint8Array(86).fill(0x32)]);
export const FAKE_STATE_SEALED = encode([1, new Uint8Array(12).fill(0x33), new Uint8Array(40).fill(0x34)]);
const REMADE_STATE = encode([1, new Uint8Array(12).fill(0x35), new Uint8Array(40).fill(0x36)]);
/** A sealed state object whose list the fake can read: [1, nonce, ['fake.state', list PUT body]] (BACKUPS-RECOVERY-02).
 *  Any other readable state is opaque to the fake, as before. */
export function fakeStateOf(listBody: Uint8Array): Uint8Array {
  return encode([1, new Uint8Array(12).fill(0x37), encode(['fake.state', listBody])]);
}
function fakeStateList(state: Uint8Array): Uint8Array | null {
  try {
    const inner = decode(bin(arr(decode(state), 3)[2] ?? null));
    if (!Array.isArray(inner) || inner.length !== 2 || inner[0] !== 'fake.state' || !(inner[1] instanceof Uint8Array)) return null;
    return inner[1];
  } catch { return null; }
}
type StoredList = { version: bigint; blob: Uint8Array; body: Uint8Array };
const same = (a: Uint8Array, b: Uint8Array): boolean => a.length === b.length && a.every((x, i) => x === b[i]);
function fold(value: string): string {
  return value.replace(/[ \t\n\r\-\u2013\u2014]/g, '').toUpperCase().replace(/[ILO]/g, (x) => x === 'O' ? '0' : '1');
}

export class FakeCore implements CorePort {
  pendingLeaves(): PendingLeaf[] { this.enter('pendingLeaves'); return []; }
  proposedAdds(): { index: bigint; userId: Id; cachedVersion: bigint }[] { this.enter('proposedAdds'); return []; }
  deviceListIngestAdd(): { version: bigint; changed: boolean; quarantined: number } { this.enter('deviceListIngestAdd'); throw new CoreError('E_CORE_STATE', 'not modelled'); }
  deviceListIngest(): { version: bigint; changed: boolean; quarantined: number } { this.enter('deviceListIngest'); throw new CoreError('E_CORE_STATE', 'not modelled'); }
  accountKindIngest(): void { this.enter('accountKindIngest'); }
  revalidatePending(): number { this.enter('revalidatePending'); return 0; }
  trustState(): TrustRecord[] { this.enter('trustState'); return []; }
  trustGeneration(): bigint { this.enter('trustGeneration'); return 0n; }
  trustVerify(): void { this.enter('trustVerify'); throw new CoreError('E_CORE_STATE', 'not modelled'); }
  safetyNumberFor(): string { this.enter('safetyNumberFor'); throw new CoreError('E_CORE_STATE', 'not modelled'); }
  sendBlockCode(): null { this.enter('sendBlockCode'); throw new CoreError('E_CORE_STATE', 'not modelled'); }
  sendBlockOwn(): boolean { this.enter('sendBlockOwn'); throw new CoreError('E_CORE_STATE', 'not modelled'); }
  search(): SearchHit[] { this.enter('search'); return []; }
  readonly calls: string[] = [];
  private readonly failures = new Map<keyof CorePort, CoreError>();
  readonly tier: number;
  private readonly idSeed: number;
  private counter = 1;
  private preferredDevice: Id | undefined;
  private info: IdentityInfo = { phase: 0, instanceId: null, userId: null, deviceId: null, username: '', listPublished: false };
  private listBody: Uint8Array | null = null;
  private accepted: StoredList | null = null;
  private candidateVersion = 0n;
  private sealed: SealedObjects = { root: null, state: null, stateUploaded: false };
  /** The list version inside the own sealed state object, as the core's `state_list` record keeps it. */
  private stateList: bigint | null = null;
  /** The sealed state a revocation's candidate replaced (the core's `state_prior`, fix-wave review NEW-2). */
  private prior: { sealed: SealedObjects; stateList: bigint | null } | null = null;
  private readonly settingsMap = new Map<string, string>();
  private record: SessionRecord | null = null;
  instanceSignerPub: Uint8Array | null = null;
  private paused = false;
  private closed = false;

  constructor(opts: { deviceId?: Id; tier?: number; idSeed?: number } = {}) {
    this.preferredDevice = opts.deviceId;
    this.tier = opts.tier ?? 1;
    this.idSeed = opts.idSeed ?? 0xf0;
  }
  static identified(o: { instanceId: Id; userId: Id; deviceId: Id; username: string; tier?: number; listPublished?: boolean; stateUploaded?: boolean;
    listEntries?: { deviceId: Id; revokedAt: bigint | null }[] }): FakeCore {
    const core = new FakeCore({ deviceId: o.deviceId, tier: o.tier });
    core.info = { phase: 2, instanceId: o.instanceId, userId: o.userId, deviceId: o.deviceId,
      username: o.username, listPublished: o.listPublished ?? true };
    core.listBody = o.listEntries === undefined ? core.makeList(o.userId, o.deviceId, 0n)
      : encode([1, fakeListBlob(o.userId, o.listEntries, 0n), new Uint8Array(64).fill(5), new Uint8Array(32)]);
    core.accepted = core.readBody(core.listBody);
    core.candidateVersion = 1n;
    core.sealed = { root: FAKE_ROOT_SEALED, state: FAKE_STATE_SEALED, stateUploaded: o.stateUploaded ?? false };
    core.stateList = core.accepted.version;
    return core;
  }
  failOnce(method: keyof CorePort, error: CoreError): void { this.failures.set(method, error); }
  private enter(method: keyof CorePort): void {
    this.calls.push(method);
    const error = this.failures.get(method);
    if (error !== undefined) { this.failures.delete(method); throw error; }
    if (this.paused || this.closed) throw new CoreError('E_STORE_PAUSED', 'the store is paused');
  }
  private nextId(): Id {
    const id = new Uint8Array(16);
    id[0] = this.idSeed;
    new DataView(id.buffer).setUint32(12, this.counter++);
    return id;
  }
  private requirePending(): void {
    if (this.info.phase !== 1) throw new CoreError('E_CORE_STATE', this.info.phase === 2 ? 'the identity is complete' : 'no signup is pending');
  }
  private requireIdentity(): void {
    if (this.info.phase !== 2) throw new CoreError('E_CORE_NO_IDENTITY');
  }
  private makeList(userId: Id, deviceId: Id, now: bigint): Uint8Array {
    return encode([1, encode(['fake.list', userId, deviceId, now]), new Uint8Array(64).fill(5), new Uint8Array(32)]);
  }
  private readBody(body: Uint8Array): StoredList {
    try {
      const a = arr(decode(body), 4);
      const version = u64(a[0] ?? null);
      const blob = bin(a[1] ?? null);
      bin(a[2] ?? null, 64); bin(a[3] ?? null, 32);
      return { version, blob, body };
    } catch { throw new CoreError('E_CORE_INPUT', 'list_body is malformed'); }
  }
  private served(body: Uint8Array, user: Id, stored: StoredList | null = null, state: Uint8Array = FAKE_STATE_SEALED):
    { row: StoredList; list: FakeList; interrupted: Uint8Array | null } {
    let row = this.readBody(body);
    let list = readFakeList(row.blob);
    if (list === null || !same(list.userId, user)) throw new CoreError('E_CREDENTIAL');
    if (row.version === 0n || (row.version === 1n && bin(arr(decode(body), 4)[3] ?? null).some((x) => x !== 0)))
      throw new CoreError('E_DEVICE_LIST_STALE');
    if (state.length === 0 && row.version !== 1n) throw new CoreError('E_CORE_INPUT', 'the backup state is missing');
    if (state.length > 0 && !this.readableState(state) && row.version !== 1n)
      throw new CoreError('E_CORE_INPUT', 'the backup state could not be read');
    // BACKUPS-RECOVERY-02 as the fake models it: a state object whose list is the served list's successor for the
    // same user is an interrupted publication and becomes the base (the chain and the signature are not modelled);
    // a state list further on is the floor's older-list refusal.
    let interrupted: Uint8Array | null = null;
    const inner = fakeStateList(state);
    if (inner !== null) {
      const next = this.readBody(inner);
      const nextList = readFakeList(next.blob);
      if (next.version === row.version + 1n && nextList !== null && same(nextList.userId, user)) {
        row = next;
        list = nextList;
        interrupted = inner;
      } else if (next.version > row.version) throw new CoreError('E_CORE_INPUT', 'the instance served an older device list');
    }
    if (stored !== null && row.version === stored.version && !same(row.blob, stored.blob))
      throw new CoreError('E_CORE_INPUT', 'the instance served a different device list at the stored version');
    if (stored !== null && row.version < stored.version)
      throw new CoreError('E_CORE_INPUT', 'the instance served an older device list');
    return { row, list, interrupted };
  }
  private readableState(state: Uint8Array): boolean {
    try {
      const a = arr(decode(state), 3);
      return a[0] === 1n && bin(a[1] ?? null, 12).length === 12 && bin(a[2] ?? null).length > 0;
    } catch { return false; }
  }
  private candidate(user: Id, entries: FakeList['entries'], now: bigint, version: bigint): Uint8Array {
    const blob = fakeListBlob(user, entries, now);
    this.candidateVersion = version;
    return encode([version, blob, new Uint8Array(64).fill(5), new Uint8Array(32).fill(0x70)]);
  }
  pause(): void { this.enter('pause'); this.paused = true; }
  resume(): Promise<void> {
    this.calls.push('resume');
    const error = this.failures.get('resume');
    if (error !== undefined) { this.failures.delete('resume'); return Promise.reject(error); }
    if (this.closed) return Promise.reject(new CoreError('E_STORE_PAUSED', 'the store is paused'));
    this.paused = false;
    return Promise.resolve();
  }
  close(): void { this.enter('close'); this.closed = true; }
  identity(): IdentityInfo { this.enter('identity'); return { ...this.info }; }
  signupBegin(instanceId: Id): string {
    this.enter('signupBegin');
    if (this.info.phase !== 0) throw new CoreError('E_CORE_STATE', this.info.phase === 1 ? 'a signup is pending' : 'an identity exists');
    this.info = { phase: 1, instanceId, userId: null, deviceId: this.preferredDevice ?? this.nextId(), username: '', listPublished: false };
    this.sealed = { root: FAKE_ROOT_SEALED, state: null, stateUploaded: false };
    return FAKE_RECOVERY_KEY;
  }
  signupRequest(invite: string, username: string, display: string, password: string | null): Uint8Array {
    this.enter('signupRequest'); this.requirePending();
    const deviceId = this.info.deviceId;
    if (deviceId === null) throw new CoreError('E_CORE_NO_IDENTITY');
    return encode([invite, username, display, new Uint8Array(32).fill(1), new Uint8Array(32).fill(2),
      new Uint8Array(64).fill(3), password, [deviceId, fakeDskPub(deviceId), 1, 1,
        encode(['fake.cred', new Uint8Array(16), deviceId])]]);
  }
  signupComplete(userId: Id, username: string, now: bigint, instanceSignerPub: Uint8Array): Uint8Array {
    this.enter('signupComplete'); this.requirePending();
    const deviceId = this.info.deviceId;
    if (deviceId === null) throw new CoreError('E_CORE_NO_IDENTITY');
    this.listBody = this.makeList(userId, deviceId, now);
    this.accepted = this.readBody(this.listBody);
    this.candidateVersion = 1n;
    this.sealed = { root: FAKE_ROOT_SEALED, state: FAKE_STATE_SEALED, stateUploaded: false };
    this.stateList = 1n;
    this.info = { ...this.info, phase: 2, userId, username, listPublished: false };
    this.instanceSignerPub = instanceSignerPub.slice();
    return this.listBody;
  }
  instanceSignerKeyMatches(observedKey: Uint8Array): void {
    this.enter('instanceSignerKeyMatches');
    if (this.instanceSignerPub === null) throw new CoreError('E_CORE_STATE', 'instance signing key is not pinned');
    if (!same(this.instanceSignerPub, observedKey)) throw new CoreError('E_CORE_STATE', 'instance external-sender key changed');
  }
  instanceSignerKeyPin(instanceId: Id, observedKey: Uint8Array): void {
    this.enter('instanceSignerKeyPin');
    if (this.info.phase !== 2 || this.info.instanceId === null) throw new CoreError('E_CORE_STATE', 'no identity');
    if (!same(this.info.instanceId, instanceId)) throw new CoreError('E_CORE_STATE', 'authenticated instance discovery does not match identity');
    if (this.instanceSignerPub !== null && !same(this.instanceSignerPub, observedKey))
      throw new CoreError('E_CORE_STATE', 'instance external-sender key changed');
    this.instanceSignerPub = observedKey.slice();
  }
  signupReset(): void {
    this.enter('signupReset'); this.requirePending();
    if (this.record !== null) throw new CoreError('E_CORE_STATE', 'the account is registered');
    this.info = { phase: 0, instanceId: null, userId: null, deviceId: null, username: '', listPublished: false };
    this.listBody = null;
    this.accepted = null;
    this.sealed = { root: null, state: null, stateUploaded: false };
    this.prior = null;
  }
  deviceListBody(): Uint8Array {
    this.enter('deviceListBody'); this.requireIdentity();
    if (this.listBody === null) throw new CoreError('E_CORE_STATE');
    return this.listBody;
  }
  deviceListPublished(): void { this.enter('deviceListPublished'); this.requireIdentity(); this.info.listPublished = true;
    this.prior = null;
    if (this.listBody !== null) this.accepted = this.readBody(this.listBody); }
  /** The candidate becomes the accepted list's own body, unpublished (the core's device_list_drop); its sealed state
   *  goes with it and the one from before the revocation is restored (fix-wave review NEW-2). */
  deviceListDrop(): void {
    this.enter('deviceListDrop'); this.requireIdentity();
    if (this.accepted === null) throw new CoreError('E_CORE_STATE');
    this.listBody = this.accepted.body;
    this.candidateVersion = this.accepted.version;
    this.info.listPublished = false;
    this.restorePrior();
  }
  private restorePrior(): void {
    if (this.prior === null) return;
    this.sealed = { ...this.sealed, state: this.prior.sealed.state, stateUploaded: this.prior.sealed.stateUploaded };
    this.stateList = this.prior.stateList;
    this.prior = null;
  }
  sessionSign(nonce: Uint8Array, purpose: 0 | 1): Uint8Array {
    this.enter('sessionSign');
    if (this.info.phase === 0) throw new CoreError('E_CORE_NO_IDENTITY');
    if (nonce.length !== 32 || (purpose !== 0 && purpose !== 1)) throw new CoreError('E_CORE_INPUT');
    return encode([nonce, purpose, new Uint8Array(64).fill(6), null, null]);
  }
  signRemoval(): Uint8Array {
    this.enter('signRemoval'); return new Uint8Array([0x82, 0x40, 0x40]);
  }
  sessionStore(s: SessionRecord): void {
    this.enter('sessionStore');
    if (this.info.phase === 0) throw new CoreError('E_CORE_NO_IDENTITY');
    this.record = { ...s };
  }
  session(): SessionRecord | null { this.enter('session'); return this.record === null ? null : { ...this.record }; }
  sessionClear(): void { this.enter('sessionClear'); this.record = null; }
  keyPackages(count: number, lastResort: boolean): Uint8Array {
    this.enter('keyPackages'); this.requireIdentity();
    if (!Number.isInteger(count) || count < 1 || count > 32) throw new CoreError('E_CORE_INPUT');
    const device = this.info.deviceId;
    if (device === null) throw new CoreError('E_CORE_NO_IDENTITY');
    const packages = Array.from({ length: count }, (_, i) => encode(['fake.kp', device, i, 0]));
    return encode([packages, lastResort ? encode(['fake.kp', device, count, 1]) : null]);
  }
  sealedObjects(): SealedObjects { this.enter('sealedObjects'); return { ...this.sealed }; }
  stateSealedUploaded(): void { this.enter('stateSealedUploaded'); this.requireIdentity(); this.sealed.stateUploaded = true; }
  stateSealedCurrent(): boolean {
    this.enter('stateSealedCurrent'); this.requireIdentity();
    return this.stateList !== null && this.stateList === this.accepted?.version;
  }
  enrolBegin(instanceId: Id): { deviceId: Id; dskPub: Uint8Array } {
    this.enter('enrolBegin');
    if (this.info.phase !== 0) throw new CoreError('E_CORE_STATE', this.info.phase === 1 ? 'a signup is pending' :
      this.info.phase === 2 ? 'an identity exists' : 'an enrolment is pending');
    const deviceId = this.preferredDevice ?? this.nextId();
    this.info = { phase: 3, instanceId, deviceId, userId: null, username: '', listPublished: false };
    return { deviceId, dskPub: fakeDskPub(deviceId) };
  }
  enrolSessionSign(nonce: Uint8Array, login: Uint8Array): Uint8Array {
    this.enter('enrolSessionSign');
    if (login.length < 1 || login.length > 256 || nonce.length !== 32) throw new CoreError('E_CORE_INPUT');
    if (this.info.phase !== 3 || this.info.deviceId === null) throw new CoreError('E_CORE_STATE', 'no enrolment is pending');
    return encode([nonce, 0, new Uint8Array(64).fill(6),
      [this.info.deviceId, fakeDskPub(this.info.deviceId), 1, 1, encode(['fake.placeholder', this.info.deviceId])], login]);
  }
  enrolRegistered(userId: Id): void {
    this.enter('enrolRegistered');
    if (userId.every((x) => x === 0)) throw new CoreError('E_CORE_INPUT', 'user_id is all zero');
    if (this.info.phase !== 3) throw new CoreError('E_CORE_STATE', 'no enrolment is pending');
    if (this.info.userId !== null && !same(this.info.userId, userId)) throw new CoreError('E_CORE_STATE', 'user already recorded');
    this.info.userId = userId;
  }
  /** The core's form check: 52 characters of the Crockford alphabet once folded (the four zero bits of the last
   *  character are not modelled: FAKE_RECOVERY_KEY ends in K). */
  recoveryKeyCheck(recoveryKey: string): void {
    this.enter('recoveryKeyCheck');
    if (!/^[0-9A-HJKMNP-TV-Z]{52}$/.test(fold(recoveryKey))) throw new CoreError('E_RECOVERY_KEY');
  }
  enrolComplete(input: { recoveryKey: string; rootSealed: Uint8Array; stateSealed: Uint8Array; listBody: Uint8Array; username: string; now: bigint }): SignedLists {
    this.enter('enrolComplete');
    if (this.info.phase !== 3 || this.info.deviceId === null) throw new CoreError('E_CORE_STATE', 'no enrolment is pending');
    if (this.info.userId === null) throw new CoreError('E_CORE_STATE', 'no user recorded');
    if (fold(input.recoveryKey) !== FAKE_RECOVERY_KEY) throw new CoreError('E_RECOVERY_KEY');
    const { row, list, interrupted } = this.served(input.listBody, this.info.userId, null, input.stateSealed);
    if (list.entries.some((entry) => same(entry.deviceId, this.info.deviceId!))) throw new CoreError('E_CORE_STATE', 'device is listed');
    this.accepted = row;
    this.listBody = this.candidate(this.info.userId,
      [...list.entries, { deviceId: this.info.deviceId, revokedAt: null }], input.now, row.version + 1n);
    this.sealed = { root: input.rootSealed, state: REMADE_STATE, stateUploaded: false };
    this.stateList = this.candidateVersion;
    this.info = { ...this.info, phase: 2, username: input.username, listPublished: false };
    return { deviceListBody: this.listBody, stateSealed: REMADE_STATE, interrupted };
  }
  enrolReset(): void {
    this.enter('enrolReset');
    if (this.info.phase !== 3) throw new CoreError('E_CORE_STATE', 'no enrolment is pending');
    this.info = { phase: 0, instanceId: null, userId: null, deviceId: null, username: '', listPublished: false };
    this.record = null;
  }
  ownDeviceList(): OwnDeviceList {
    this.enter('ownDeviceList'); this.requireIdentity();
    if (this.accepted === null || this.info.deviceId === null) throw new CoreError('E_CORE_STATE');
    const list = readFakeList(this.accepted.blob);
    if (list === null) throw new CoreError('E_CORE_STATE');
    return { version: this.accepted.version, published: this.info.listPublished, entries: list.entries.map((entry) => ({
      deviceId: entry.deviceId, dskPub: entryKey(entry),
      tier: (same(entry.deviceId, this.info.deviceId!) ? this.tier : 1) as 0 | 1,
      addedAt: list.at, revokedAt: entry.revokedAt,
    })) };
  }
  ownDeviceListUpdate(historyBody: Uint8Array): { version: bigint; listed: boolean } {
    this.enter('ownDeviceListUpdate'); this.requireIdentity();
    if (this.accepted === null || this.info.deviceId === null) throw new CoreError('E_CORE_STATE');
    const rows = arr(decode(historyBody));
    for (const raw of rows) {
      const row = this.readBody(encode(raw));
      if (row.version <= this.accepted.version) {
        if (!same(row.blob, this.accepted.blob)) throw new CoreError('E_DEVICE_LIST_STALE');
        continue;
      }
      this.accepted = row;
    }
    if (!this.info.listPublished && this.candidateVersion <= this.accepted.version) {
      // A candidate the instance holds keeps its state; one the chain overtook is dropped with it (NEW-2).
      if (this.listBody !== null && same(this.readBody(this.listBody).blob, this.accepted.blob)) this.prior = null;
      else this.restorePrior();
      this.listBody = this.accepted.body;
      this.info.listPublished = true;
    }
    const list = readFakeList(this.accepted.blob);
    if (list === null) throw new CoreError('E_CREDENTIAL');
    // The core's own judgement is the pair too: its device_id and its dsk_pub in one unrevoked entry.
    return { version: this.accepted.version, listed: fakeListNames(list, this.info.deviceId, fakeDskPub(this.info.deviceId)) };
  }
  deviceListRevoke(input: { recoveryKey: string; rootSealed: Uint8Array; stateSealed: Uint8Array; listBody: Uint8Array; deviceIds: Id[]; now: bigint }): SignedLists {
    this.enter('deviceListRevoke');
    if (input.deviceIds.length < 1 || input.deviceIds.length > 64 || input.deviceIds.some((id) => id.length !== 16))
      throw new CoreError('E_CORE_INPUT');
    this.requireIdentity();
    if (fold(input.recoveryKey) !== FAKE_RECOVERY_KEY) throw new CoreError('E_RECOVERY_KEY');
    if (this.info.userId === null) throw new CoreError('E_CORE_STATE');
    const { row, list, interrupted } = this.served(input.listBody, this.info.userId, this.accepted, input.stateSealed);
    for (const id of input.deviceIds) {
      if (!fakeListNames(list, id)) throw new CoreError('E_CORE_NOT_FOUND');
    }
    this.accepted = row;
    this.listBody = this.candidate(this.info.userId, list.entries.map((entry) => ({ ...entry,
      revokedAt: input.deviceIds.some((id) => same(id, entry.deviceId)) ? input.now : entry.revokedAt,
    })), input.now, row.version + 1n);
    this.info.listPublished = false;
    this.prior ??= { sealed: { ...this.sealed }, stateList: this.stateList };
    this.sealed = { root: this.sealed.root, state: REMADE_STATE, stateUploaded: false };
    this.stateList = this.candidateVersion;
    return { deviceListBody: this.listBody, stateSealed: REMADE_STATE, interrupted };
  }
  settings(): Record<string, string> {
    this.enter('settings');
    return Object.fromEntries([...this.settingsMap].sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0));
  }
  settingPut(k: string, v: string): void {
    this.enter('settingPut');
    const enc = new TextEncoder();
    if (enc.encode(k).length < 1 || enc.encode(k).length > 128 || enc.encode(v).length > 1024) throw new CoreError('E_CORE_INPUT');
    this.settingsMap.set(k, v);
  }
  settingDelete(k: string): void {
    this.enter('settingDelete');
    const len = new TextEncoder().encode(k).length;
    if (len < 1 || len > 128) throw new CoreError('E_CORE_INPUT');
    this.settingsMap.delete(k);
  }
  private notModelled(name: keyof CorePort, ...args: unknown[]): never {
    this.enter(name);
    void args;
    throw new CoreError('E_CORE_STATE', 'not modelled');
  }
  groups(): GroupInfo[] { return this.notModelled('groups'); }
  groupHealSnapshot(groupId: Id): Uint8Array { return this.notModelled('groupHealSnapshot', groupId); }
  groupCreate(groupId: Id, communityId: Id | null, channelId: Id, policyVersion: bigint, externalSenderPub: Uint8Array): Uint8Array { return this.notModelled('groupCreate', groupId, communityId, channelId, policyVersion, externalSenderPub); }
  groupRegistered(groupId: Id, nextSeq: bigint): void { return this.notModelled('groupRegistered', groupId, nextSeq); }
  groupDiscard(groupId: Id): void { return this.notModelled('groupDiscard', groupId); }
  groupJoinExternal(g: ExpectedGroup, infoBody: Uint8Array, treeBody: Uint8Array): Uint8Array { return this.notModelled('groupJoinExternal', g, infoBody, treeBody); }
  groupJoined(groupId: Id, seq: bigint): void { return this.notModelled('groupJoined', groupId, seq); }
  welcomesApply(welcomesBody: Uint8Array, expected: ExpectedGroup[]): WelcomeOutcome[] { return this.notModelled('welcomesApply', welcomesBody, expected); }
  humanInteractionWelcomeApply(welcomesBody: Uint8Array, currentContextProof: Uint8Array, now: bigint): WelcomeOutcome[] {
    return this.notModelled('humanInteractionWelcomeApply', welcomesBody, currentContextProof, now);
  }
  humanInteractionSendPrepare(requestId: Id, communityId: Id, groupId: Id, requestBody: Uint8Array, now: bigint): Id {
    return this.notModelled('humanInteractionSendPrepare', requestId, communityId, groupId, requestBody, now);
  }
  humanInteractionEventsRead(after: bigint, limit: number): HumanInteractionEventPage { return this.notModelled('humanInteractionEventsRead', after, limit); }
  humanInteractionReceiptsRead(communityId: Uint8Array, installationId: Uint8Array, botUserId: Uint8Array, requestIds: readonly Uint8Array[]): HumanInteractionEvent[] { return this.notModelled('humanInteractionReceiptsRead', communityId, installationId, botUserId, requestIds); }
  humanInteractionEventsAck(through: bigint): void { return this.notModelled('humanInteractionEventsAck', through); }
  humanInteractionEventsAckSelected(deliveryIds: readonly bigint[]): void { return this.notModelled('humanInteractionEventsAckSelected', deliveryIds); }
  groupApply(groupId: Id, handshakes: Uint8Array, messages: Uint8Array, through: bigint): ApplyResult { return this.notModelled('groupApply', groupId, handshakes, messages, through); }
  commitBuild(groupId: Id, proposalsBody: Uint8Array): Uint8Array { return this.notModelled('commitBuild', groupId, proposalsBody); }
  commitConfirm(groupId: Id): ApplyResult { return this.notModelled('commitConfirm', groupId); }
  commitAbort(groupId: Id): void { return this.notModelled('commitAbort', groupId); }
  cursorBody(groupId: Id): Uint8Array | null { return this.notModelled('cursorBody', groupId); }
  cursorAcked(groupId: Id, lastSeq: bigint, lastEpoch: bigint): void { return this.notModelled('cursorAcked', groupId, lastSeq, lastEpoch); }
  messageDeleted(groupId: Id, seq: bigint): ApplyResult { return this.notModelled('messageDeleted', groupId, seq); }
  removalCursor(groupId: Id): bigint { return this.notModelled('removalCursor', groupId); }
  removalActorHistoryVersion(groupId: Id, entry: import('../core-port').RemovalFeedEntry): bigint { return this.notModelled('removalActorHistoryVersion', groupId, entry); }
  refreshRemovalActorHistory(groupId: Id, entry: import('../core-port').RemovalFeedEntry, history: Uint8Array, headList: Uint8Array): bigint { return this.notModelled('refreshRemovalActorHistory', groupId, entry, history, headList); }
  ingestRemoval(groupId: Id, entry: import('../core-port').RemovalFeedEntry): bigint { return this.notModelled('ingestRemoval', groupId, entry); }
  sendPrepare(groupId: Id, request: SendRequest, now: bigint): Id { return this.notModelled('sendPrepare', groupId, request, now); }
  sendEncrypt(msgId: Id): { groupId: Id; messageBody: Uint8Array } { return this.notModelled('sendEncrypt', msgId); }
  sendConfirm(msgId: Id, response: Uint8Array): { groupId: Id; seq: bigint } { return this.notModelled('sendConfirm', msgId, response); }
  sendRequeue(msgId: Id): void { return this.notModelled('sendRequeue', msgId); }
  sendFail(msgId: Id, error: string): void { return this.notModelled('sendFail', msgId, error); }
  sendRetry(msgId: Id): void { return this.notModelled('sendRetry', msgId); }
  sendDiscard(msgId: Id): void { return this.notModelled('sendDiscard', msgId); }
  outbox(groupId: Id): OutboxRow[] { return this.notModelled('outbox', groupId); }
  timeline(groupId: Id, beforeSeq: bigint, limit: number): TimelineRow[] { return this.notModelled('timeline', groupId, beforeSeq, limit); }
  threadTimeline(groupId: Id, rootMsgId: Id, beforeSeq: bigint, limit: number): { root: TimelineRow | null; rows: TimelineRow[] } {
    return this.notModelled('threadTimeline', groupId, rootMsgId, beforeSeq, limit);
  }
  threads(groupId: Id): ThreadRow[] { return this.notModelled('threads', groupId); }
  groupRow(groupId: Id): GroupInfo | null { return this.notModelled('groupRow', groupId); }
  markRead(groupId: Id, seq: bigint, now: bigint): void { return this.notModelled('markRead', groupId, seq, now); }
  activity(): ActivityRow[] { return this.notModelled('activity'); }
  pins(groupId: Id): PinRow[] { return this.notModelled('pins', groupId); }
  attachmentGet(groupId: Id, seq: bigint, index: number): AttachmentDescriptor { return this.notModelled('attachmentGet', groupId, seq, index); }
  purges(): PurgeRow[] { return this.notModelled('purges'); }
  purgeDone(groupId: Id, seq: bigint): void { return this.notModelled('purgeDone', groupId, seq); }
  ownRolesSet(communityId: Id, roleIds: Id[]): void { return this.notModelled('ownRolesSet', communityId, roleIds); }
  /** web3.5 R1: every authoritySet call, in hex, in call order (C4 reads it). */
  readonly authoritySets: { groupId: string; pinners: string[]; broadcasters: string[]; mentionable: string[] }[] = [];
  authoritySet(groupId: Id, pinners: Id[], broadcasters: Id[], mentionable: Id[]): void {
    this.enter('authoritySet');
    this.authoritySets.push({ groupId: toHex(groupId), pinners: pinners.map(toHex), broadcasters: broadcasters.map(toHex),
      mentionable: mentionable.map(toHex) });
  }
  // web-3 call groups (L-TS-55) are group-half methods: ModelCore models them (lesson f); here they are not modelled.
  callGroupCreate(groupId: Id, communityId: Id | null, channelId: Id, policyVersion: bigint, externalSenderPub: Uint8Array): Uint8Array {
    return this.notModelled('callGroupCreate', groupId, communityId, channelId, policyVersion, externalSenderPub);
  }
  callGroupJoinExternal(g: ExpectedGroup, infoBody: Uint8Array, treeBody: Uint8Array): Uint8Array { return this.notModelled('callGroupJoinExternal', g, infoBody, treeBody); }
  callMediaEpoch(groupId: Id): CallMediaEpoch { return this.notModelled('callMediaEpoch', groupId); }
  callJoinCheck(groupId: Id): void { return this.notModelled('callJoinCheck', groupId); }
  callLeaveProposal(groupId: Id): Uint8Array { return this.notModelled('callLeaveProposal', groupId); }
  callRotate(groupId: Id): Uint8Array { return this.notModelled('callRotate', groupId); }
  callGroupForget(groupId: Id): void { return this.notModelled('callGroupForget', groupId); }
}
