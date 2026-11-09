// Bot v2 (task 20): the community Bot's command catalog, held on this device. The picker reads only this copy; the network
// is asked for it once on connect when the instance's revision is newer than the held one (GET /v1/bot/revisions, then the
// catalog GET), and op 61 bot_catalog replaces it live. The catalog is not secret (names and argument shapes only); it is
// display only: the Bot authorises every operation itself. Its command-key publication is checked against the Bot user's
// device list in the core before anything is sealed to it.
import { arr, bin, decode, str, u53, u64, type CborValue } from '../cbor';
import { fromHex, toHex } from '../hex';
import type { BotCatalogFrame } from '../gateway/frames';

/** The instance's states, plus two of this device's own: `unverified`, a published key this device cannot check (or more
 *  than one Bot account to check it against), and `changed`, a Bot account other than the one this device pinned. */
export type BotState = 'ready' | 'degraded' | 'offline' | 'disabled' | 'absent' | 'unverified' | 'changed';
/** Argument type codes: 0 text, 1 uint, 2 user, 3 channel, 4 duration (uint seconds), 5 bool (uint 0|1). */
export type ArgType = 0 | 1 | 2 | 3 | 4 | 5;
export interface ArgShape { name: string; type: ArgType; required: boolean }
export interface BotCommandSpec { name: string; description: string; operation: number; args: ArgShape[]; permissionBit: number;
  needsBotChannel: boolean; handlerKind: number }
export interface BotStatus { state: BotState; since: number | null }
export interface BotCatalog {
  revision: bigint;
  commands: BotCommandSpec[];
  /** The verified command key's id; null with no publication or one this device could not check (commands greyed). */
  keyId: Uint8Array | null;
  /** The verified key's fingerprint, for admins (the Bot page, task 22). */
  fingerprint: string | null;
  /** The instance's status, except where this device's own check says `unverified` or `changed`. */
  status: BotStatus;
  /** The Bot channels this member can view (hex ids); null until read. A command that needs one is greyed without one. */
  botChannels: string[] | null;
  /** The Bot user this device pinned for the community (hex), the key's signer; null while unknown. */
  botUserId: string | null;
}
/** One kind/scope/revision of GET /v1/bot/revisions; kind = 3 is a community's catalog (scope_id the community). */
export interface BotRevision { kind: number; scopeId: string; revision: bigint }

/** The slice view of a catalog (state/types `botCatalog:<community>`): no bigint and no bytes. */
export interface BotCatalogView { revision: string; commands: BotCommandSpec[]; keyId: string | null; fingerprint: string | null;
  status: BotStatus; botChannels: string[] | null; botUserId: string | null }

export const CATALOG_KIND = 3;
const STATES: readonly BotState[] = ['ready', 'degraded', 'offline', 'disabled', 'absent'];
const MAX_COMMANDS = 256;
const MAX_ARGS = 16;
const MAX_PUBLICATION = 256;
/** A command name the picker can show and parse: lower-case words of letters, digits and hyphens, at most 32 bytes. */
const COMMAND_NAME = /^[a-z0-9][a-z0-9-]*(?: [a-z0-9][a-z0-9-]*)*$/u;
/** An argument name: one such word (it is a chip label and a `name:` key). */
const ARG_NAME = /^[a-z0-9][a-z0-9_-]{0,31}$/u;
const utf8 = new TextEncoder();
const bytes = (s: string): number => utf8.encode(s).length;
/** A held catalog or publication the core refused this way may name another Bot account: the Bot user is looked up again. */
const SIGNER_REFUSALS: ReadonlySet<string> = new Set(['E_BOT_SIGNATURE', 'E_DEVICE_UNLISTED', 'E_CREDENTIAL']);

class CatalogError extends Error {
  readonly code = 'E_BOT_CATALOG';
  constructor(detail: string) { super(`E_BOT_CATALOG: ${detail}`); }
}
const refuse = (detail: string): never => { throw new CatalogError(detail); };
const bit = (v: CborValue): boolean => { const n = u53(v); if (n !== 0 && n !== 1) refuse('flag'); return n === 1; };

function readArg(value: CborValue): ArgShape {
  const [name, type, required] = arr(value, 3);
  const n = str(name);
  if (bytes(n) < 1 || bytes(n) > 32) refuse('argument name');
  const t = u53(type);
  if (t > 5) refuse('argument type');
  return { name: n, type: t as ArgType, required: bit(required) };
}

function readCommand(value: CborValue): BotCommandSpec {
  const [name, description, operation, shape, permission, needs, handler] = arr(value, 7);
  const n = str(name); const d = str(description);
  if (bytes(n) < 1 || bytes(n) > 64 || bytes(d) > 512) refuse('name or description');
  const args = arr(shape).map(readArg);
  if (args.length > MAX_ARGS || new Set(args.map(a => a.name)).size !== args.length) refuse('arguments');
  return { name: n, description: d, operation: u53(operation), args, permissionBit: u53(permission), needsBotChannel: bit(needs),
    handlerKind: u53(handler) };
}

/** Whether the picker can show a command: its name and its argument names in the picker's grammar. */
const showable = (c: BotCommandSpec): boolean => bytes(c.name) <= 32 && COMMAND_NAME.test(c.name) && c.args.every(a => ARG_NAME.test(a.name));

/** A catalog array, read whole or refused (at most 256 commands with unique names); a command whose name or argument names
 *  are outside the picker's grammar is dropped, since the names reach chips, ids and the parser. */
export function decodeBotCommands(values: readonly CborValue[]): BotCommandSpec[] {
  if (values.length > MAX_COMMANDS) refuse('too many commands');
  const commands = values.map(readCommand);
  if (new Set(commands.map(c => c.name)).size !== commands.length) refuse('duplicate name');
  return commands.filter(showable);
}

function readPublication(value: CborValue): Uint8Array | null {
  if (value === null) return null;
  const p = bin(value);
  if (p.length === 0 || p.length > MAX_PUBLICATION) refuse('publication');
  return p;
}

/** The community a publication `[1, community_id, key_id, …]` names, or null when it is no publication. */
function publicationCommunity(publication: Uint8Array): string | null {
  try {
    const a = arr(decode(publication));
    return a.length >= 2 && a[0] === 1n && a[1] instanceof Uint8Array && a[1].length === 16 ? toHex(a[1]) : null;
  } catch { return null; }
}

/** GET /v1/communities/{community}/bot/catalog → [revision, commands, publication | null, [state, since | null]]. */
export function decodeBotCatalogResponse(body: Uint8Array): { revision: bigint; commands: BotCommandSpec[]; publication: Uint8Array | null; status: BotStatus } {
  const [revision, commands, publication, status] = arr(decode(body), 4);
  const [state, since] = arr(status, 2);
  const s = u53(state);
  if (s > 4) refuse('state');
  return { revision: u64(revision), commands: decodeBotCommands(arr(commands)), publication: readPublication(publication),
    status: { state: STATES[s], since: since === null ? null : u53(since) } };
}

/** GET /v1/bot/revisions → [[kind, scope_id, revision]…]. */
export function decodeBotRevisions(body: Uint8Array): BotRevision[] {
  return arr(decode(body)).map(row => { const [kind, scope, revision] = arr(row, 3); return { kind: u53(kind), scopeId: toHex(bin(scope, 16)), revision: u64(revision) }; });
}

/** GET /v1/communities/{community}/bot/channels → [[channel_id, marked_at, marked_by]…], filtered by the instance to the
 *  channels this member can view; only the ids are kept. */
export function decodeBotChannels(body: Uint8Array): string[] {
  return arr(decode(body)).map(row => toHex(bin(arr(row, 3)[0], 16)));
}

/** What the server names as a community's Bot user: one id, none, or more than one kind-1 account (refused). */
export type BotUserCandidate = Uint8Array | null | 'ambiguous';

export interface BotCatalogDeps {
  fetchCatalog(communityId: string): Promise<Uint8Array>;
  fetchBotChannels(communityId: string): Promise<Uint8Array>;
  fetchRevisions(): Promise<Uint8Array>;
  /** The server's Bot user for the community (the key's signer). Pinned on first sight; a later different one is never used. */
  botUserId(communityId: string): Promise<BotUserCandidate>;
  /** CorePort.botCommandKeyVerify: throws the refusal (E_BOT_SIGNATURE, E_CREDENTIAL, E_DEVICE_LIST_STALE, ...). */
  verify(publication: Uint8Array, botUserId: Uint8Array): { keyId: Uint8Array; fingerprint: string };
  /** CorePort.botUserPin (task 20b): the community's Bot user, pinned in the core store on first sight. State 2 is
   *  another user than the pinned one, which stays; only sign-out (a store wipe) clears a pin. Throws E_CREDENTIAL for an
   *  account the instance attested as a human. */
  pinBotUser(communityId: string, botUserId: Uint8Array): { state: 0 | 1 | 2; pinned: Uint8Array };
  /** CorePort.botUserPinned: the stored pin, or null. */
  pinnedBotUser(communityId: string): Uint8Array | null;
  /** Task 20b review I1: reads the candidate's device-list head and ingests its account-kind attestation, so the core can
   *  tell a Bot from a human before anything is pinned. Asked before a first pin, at most once per connection. */
  ingestBotAttestation(botUserId: Uint8Array): Promise<void>;
  /** Task 20b (Jonas, 2026-11-07): reads the Bot user's device list (the head with its identity keys, the history from
   *  version 1), builds the device_list_push and pins it in the core (CorePort.botDeviceListPin). Resolves the pin state:
   *  0 current, 1 a key change held, 2 behind. Asked at most once per connection per Bot user, on E_DEVICE_LIST_STALE. */
  pinBotDeviceList(botUserId: Uint8Array): Promise<0 | 1 | 2>;
  /** A catalog read or a key check that failed, by code (diagnostics); never its content. */
  onError?(code: string): void;
}

interface Entry {
  revision: bigint;
  commands: BotCommandSpec[];
  publication: Uint8Array | null;
  /** The publication the core accepted; null when none was, so nothing is sealed. */
  verified: { publication: Uint8Array; keyId: Uint8Array; fingerprint: string } | null;
  /** The core's last refusal of the publication (E_UMK_CHANGED greys it `changed`); null after an accept. */
  refusal?: string | null;
  serverStatus: BotStatus;
  botChannels: string[] | null;
}

const codeOf = (e: unknown): string => (typeof e === 'object' && e !== null && 'code' in e && typeof e.code === 'string' ? e.code : 'E_BOT_CATALOG');

export class BotCatalogStore {
  private readonly entries = new Map<string, Entry>();
  private readonly listeners = new Map<string, Set<() => void>>();
  private revisions: Promise<BotRevision[] | null> | null = null;
  private readonly inFlight = new Map<string, Promise<void>>();
  /** community → the Bot user this device pinned on first sight (hex): a copy of the core store's pin (task 20b), which
   *  survives a reload and is never replaced. */
  private readonly pins = new Map<string, string>();
  /** Bot users (hex) whose device list was fetched and pinned on this connection (task 20b: at most once per connect). */
  private readonly listFetched = new Set<string>();
  /** Communities whose first pin the core refused on this connection (no Bot attestation, or a human's): not asked again
   *  until the next connection. */
  private readonly pinRefused = new Set<string>();
  /** Bot users (hex) whose pinned device list holds a key change (botDeviceListPin state 1): greyed `changed`. */
  private readonly listHeld = new Set<string>();
  /** community → why its Bot user cannot be used: the server named another one, or more than one. */
  private readonly botProblem = new Map<string, 'changed' | 'ambiguous'>();
  private readonly resolving = new Map<string, Promise<void>>();

  constructor(private readonly deps: BotCatalogDeps) {}

  get(communityId: string): BotCatalog | null {
    const e = this.entries.get(communityId);
    if (e === undefined) return null;
    return { revision: e.revision, commands: e.commands, keyId: e.verified?.keyId ?? null, fingerprint: e.verified?.fingerprint ?? null,
      status: this.statusOf(communityId, e), botChannels: e.botChannels, botUserId: this.pinOf(communityId) ?? null };
  }

  /** The slice form of get(). */
  view(communityId: string): BotCatalogView | null {
    const c = this.get(communityId);
    return c === null ? null : { ...c, revision: c.revision.toString(), keyId: c.keyId === null ? null : toHex(c.keyId) };
  }

  /** The publication the core accepted, which is the only one anything is sealed to; null otherwise. */
  publication(communityId: string): Uint8Array | null {
    if (this.botProblem.has(communityId)) return null;
    return this.entries.get(communityId)?.verified?.publication ?? null;
  }

  subscribe(communityId: string, fn: () => void): () => void {
    const set = this.listeners.get(communityId) ?? new Set();
    set.add(fn);
    this.listeners.set(communityId, set);
    return () => { set.delete(fn); };
  }

  /** op 61: a newer revision replaces the held catalog with no request (the key check is local); an older one is ignored. */
  apply(frame: BotCatalogFrame): void {
    const communityId = toHex(frame.communityId);
    let commands: BotCommandSpec[];
    let publication: Uint8Array | null;
    try { commands = decodeBotCommands(frame.commands); publication = readPublication(frame.publication); }
    catch (e) { this.deps.onError?.(codeOf(e)); return; }
    const held = this.entries.get(communityId);
    if (held !== undefined && frame.revision <= held.revision) return;
    // op 61 carries no status, but only a running Bot publishes: a held absent, disabled or offline reads as ready now; a
    // degraded one stays degraded until the next read says otherwise.
    const serverStatus: BotStatus = held?.serverStatus.state === 'degraded' ? held.serverStatus : { state: 'ready', since: null };
    const entry: Entry = { revision: frame.revision, commands, publication, verified: null, serverStatus, botChannels: held?.botChannels ?? null };
    this.entries.set(communityId, entry);
    if (this.pinOf(communityId) !== undefined || publication === null) {
      this.verifyEntry(communityId, entry);
      this.notify(communityId);
      return;
    }
    void this.resolveBot(communityId).then(() => {
      if (this.entries.get(communityId) !== entry) return;
      this.verifyEntry(communityId, entry);
      this.notify(communityId);
    });
  }

  /** A new gateway connection: the next sync reads the revisions again (op 61 is not replayed). */
  newConnection(): void { this.revisions = null; this.listFetched.clear(); this.pinRefused.clear(); }

  /** The instance's revisions for this connection (one read, shared with the catalog sync); null when it could not be read. */
  readRevisions(): Promise<BotRevision[] | null> {
    this.revisions ??= this.deps.fetchRevisions().then(decodeBotRevisions).catch((e: unknown) => { this.deps.onError?.(codeOf(e)); return null; });
    return this.revisions;
  }

  /** Connect sync: fetch the catalog only when none is held or the instance's revision is newer than the held one. */
  async syncOnConnect(communityId: string): Promise<void> {
    const revisions = await this.readRevisions();
    const held = this.entries.get(communityId);
    if (held !== undefined) {
      if (revisions === null) return;
      const server = revisions.find(r => r.kind === CATALOG_KIND && r.scopeId === communityId);
      if (server === undefined || server.revision <= held.revision) return;
    }
    await this.refresh(communityId);
  }

  /** Reads the catalog and the Bot channels now (a structure change, op 54, or a stale key). A failure keeps what is held. */
  refresh(communityId: string): Promise<void> {
    const running = this.inFlight.get(communityId);
    if (running !== undefined) return running;
    const task = this.load(communityId).finally(() => { this.inFlight.delete(communityId); });
    this.inFlight.set(communityId, task);
    return task;
  }

  /** Checks a held publication the core has not accepted yet again (its signer's device list may have arrived since); local,
   *  no request. True when it is usable now. */
  reverify(communityId: string): boolean {
    const e = this.entries.get(communityId);
    if (e === undefined) return false;
    if (e.verified === null && e.publication !== null && this.pinOf(communityId) !== undefined) {
      this.verifyEntry(communityId, e);
      if (e.verified !== null) this.notify(communityId);
    }
    return this.publication(communityId) !== null;
  }

  /** The core no longer holds the key as verified (expired, or evicted from its table): drop this device's verdict too, so
   *  the next check is a real one. */
  invalidate(communityId: string): void {
    const e = this.entries.get(communityId);
    if (e === undefined || e.verified === null) return;
    e.verified = null;
    this.notify(communityId);
  }

  /** Reconnect: the server's Bot user is looked up again (a reinstall is noticed, never adopted silently), then the key is
   *  checked again locally. */
  async reresolve(communityId: string): Promise<void> {
    if (!this.entries.has(communityId)) return;
    await this.resolveBot(communityId);
    const e = this.entries.get(communityId);
    if (e !== undefined) { this.verifyEntry(communityId, e); this.notify(communityId); }
  }

  /** The trust refresh ingested account-kind attestations (review M6): a community whose first pin was refused for want of
   *  the Bot's attestation is resolved again at once, and its key checked. */
  async attestationsChanged(): Promise<void> {
    const waiting = [...this.pinRefused];
    this.pinRefused.clear();
    for (const communityId of waiting) {
      if (!this.entries.has(communityId) || this.pinOf(communityId) !== undefined) continue;
      await this.reresolve(communityId);
    }
  }

  /** Every held community (sign-out drops them all). */
  communities(): string[] { return [...this.entries.keys()]; }

  clear(): void {
    const held = [...this.entries.keys()];
    this.entries.clear();
    this.pins.clear();
    this.listFetched.clear();
    this.pinRefused.clear();
    this.listHeld.clear();
    this.botProblem.clear();
    this.revisions = null;
    for (const id of held) this.notify(id);
  }

  /** Asks the server for the community's Bot user and applies the pin rule: the first one seen is pinned; the same one again
   *  is fine; another one is `changed` and is never used; more than one kind-1 account is refused. */
  private resolveBot(communityId: string): Promise<void> {
    const running = this.resolving.get(communityId);
    if (running !== undefined) return running;
    const task = (async () => {
      let candidate: BotUserCandidate;
      try { candidate = await this.deps.botUserId(communityId); } catch { return; }
      if (candidate === 'ambiguous') { this.botProblem.set(communityId, 'ambiguous'); return; }
      if (candidate === null) return;
      // The pin is the core store's (task 20b): a first sight is written there, so a reload never re-pins. It is written only
      // for an account the core holds a Bot attestation for (Task 20b review I1): before a first pin the candidate's
      // attestation is read and ingested, and a refusal (none yet: E_DEVICE_LIST_STALE; a human: E_CREDENTIAL) leaves the
      // commands greyed unverified, tried again on the next connection, never on this one.
      // The core refuses any candidate it holds no Bot attestation for, even with a pin held (fef388bd); a held pin is
      // compared first, and its attestation read once when the core lacks it.
      if (this.pinRefused.has(communityId)) return;
      // A head that could not be read (offline, a transient error) is no refusal (review M6): the pin is tried again on the
      // next catalog read or trust refresh of this connection. Only a refusal after a successful read waits for the next
      // connection (or for an attestation the trust refresh brings: attestationsChanged).
      let readFailed = false;
      const ingest = async (): Promise<void> => {
        try { await this.deps.ingestBotAttestation(candidate); readFailed = false; }
        catch (e) { readFailed = true; this.deps.onError?.(codeOf(e)); }
      };
      let ingested = false;
      if (this.pinOf(communityId) === undefined) { await ingest(); ingested = true; }
      let answer: { state: 0 | 1 | 2; pinned: Uint8Array } | null = null;
      for (;;) {
        try { answer = this.deps.pinBotUser(communityId, candidate); break; }
        catch (e) {
          if (!ingested && codeOf(e) === 'E_DEVICE_LIST_STALE') { await ingest(); ingested = true; continue; }
          if (!readFailed) this.pinRefused.add(communityId);
          this.deps.onError?.(codeOf(e));
          return;
        }
      }
      this.pins.set(communityId, toHex(answer.pinned));
      if (answer.state === 0) { this.botProblem.delete(communityId); return; }
      if (answer.state === 1) { if (this.botProblem.get(communityId) === 'ambiguous') this.botProblem.delete(communityId); return; }
      this.botProblem.set(communityId, 'changed');
      this.deps.onError?.('E_BOT_USER_CHANGED');
    })().finally(() => { this.resolving.delete(communityId); });
    this.resolving.set(communityId, task);
    return task;
  }

  private async load(communityId: string): Promise<void> {
    let decoded: ReturnType<typeof decodeBotCatalogResponse>;
    let channels: string[] | null;
    try {
      const [body, list] = await Promise.all([this.deps.fetchCatalog(communityId), this.deps.fetchBotChannels(communityId).catch(() => null)]);
      decoded = decodeBotCatalogResponse(body);
      channels = list === null ? null : decodeBotChannels(list);
    } catch (e) { this.deps.onError?.(codeOf(e)); return; }
    // Every read looks the Bot user up again: a reinstall under another account is noticed (and greyed), never adopted.
    if (decoded.publication !== null) await this.resolveBot(communityId);
    // Compared after every await: a push (op 61) that landed meanwhile and is newer than this read wins; this read's status
    // and channels are still current.
    const current = this.entries.get(communityId);
    const keep = current !== undefined && current.revision > decoded.revision;
    const base = keep ? current : { revision: decoded.revision, commands: decoded.commands, publication: decoded.publication, verified: null };
    const entry: Entry = { ...base, serverStatus: decoded.status, botChannels: channels ?? current?.botChannels ?? null };
    if (!keep || entry.verified === null) this.verifyEntry(communityId, entry);
    this.entries.set(communityId, entry);
    this.notify(communityId);
  }

  /** The community's pinned Bot user (hex): this worker's copy, else the core store's (after a reload); undefined if none. */
  private pinOf(communityId: string): string | undefined {
    const held = this.pins.get(communityId);
    if (held !== undefined) return held;
    let stored: Uint8Array | null;
    try { stored = this.deps.pinnedBotUser(communityId); } catch { return undefined; }
    if (stored === null) return undefined;
    const id = toHex(stored);
    this.pins.set(communityId, id);
    return id;
  }

  private verifyEntry(communityId: string, e: Entry): void {
    e.verified = null;
    e.refusal = null;
    const pinned = this.pinOf(communityId);
    if (e.publication === null || pinned === undefined || this.botProblem.has(communityId)) return;
    // A publication for another community is refused here, before the core sees it (it would replace that community's key
    // in the core's verified set).
    if (publicationCommunity(e.publication) !== communityId) { this.deps.onError?.('E_BOT_CONTEXT'); return; }
    try {
      const { keyId, fingerprint } = this.deps.verify(e.publication, fromHex(pinned));
      e.verified = { publication: e.publication, keyId, fingerprint };
      this.listHeld.delete(pinned);
    } catch (err) {
      const code = codeOf(err);
      e.refusal = code;
      this.deps.onError?.(code);
      // A refusal that may mean another Bot account: look it up again (it is greyed `changed` if so, never switched to).
      if (SIGNER_REFUSALS.has(code)) void this.resolveBot(communityId).then(() => { if (this.botProblem.has(communityId)) this.notify(communityId); });
      // Task 20b: no device list for the Bot (this member shares no group with it), or one behind its head: read it and pin
      // it once on this connection, then check again.
      if (code === 'E_DEVICE_LIST_STALE') this.fetchBotList(pinned);
    }
  }

  /** Fetches and pins the Bot user's device list, at most once per connection, then checks every held publication signed
   *  by that Bot user again. A failure keeps them greyed `unverified`; a held key change greys them `changed`. */
  private fetchBotList(botUser: string): void {
    if (this.listFetched.has(botUser)) return;
    this.listFetched.add(botUser);
    void this.deps.pinBotDeviceList(fromHex(botUser)).then((state) => {
      if (state === 1) this.listHeld.add(botUser); else this.listHeld.delete(botUser);
      for (const [communityId, e] of this.entries) {
        if (this.pins.get(communityId) !== botUser || e.verified !== null) continue;
        this.verifyEntry(communityId, e);
        this.notify(communityId);
      }
    }, (err: unknown) => { this.deps.onError?.(codeOf(err)); });
  }

  /** The greyed reasons this device adds to the instance's status. */
  private statusOf(communityId: string, e: Entry): BotStatus {
    const problem = this.botProblem.get(communityId);
    if (problem === 'changed') return { state: 'changed', since: null };
    if (problem === 'ambiguous') return { state: 'unverified', since: null };
    // A key change of the Bot's device list is held until it is verified (task 20b): its commands are paused.
    const pinned = this.pins.get(communityId);
    if (e.verified === null && e.publication !== null && (e.refusal === 'E_UMK_CHANGED' || (pinned !== undefined && this.listHeld.has(pinned)))) {
      return { state: 'changed', since: null };
    }
    if (e.verified !== null || e.serverStatus.state !== 'ready') return e.serverStatus;
    // Ready, but nothing to seal to: no key published yet is the Bot still starting (offline); a key this device could not
    // check has its own reason.
    return e.publication === null ? { state: 'offline', since: e.serverStatus.since } : { state: 'unverified', since: null };
  }

  private notify(communityId: string): void {
    for (const fn of [...(this.listeners.get(communityId) ?? [])]) {
      try { fn(); } catch { /* a listener's failure is its own */ }
    }
  }
}
