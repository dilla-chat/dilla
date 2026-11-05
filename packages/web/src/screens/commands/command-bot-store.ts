// The Bot's command list, kept per community outside any component: a picker closing or a CommandHost unmounting
// must not discard a load (it takes 4-6 s: status, an op-3 request over the interaction group and the Bot's reply, status again).
// A community's list loads when its composer mounts and refreshes every 30 s while it stays mounted. A failed load never
// replaces the last good list; it is recorded in Diagnostics (E_BOT_CAPABILITIES) and the next refresh retries. A failure with
// no good list yet (a page just reloaded) retries sooner, at 2 s, 5 s and 10 s, and the picker keeps saying it is loading
// meanwhile; opening the picker after a failed load retries at once. A community whose Bot is gone or not ready (the
// provider is unavailable) is not a fault: its list is cleared and nothing retries until the next refresh.
import { createBotUtilities, BotUtilitiesProviderUnavailableError, type CoreClient } from '@dilla/client-core';

/** Re-read every 30 s while a community's composer is mounted (an owner may turn a feature off in another tab). */
export const CAPABILITY_REFRESH_MS = 30_000;
/** Enter on a Bot command that is not yet known waits this long for the load before the usual refusal. */
export const COMMAND_LIST_WAIT_MS = 10_000;
/** After a failed load with no good list: retry after these delays (2 s, 5 s, 10 s), then on the normal refresh. */
export const COMMAND_LIST_RETRY_MS: readonly number[] = Object.freeze([2_000, 5_000, 10_000]);

export interface CommandBot { operations: ReadonlySet<number> | null; botUserId: string | null }
export interface CommandBotSnapshot extends CommandBot {
  /** A load is in flight. */
  loading: boolean;
  /** The list has never loaded (no good list) and a load is in flight or a quick retry is waiting: the picker says so instead of "no match". */
  waiting: boolean;
  /** The Bot's installation id from the last good load: a binding check for callers that verify against that Bot (poll cards). */
  installationId: string | null;
  /** The last load found no ready Bot (a definitive refusal, not a fault): nothing will retry until the next refresh. */
  unavailable: boolean;
}

export const NO_BOT: CommandBotSnapshot = Object.freeze({ operations: null, botUserId: null, loading: false, waiting: false, installationId: null, unavailable: false });

interface Entry {
  snapshot: CommandBotSnapshot;
  listeners: Set<() => void>;
  mounted: number;
  timer: number | null;
  inFlight: boolean;
  /** The last load failed (not for a missing Bot): opening the picker retries. */
  failed: boolean;
  /** Failures in a row with no good list, which picks the next quick retry. */
  failures: number;
  retry: number | null;
  /** When the last load settled (ms), success or failure. */
  settledAt: number;
}

const stores = new WeakMap<CoreClient, Map<string, Entry>>();

function entryOf(client: CoreClient, communityId: string): Entry {
  let byCommunity = stores.get(client);
  if (byCommunity === undefined) { byCommunity = new Map(); stores.set(client, byCommunity); }
  let entry = byCommunity.get(communityId);
  if (entry === undefined) {
    entry = { snapshot: NO_BOT, listeners: new Set(), mounted: 0, timer: null, inFlight: false, failed: false, failures: 0, retry: null, settledAt: Number.NEGATIVE_INFINITY };
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
  entry.inFlight = true;
  publish(entry, { ...entry.snapshot, loading: true, waiting: entry.snapshot.operations === null });
  void createBotUtilities(client, communityId).capabilities().then(caps => {
    entry.inFlight = false; entry.settledAt = Date.now(); entry.failed = false; entry.failures = 0;
    publish(entry, { operations: new Set(caps.supportedOperations), botUserId: caps.botUserId, installationId: caps.installationId, loading: false, waiting: false, unavailable: false });
  }, (error: unknown) => {
    entry.inFlight = false; entry.settledAt = Date.now();
    if (error instanceof BotUtilitiesProviderUnavailableError) {
      // A community without a ready Bot is not a fault: its commands go (a removed Bot's list must not linger) and nothing retries quickly.
      entry.failed = false; entry.failures = 0;
      publish(entry, { operations: null, botUserId: null, installationId: null, loading: false, waiting: false, unavailable: true });
      return;
    }
    void client.call({ m: 'callNote', code: 'E_BOT_CAPABILITIES' }).catch(() => undefined);
    entry.failed = true;
    // No good list yet: try again soon (2 s, 5 s, 10 s) and keep saying "loading"; with one, the normal refresh retries.
    const delay = entry.snapshot.operations === null && entry.mounted > 0 ? COMMAND_LIST_RETRY_MS[entry.failures] : undefined;
    entry.failures += 1;
    if (delay !== undefined) entry.retry = window.setTimeout(() => { entry.retry = null; refreshCommandBot(client, communityId); }, delay);
    publish(entry, { ...entry.snapshot, loading: false, waiting: entry.snapshot.operations === null && delay !== undefined });
  });
}

/** The picker opened: when the last load failed, retry now instead of waiting for the timer. */
export function retryCommandBotIfFailed(client: CoreClient, communityId: string): void {
  const entry = entryOf(client, communityId);
  if (entry.failed && !entry.inFlight) refreshCommandBot(client, communityId);
}

/** A community's composer mounted: load its list now (if it is unknown or older than a refresh) and keep it fresh. Returns the release. */
export function mountCommandBot(client: CoreClient, communityId: string): () => void {
  const entry = entryOf(client, communityId);
  entry.mounted += 1;
  if (entry.mounted === 1) {
    // A remount within one refresh period of the last load keeps that result (the store outlives the composer).
    if (entry.failed || Date.now() - entry.settledAt >= CAPABILITY_REFRESH_MS) refreshCommandBot(client, communityId);
    entry.timer = window.setInterval(() => { refreshCommandBot(client, communityId); }, CAPABILITY_REFRESH_MS);
  }
  return () => {
    entry.mounted -= 1;
    if (entry.mounted === 0) { clearRetry(entry); if (entry.timer !== null) { window.clearInterval(entry.timer); entry.timer = null; } }
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
 * failed or there is no ready Bot (unavailable says which). No timer is mounted, so nothing refreshes after this.
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
