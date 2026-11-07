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
/** One kind/scope/revision of GET /v1/bot/revisions; kind 3 is a community's catalog (scope_id the community). */
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
  /** A catalog read or a key check that failed, by code (diagnostics); never its content. */
  onError?(code: string): void;
}

interface Entry {
  revision: bigint;
  commands: BotCommandSpec[];
  publication: Uint8Array | null;
  /** The publication the core accepted; null when none was, so nothing is sealed. */
  verified: { publication: Uint8Array; keyId: Uint8Array; fingerprint: string } | null;
  serverStatus: BotStatus;
  botChannels: string[] | null;
}

const codeOf = (e: unknown): string => (typeof e === 'object' && e !== null && 'code' in e && typeof e.code === 'string' ? e.code : 'E_BOT_CATALOG');

export class BotCatalogStore {
  private readonly entries = new Map<string, Entry>();
  private readonly listeners = new Map<string, Set<() => void>>();
  private revisions: Promise<BotRevision[] | null> | null = null;
  private readonly inFlight = new Map<string, Promise<void>>();
  /** community → the Bot user this device pinned on first sight (hex). Never replaced while the worker lives. */
  private readonly pins = new Map<string, string>();
  /** community → why its Bot user cannot be used: the server named another one, or more than one. */
  private readonly botProblem = new Map<string, 'changed' | 'ambiguous'>();
  private readonly resolving = new Map<string, Promise<void>>();

  constructor(private readonly deps: BotCatalogDeps) {}

  get(communityId: string): BotCatalog | null {
    const e = this.entries.get(communityId);
    if (e === undefined) return null;
    return { revision: e.revision, commands: e.commands, keyId: e.verified?.keyId ?? null, fingerprint: e.verified?.fingerprint ?? null,
      status: this.statusOf(communityId, e), botChannels: e.botChannels, botUserId: this.pins.get(communityId) ?? null };
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
    if (this.pins.has(communityId) || publication === null) {
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
  newConnection(): void { this.revisions = null; }

  /** Connect sync: fetch the catalog only when none is held or the instance's revision is newer than the held one. */
  async syncOnConnect(communityId: string): Promise<void> {
    this.revisions ??= this.deps.fetchRevisions().then(decodeBotRevisions).catch((e: unknown) => { this.deps.onError?.(codeOf(e)); return null; });
    const revisions = await this.revisions;
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
    if (e.verified === null && e.publication !== null && this.pins.has(communityId)) {
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

  /** Every held community (sign-out drops them all). */
  communities(): string[] { return [...this.entries.keys()]; }

  clear(): void {
    const held = [...this.entries.keys()];
    this.entries.clear();
    this.pins.clear();
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
      const pinned = this.pins.get(communityId);
      if (candidate === 'ambiguous') { this.botProblem.set(communityId, 'ambiguous'); return; }
      if (candidate === null) return;
      const id = toHex(candidate);
      if (pinned === undefined) { this.pins.set(communityId, id); this.botProblem.delete(communityId); return; }
      if (pinned === id) { if (this.botProblem.get(communityId) === 'ambiguous') this.botProblem.delete(communityId); return; }
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

  private verifyEntry(communityId: string, e: Entry): void {
    e.verified = null;
    const pinned = this.pins.get(communityId);
    if (e.publication === null || pinned === undefined || this.botProblem.has(communityId)) return;
    // A publication for another community is refused here, before the core sees it (it would replace that community's key
    // in the core's verified set).
    if (publicationCommunity(e.publication) !== communityId) { this.deps.onError?.('E_BOT_CONTEXT'); return; }
    try {
      const { keyId, fingerprint } = this.deps.verify(e.publication, fromHex(pinned));
      e.verified = { publication: e.publication, keyId, fingerprint };
    } catch (err) {
      const code = codeOf(err);
      this.deps.onError?.(code);
      // A refusal that may mean another Bot account: look it up again (it is greyed `changed` if so, never switched to).
      if (SIGNER_REFUSALS.has(code)) void this.resolveBot(communityId).then(() => { if (this.botProblem.has(communityId)) this.notify(communityId); });
    }
  }

  /** The greyed reasons this device adds to the instance's status. */
  private statusOf(communityId: string, e: Entry): BotStatus {
    const problem = this.botProblem.get(communityId);
    if (problem === 'changed') return { state: 'changed', since: null };
    if (problem === 'ambiguous') return { state: 'unverified', since: null };
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
