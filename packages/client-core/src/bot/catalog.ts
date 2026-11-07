// Bot v2 (task 20): the community Bot's command catalog, held on this device. The picker reads only this copy; the network
// is asked for it once on connect when the instance's revision is newer than the held one (GET /v1/bot/revisions, then the
// catalog GET), and op 61 bot_catalog replaces it live. The catalog is not secret (names and argument shapes only); its
// command-key publication is checked against the Bot user's device list in the core before anything is sealed to it.
import { arr, bin, decode, str, u53, u64, type CborValue } from '../cbor';
import { toHex } from '../hex';
import type { BotCatalogFrame } from '../gateway/frames';

export type BotState = 'ready' | 'degraded' | 'offline' | 'disabled' | 'absent';
/** Argument type codes: 0 text, 1 uint, 2 user, 3 channel, 4 duration (uint seconds), 5 bool (uint 0|1). */
export type ArgType = 0 | 1 | 2 | 3 | 4 | 5;
export interface ArgShape { name: string; type: ArgType; required: boolean }
export interface BotCommandSpec { name: string; description: string; operation: number; args: ArgShape[]; permissionBit: number;
  needsBotChannel: boolean; handlerKind: number }
export interface BotStatus { state: BotState; since: number | null }
export interface BotCatalog {
  revision: bigint;
  commands: BotCommandSpec[];
  /** The verified command key's id; null with no publication or one the core refused (the commands are then greyed). */
  keyId: Uint8Array | null;
  /** The verified key's fingerprint, for admins (the Bot page, task 22). */
  fingerprint: string | null;
  /** The instance's status, except that a publication the core refused reads as offline. */
  status: BotStatus;
  /** The Bot channels this member can view (hex ids); null until read. A command that needs one is greyed without one. */
  botChannels: string[] | null;
  /** The community Bot's user id (hex), the key's signer; null while unknown. */
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
const utf8 = new TextEncoder();
const bytes = (s: string): number => utf8.encode(s).length;

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

/** A catalog array, read whole or refused: at most 256 commands with unique names. */
export function decodeBotCommands(values: readonly CborValue[]): BotCommandSpec[] {
  if (values.length > MAX_COMMANDS) refuse('too many commands');
  const commands = values.map(readCommand);
  if (new Set(commands.map(c => c.name)).size !== commands.length) refuse('duplicate name');
  return commands;
}

function readPublication(value: CborValue): Uint8Array | null {
  if (value === null) return null;
  const p = bin(value);
  if (p.length === 0 || p.length > MAX_PUBLICATION) refuse('publication');
  return p;
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

export interface BotCatalogDeps {
  fetchCatalog(communityId: string): Promise<Uint8Array>;
  fetchBotChannels(communityId: string): Promise<Uint8Array>;
  fetchRevisions(): Promise<Uint8Array>;
  /** The community Bot's user id: the key's signer, whose stored device list the core checks it against. */
  botUserId(communityId: string): Promise<Uint8Array | null>;
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
  botUserId: Uint8Array | null;
}

const codeOf = (e: unknown): string => (typeof e === 'object' && e !== null && 'code' in e && typeof e.code === 'string' ? e.code : 'E_BOT_CATALOG');

export class BotCatalogStore {
  private readonly entries = new Map<string, Entry>();
  private readonly listeners = new Map<string, Set<() => void>>();
  private revisions: Promise<BotRevision[] | null> | null = null;
  private readonly inFlight = new Map<string, Promise<void>>();

  constructor(private readonly deps: BotCatalogDeps) {}

  get(communityId: string): BotCatalog | null {
    const e = this.entries.get(communityId);
    if (e === undefined) return null;
    return { revision: e.revision, commands: e.commands, keyId: e.verified?.keyId ?? null, fingerprint: e.verified?.fingerprint ?? null,
      status: this.statusOf(e), botChannels: e.botChannels, botUserId: e.botUserId === null ? null : toHex(e.botUserId) };
  }

  /** The slice form of get(). */
  view(communityId: string): BotCatalogView | null {
    const c = this.get(communityId);
    return c === null ? null : { ...c, revision: c.revision.toString(), keyId: c.keyId === null ? null : toHex(c.keyId) };
  }

  /** The publication the core accepted, which is the only one anything is sealed to; null otherwise. */
  publication(communityId: string): Uint8Array | null {
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
    const entry: Entry = { revision: frame.revision, commands, publication, verified: null,
      serverStatus, botChannels: held?.botChannels ?? null, botUserId: held?.botUserId ?? null };
    this.entries.set(communityId, entry);
    if (entry.botUserId !== null || publication === null) {
      this.verifyEntry(entry);
      this.notify(communityId);
      return;
    }
    void this.deps.botUserId(communityId).then(user => {
      if (this.entries.get(communityId) !== entry) return;
      entry.botUserId = user;
      this.verifyEntry(entry);
      this.notify(communityId);
    }, () => undefined);
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
    if (e.verified === null && e.publication !== null && e.botUserId !== null) {
      this.verifyEntry(e);
      if (e.verified !== null) this.notify(communityId);
    }
    return e.verified !== null;
  }

  /** Every held community (sign-out drops them all). */
  communities(): string[] { return [...this.entries.keys()]; }

  clear(): void {
    const held = [...this.entries.keys()];
    this.entries.clear();
    this.revisions = null;
    for (const id of held) this.notify(id);
  }

  private async load(communityId: string): Promise<void> {
    let decoded: ReturnType<typeof decodeBotCatalogResponse>;
    let channels: string[] | null;
    try {
      const [body, list] = await Promise.all([this.deps.fetchCatalog(communityId), this.deps.fetchBotChannels(communityId).catch(() => null)]);
      decoded = decodeBotCatalogResponse(body);
      channels = list === null ? null : decodeBotChannels(list);
    } catch (e) { this.deps.onError?.(codeOf(e)); return; }
    const held = this.entries.get(communityId);
    // A push that landed while this read was out may be newer; the status and channels of this read are still current.
    const keep = held !== undefined && held.revision > decoded.revision;
    let botUserId = held?.botUserId ?? null;
    if (decoded.publication !== null && botUserId === null) {
      try { botUserId = await this.deps.botUserId(communityId); } catch { botUserId = null; }
    }
    const current = this.entries.get(communityId);
    const base = keep && current !== undefined ? current
      : { revision: decoded.revision, commands: decoded.commands, publication: decoded.publication, verified: null };
    const entry: Entry = { ...base, serverStatus: decoded.status, botChannels: channels ?? current?.botChannels ?? null, botUserId };
    if (!keep || entry.verified === null) this.verifyEntry(entry);
    this.entries.set(communityId, entry);
    this.notify(communityId);
  }

  private verifyEntry(e: Entry): void {
    e.verified = null;
    if (e.publication === null || e.botUserId === null) return;
    try {
      const { keyId, fingerprint } = this.deps.verify(e.publication, e.botUserId);
      e.verified = { publication: e.publication, keyId, fingerprint };
    } catch (err) { this.deps.onError?.(codeOf(err)); }
  }

  /** A held publication the core refused is the offline reason: its commands are greyed, never sealed to. */
  private statusOf(e: Entry): BotStatus {
    if (e.verified === null && e.serverStatus.state === 'ready') return { state: 'offline', since: e.serverStatus.since };
    return e.serverStatus;
  }

  private notify(communityId: string): void {
    for (const fn of [...(this.listeners.get(communityId) ?? [])]) {
      try { fn(); } catch { /* a listener's failure is its own */ }
    }
  }
}
