/**
 * Bot v2 state hints (op 63). A hint carries no content: it says a user, channel or call scope has a newer revision, and the
 * client fetches once. This class only decides when: at most one fetch per scope per 250 ms, a channel or call scope only
 * while a surface shows it, the user scope (reminders) always, and one catch-up per scope at connect when the instance is
 * ahead of the revision this device last saw (op 63 is not replayed).
 */

/** Scope kinds of op 63: 0 the user's own (its id the community), 1 a channel, 2 a call (its id the voice channel). */
export interface HintScope { kind: 0 | 1 | 2; id: string }
export interface Hint extends HintScope { revision: bigint }
/** One row of `GET /v1/bot/revisions`; kind 3 (the catalog) is not a hint scope. */
export interface HintRevision { kind: number; scopeId: string; revision: bigint }

export interface BotHintsDeps {
  /** Fetches the scope's state once. A rejection is ignored here: the surface that owns the fetch reports it. */
  fetch(scope: HintScope): Promise<void>;
  now(): number;
  /** Runs `fn` after `ms`; the returned function cancels it. */
  setTimer(fn: () => void, ms: number): () => void;
  /** Whether a surface shows the scope now. The user scope is fetched whatever this says. */
  visible(scope: HintScope): boolean;
}

/** Hint coalescing: at most one fetch per scope in this window. */
export const HINT_COALESCE_MS = 250;

const keyOf = (scope: HintScope): string => `${scope.kind}:${scope.id}`;

export class BotHints {
  private readonly seen = new Map<string, bigint>();
  private readonly lastFetch = new Map<string, number>();
  private readonly pending = new Map<string, () => void>();
  /** Surfaces showing a channel or call scope, counted per scope. These follow the pages, not the account: a sign-out does
   *  not reset them, so an "off" from a surface that mounted before a re-sign-in cannot hide one mounted after it. */
  private readonly watched = new Map<string, number>();
  /** Scopes whose hint was dropped because nothing showed them: the next watch-on fetches once. */
  private readonly missed = new Set<string>();

  constructor(private readonly deps: BotHintsDeps) {}

  /** An op 63 frame. A revision that is not newer than the last seen one changes nothing. */
  onHint(hint: Hint): void {
    const scope = { kind: hint.kind, id: hint.id };
    const key = keyOf(scope);
    const held = this.seen.get(key);
    if (held !== undefined && hint.revision <= held) return;
    this.seen.set(key, hint.revision);
    this.request(scope);
  }

  /** The connect sync: every scope the instance is ahead of (or this device never saw) is requested once. */
  onConnect(revisions: readonly HintRevision[]): void {
    for (const row of revisions) {
      if (row.kind !== 0 && row.kind !== 1 && row.kind !== 2) continue;
      const scope: HintScope = { kind: row.kind, id: row.scopeId };
      const key = keyOf(scope);
      const held = this.seen.get(key);
      if (held !== undefined && row.revision <= held) continue;
      this.seen.set(key, row.revision);
      this.request(scope);
    }
  }

  /** A surface starts (`on`) or stops showing a channel or call scope. */
  watch(scope: HintScope, on: boolean): void {
    const key = keyOf(scope);
    const count = (this.watched.get(key) ?? 0) + (on ? 1 : -1);
    if (count > 0) this.watched.set(key, count); else this.watched.delete(key);
    if (on && this.missed.delete(key)) this.request(scope);
  }

  /** Whether a surface shows the scope now. */
  shown(scope: HintScope): boolean { return (this.watched.get(keyOf(scope)) ?? 0) > 0; }

  /** Sign-out: nothing is fetched afterwards and no revision is kept. */
  clear(): void {
    for (const cancel of this.pending.values()) cancel();
    this.pending.clear();
    this.seen.clear();
    this.lastFetch.clear();
    this.missed.clear();
  }

  private request(scope: HintScope): void {
    const key = keyOf(scope);
    if (scope.kind !== 0 && !this.deps.visible(scope)) { this.missed.add(key); return; }
    if (this.pending.has(key)) return;      // the trailing fetch reads whatever is newest
    const last = this.lastFetch.get(key);
    const wait = last === undefined ? 0 : Math.max(0, last + HINT_COALESCE_MS - this.deps.now());
    if (wait === 0) { this.run(scope); return; }
    this.pending.set(key, this.deps.setTimer(() => {
      this.pending.delete(key);
      if (scope.kind !== 0 && !this.deps.visible(scope)) { this.missed.add(key); return; }
      this.run(scope);
    }, wait));
  }

  private run(scope: HintScope): void {
    this.lastFetch.set(keyOf(scope), this.deps.now());
    this.deps.fetch(scope).catch(() => undefined);
  }
}

/** The part of the page's client a hint watcher needs. */
export interface HintClient {
  call(command: { m: 'watchBotScope'; kind: 1 | 2; channelId: string; on: boolean }): Promise<unknown>;
  get(name: 'botHints'): Record<string, number> | undefined;
  subscribe(name: 'botHints', listener: () => void): () => void;
}

interface Fan { listeners: Set<() => void>; stop: () => void }
const fans = new WeakMap<object, Map<string, Fan>>();

/**
 * A surface's side of the hints: `onHint` runs whenever the worker asks for a fetch of the scope (the user scope: any
 * community's), never on a timer and never for state the surface already read. All watchers of one scope on one client share
 * a single slice subscription and a single `watchBotScope` registration, which is on while any of them is. Returns the stop.
 */
export function watchBotHint(client: HintClient, scope: { kind: 0 } | { kind: 1 | 2; id: string }, onHint: () => void): () => void {
  const key = scope.kind === 0 ? '0' : `${scope.kind}:${scope.id}`;
  let byScope = fans.get(client);
  if (byScope === undefined) { byScope = new Map(); fans.set(client, byScope); }
  let fan = byScope.get(key);
  if (fan === undefined) {
    const listeners = new Set<() => void>();
    const wanted = (k: string): boolean => scope.kind === 0 ? k.startsWith('0:') : k === key;
    let seen = { ...(client.get('botHints') ?? {}) };
    const unsubscribe = client.subscribe('botHints', () => {
      const now = client.get('botHints') ?? {};
      let rose = false;
      for (const [k, count] of Object.entries(now)) if (wanted(k) && count > (seen[k] ?? 0)) rose = true;
      seen = { ...now };
      if (rose) for (const l of [...listeners]) l();
    });
    if (scope.kind !== 0) void client.call({ m: 'watchBotScope', kind: scope.kind, channelId: scope.id, on: true }).catch(() => undefined);
    fan = { listeners, stop: () => {
      unsubscribe();
      if (scope.kind !== 0) void client.call({ m: 'watchBotScope', kind: scope.kind, channelId: scope.id, on: false }).catch(() => undefined);
    } };
    byScope.set(key, fan);
  }
  const held = fan;
  held.listeners.add(onHint);
  return () => {
    held.listeners.delete(onHint);
    if (held.listeners.size > 0) return;
    held.stop();
    if (byScope.get(key) === held) byScope.delete(key);
  };
}
