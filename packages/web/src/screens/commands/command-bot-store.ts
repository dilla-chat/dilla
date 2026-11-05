// The Bot's command list, kept per community outside any component: a picker closing or a CommandHost unmounting
// must not discard a load (it takes 4-6 s: status, an op-3 request over the interaction group and the Bot's reply, status again).
// An idle client makes no periodic HTTP (the wire-5 rule), so the store has no background timer. A community's list loads once
// when its composer first mounts (a hidden window waits until it is shown), or on first need (a poll card, Enter on an unknown
// command). Opening the picker keeps it fresh: a full load when there is no answer yet or the last load failed; otherwise, when the
// list was last confirmed more than 30 s ago, one status read (one plain GET), and the op-3 round trip again only when the status
// shows a different installation, Bot user, state or revision than the last full load saw (an owner toggling a feature moves the
// revision). A failed load never replaces the last good list; it is recorded in Diagnostics (E_BOT_CAPABILITIES). A failure with no
// answer yet (a page just reloaded) retries sooner, at 2 s, 5 s and 10 s (bounded, never periodic), and the picker keeps saying it
// is loading meanwhile. A community whose Bot is gone or not ready (the status read refuses) is not a fault: its list is cleared,
// and only a status change seen on a picker open loads it again. The store is keyed by CoreClient and relies on sign-out reloading
// the page; a sign-out without a reload must drop it, or another account would see this one's list.
import { createBotUtilities, BotUtilitiesProviderUnavailableError, type CoreClient } from '@dilla/client-core';

/** Opening the picker re-checks the Bot's status when the list was last confirmed longer ago than this (an owner may turn a feature off in another tab). */
export const PICKER_RECHECK_MS = 30_000;
/** Enter on a Bot command that is not yet known waits this long for the load before the usual refusal. */
export const COMMAND_LIST_WAIT_MS = 10_000;
/** After a failed load with no good list: retry after these delays (2 s, 5 s, 10 s), then only when the picker opens. */
export const COMMAND_LIST_RETRY_MS: readonly number[] = Object.freeze([2_000, 5_000, 10_000]);

export interface CommandBot { operations: ReadonlySet<number> | null; botUserId: string | null }
export interface CommandBotSnapshot extends CommandBot {
  /** A load is in flight. */
  loading: boolean;
  /** The list has never loaded (no good list) and a load is in flight or a quick retry is waiting: the picker says so instead of "no match". */
  waiting: boolean;
  /** The Bot's installation id from the last good load: a binding check for callers that verify against that Bot (poll cards). */
  installationId: string | null;
  /** The last load found no ready Bot (a definitive refusal, not a fault): nothing retries until a picker open sees a status change. */
  unavailable: boolean;
}

export const NO_BOT: CommandBotSnapshot = Object.freeze({ operations: null, botUserId: null, loading: false, waiting: false, installationId: null, unavailable: false });

interface Entry {
  snapshot: CommandBotSnapshot;
  listeners: Set<() => void>;
  mounted: number;
  inFlight: boolean;
  /** The last load failed (not for a missing Bot): opening the picker retries. */
  failed: boolean;
  /** Failures in a row with no good list, which picks the next quick retry. */
  failures: number;
  retry: number | null;
  /** When the list was last known current (ms): a load settling with an answer, or a status read matching the last full load. */
  checkedAt: number;
  /** The status the last full load started from (see statusKey); undefined when that read failed (the next check adopts one). */
  loadedKey: string | undefined;
  /** A picker-open status read is in flight. */
  statusInFlight: boolean;
  /** A load was due while the window was hidden: becoming visible runs it once. */
  deferred: boolean;
  /** The document's visibilitychange listener while mounted. */
  onVisible: (() => void) | null;
}

/** Whether a Bot refusal means "no ready Bot" (the status read refused), not a transient fault of the op-3 round trip. */
export function botDefinitelyUnavailable(error: unknown): boolean {
  // The status read's refusal carries no operation; an op-3 refusal (status 5, or the installation changing mid-load) is transient.
  return error instanceof BotUtilitiesProviderUnavailableError && error.operation !== 3;
}

function hiddenNow(): boolean {
  return typeof document !== 'undefined' && document.visibilityState === 'hidden';
}

/** What identifies the Bot's installation and configuration: a change means the list may differ. */
function statusKey(status: unknown): string {
  if (status === null || typeof status !== 'object') return 'none';
  const row = status as { installationId?: unknown; botUserId?: unknown; state?: unknown; revision?: unknown };
  return [row.installationId, row.botUserId, row.state, row.revision].map(String).join(':');
}

function readStatusKey(client: CoreClient, communityId: string): Promise<string | undefined> {
  return client.call({ m: 'getBotCommandStatus', communityId }).then(statusKey, () => undefined);
}

const stores = new WeakMap<CoreClient, Map<string, Entry>>();

function entryOf(client: CoreClient, communityId: string): Entry {
  let byCommunity = stores.get(client);
  if (byCommunity === undefined) { byCommunity = new Map(); stores.set(client, byCommunity); }
  let entry = byCommunity.get(communityId);
  if (entry === undefined) {
    entry = { snapshot: NO_BOT, listeners: new Set(), mounted: 0, inFlight: false, failed: false, failures: 0, retry: null,
      checkedAt: Number.NEGATIVE_INFINITY, loadedKey: undefined, statusInFlight: false, deferred: false, onVisible: null };
    byCommunity.set(communityId, entry);
  }
  return entry;
}

function publish(entry: Entry, next: CommandBotSnapshot): void {
  entry.snapshot = next;
  for (const listener of [...entry.listeners]) listener();
}

function clearRetry(entry: Entry): void {
  if (entry.retry !== null) { window.clearTimeout(entry.retry); entry.retry = null; }
}

/** Starts one load unless one is in flight; the result updates the store whether or not anything is open. */
export function refreshCommandBot(client: CoreClient, communityId: string): void {
  const entry = entryOf(client, communityId);
  if (entry.inFlight) return;
  clearRetry(entry);
  entry.inFlight = true; entry.deferred = false;
  // Only a first, unknown load says "loading": a community known to have no Bot stays quiet while it re-checks (final review M2).
  publish(entry, { ...entry.snapshot, loading: true, waiting: !answered(entry) });
  // The status this load starts from, read alongside: a picker-open status check compares later reads with it.
  void readStatusKey(client, communityId).then(key => { entry.loadedKey = key; });
  void createBotUtilities(client, communityId).capabilities().then(caps => {
    entry.inFlight = false; entry.checkedAt = Date.now(); entry.failed = false; entry.failures = 0;
    publish(entry, { operations: new Set(caps.supportedOperations), botUserId: caps.botUserId, installationId: caps.installationId, loading: false, waiting: false, unavailable: false });
  }, (error: unknown) => {
    entry.inFlight = false;
    if (botDefinitelyUnavailable(error)) {
      entry.checkedAt = Date.now();
      // A community without a ready Bot is not a fault: its commands go (a removed Bot's list must not linger) and nothing retries quickly.
      entry.failed = false; entry.failures = 0;
      publish(entry, { operations: null, botUserId: null, installationId: null, loading: false, waiting: false, unavailable: true });
      return;
    }
    void client.call({ m: 'callNote', code: 'E_BOT_CAPABILITIES' }).catch(() => undefined);
    entry.failed = true;
    // No answer yet: try again soon (2 s, 5 s, 10 s) and keep saying "loading"; after that (or with a list) the picker retries.
    const delay = !answered(entry) && entry.mounted > 0 ? COMMAND_LIST_RETRY_MS[entry.failures] : undefined;
    entry.failures += 1;
    if (delay !== undefined) {
      entry.retry = window.setTimeout(() => {
        entry.retry = null;
        // A hidden window makes no background request; becoming visible loads instead.
        if (!hiddenNow()) refreshCommandBot(client, communityId); else entry.deferred = true;
      }, delay);
    }
    publish(entry, { ...entry.snapshot, loading: false, waiting: !answered(entry) && delay !== undefined });
  });
}

/** The store holds an answer: a list, or a definitive "no ready Bot". */
function answered(entry: Entry): boolean {
  return entry.snapshot.operations !== null || entry.snapshot.unavailable;
}

/**
 * The picker-open check on an answered list: one status read, and a full load only when it differs from what the last full load
 * started from. A community recorded as having no ready Bot whose status now reads loads again (a Bot was installed since).
 */
function statusCheck(client: CoreClient, communityId: string): void {
  const entry = entryOf(client, communityId);
  if (entry.inFlight || entry.statusInFlight) return;
  entry.statusInFlight = true;
  void readStatusKey(client, communityId).then(key => {
    entry.statusInFlight = false;
    if (key === undefined || entry.inFlight) return;
    if (entry.loadedKey === undefined && !entry.snapshot.unavailable) { entry.loadedKey = key; entry.checkedAt = Date.now(); return; }
    if (key !== entry.loadedKey) refreshCommandBot(client, communityId);
    else entry.checkedAt = Date.now();
  });
}

/** The picker opened: load now when there is no answer or the last load failed; re-check the status when the list is older than 30 s. */
export function refreshCommandBotForPicker(client: CoreClient, communityId: string): void {
  const entry = entryOf(client, communityId);
  if (entry.inFlight) return;
  if (entry.failed || !answered(entry)) { refreshCommandBot(client, communityId); return; }
  if (Date.now() - entry.checkedAt >= PICKER_RECHECK_MS) statusCheck(client, communityId);
}

/** A community's composer mounted: load its list once if it is unknown or the last load failed. No timer: nothing repeats. Returns the release. */
export function mountCommandBot(client: CoreClient, communityId: string): () => void {
  const entry = entryOf(client, communityId);
  entry.mounted += 1;
  if (entry.mounted === 1) {
    // A remount keeps the stored result (the store outlives the composer); the picker re-checks it. A hidden window waits until it is shown.
    if (!entry.inFlight && entry.retry === null && (entry.failed || !answered(entry))) {
      if (hiddenNow()) entry.deferred = true; else refreshCommandBot(client, communityId);
    }
    entry.onVisible = () => {
      if (entry.deferred && !hiddenNow() && !entry.inFlight) refreshCommandBot(client, communityId);
    };
    document.addEventListener('visibilitychange', entry.onVisible);
  }
  return () => {
    entry.mounted -= 1;
    if (entry.mounted === 0) {
      clearRetry(entry); entry.deferred = false;
      if (entry.onVisible !== null) { document.removeEventListener('visibilitychange', entry.onVisible); entry.onVisible = null; }
    }
  };
}

export function subscribeCommandBot(client: CoreClient, communityId: string, listener: () => void): () => void {
  const entry = entryOf(client, communityId);
  entry.listeners.add(listener);
  return () => { entry.listeners.delete(listener); };
}

export function commandBotSnapshot(client: CoreClient, communityId: string): CommandBotSnapshot {
  return entryOf(client, communityId).snapshot;
}

/**
 * The Bot's identity for a verifier that is not a composer (a poll card): the stored one when a list is already held, else a load
 * (started now unless one is in flight) awaited to its end. Resolves with the snapshot either way; botUserId is null when the load
 * failed or there is no ready Bot (unavailable says which). Nothing refreshes after this.
 */
export function loadedCommandBot(client: CoreClient, communityId: string): Promise<CommandBotSnapshot> {
  const entry = entryOf(client, communityId);
  if (entry.snapshot.botUserId === null) refreshCommandBot(client, communityId);
  if (!entry.snapshot.loading) return Promise.resolve(entry.snapshot);
  return new Promise(resolve => {
    const listener = () => { if (!entry.snapshot.loading) { entry.listeners.delete(listener); resolve(entry.snapshot); } };
    entry.listeners.add(listener);
  });
}
