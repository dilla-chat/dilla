// The Bot's command list, kept per community outside any component: a picker closing or a CommandHost unmounting
// must not discard a load (it takes 4-6 s: status, an encrypted op-3 request and the Bot's reply, status again).
// A community's list loads when its composer mounts and refreshes every 30 s while it stays mounted. A failed load never
// replaces the last good list; it is recorded in Diagnostics (E_BOT_CAPABILITIES) and the next refresh retries.
import { createBotUtilities, BotUtilitiesProviderUnavailableError, type CoreClient } from '@dilla/client-core';

/** Re-read every 30 s while a community's composer is mounted (an owner may turn a feature off in another tab). */
export const CAPABILITY_REFRESH_MS = 30_000;
/** Enter on a Bot command that is not yet known waits this long for the load before the usual refusal. */
export const COMMAND_LIST_WAIT_MS = 10_000;

export interface CommandBot { operations: ReadonlySet<number> | null; botUserId: string | null }
export interface CommandBotSnapshot extends CommandBot {
  /** A load is in flight. */
  loading: boolean;
  /** The list has never loaded (no good list) and a load is in flight: the picker says so instead of "no match". */
  waiting: boolean;
}

export const NO_BOT: CommandBotSnapshot = Object.freeze({ operations: null, botUserId: null, loading: false, waiting: false });

interface Entry {
  snapshot: CommandBotSnapshot;
  listeners: Set<() => void>;
  mounted: number;
  timer: number | null;
  inFlight: boolean;
  /** When the last load settled (ms), success or failure. */
  settledAt: number;
}

const stores = new WeakMap<CoreClient, Map<string, Entry>>();

function entryOf(client: CoreClient, communityId: string): Entry {
  let byCommunity = stores.get(client);
  if (byCommunity === undefined) { byCommunity = new Map(); stores.set(client, byCommunity); }
  let entry = byCommunity.get(communityId);
  if (entry === undefined) {
    entry = { snapshot: NO_BOT, listeners: new Set(), mounted: 0, timer: null, inFlight: false, settledAt: Number.NEGATIVE_INFINITY };
    byCommunity.set(communityId, entry);
  }
  return entry;
}

function publish(entry: Entry, next: CommandBotSnapshot): void {
  entry.snapshot = next;
  for (const listener of [...entry.listeners]) listener();
}

/** Starts one load unless one is in flight; the result updates the store whether or not anything is open. */
export function refreshCommandBot(client: CoreClient, communityId: string): void {
  const entry = entryOf(client, communityId);
  if (entry.inFlight) return;
  entry.inFlight = true;
  publish(entry, { ...entry.snapshot, loading: true, waiting: entry.snapshot.operations === null });
  void createBotUtilities(client, communityId).capabilities().then(caps => {
    entry.inFlight = false; entry.settledAt = Date.now();
    publish(entry, { operations: new Set(caps.supportedOperations), botUserId: caps.botUserId, loading: false, waiting: false });
  }, (error: unknown) => {
    entry.inFlight = false; entry.settledAt = Date.now();
    // A community without a ready Bot is not a fault; any other failure is named in Diagnostics.
    if (!(error instanceof BotUtilitiesProviderUnavailableError)) void client.call({ m: 'callNote', code: 'E_BOT_CAPABILITIES' }).catch(() => undefined);
    publish(entry, { ...entry.snapshot, loading: false, waiting: false });
  });
}

/** A community's composer mounted: load its list now (if it is unknown or older than a refresh) and keep it fresh. Returns the release. */
export function mountCommandBot(client: CoreClient, communityId: string): () => void {
  const entry = entryOf(client, communityId);
  entry.mounted += 1;
  if (entry.mounted === 1) {
    // A remount within one refresh period of the last load keeps that result (the store outlives the composer).
    if (Date.now() - entry.settledAt >= CAPABILITY_REFRESH_MS) refreshCommandBot(client, communityId);
    entry.timer = window.setInterval(() => { refreshCommandBot(client, communityId); }, CAPABILITY_REFRESH_MS);
  }
  return () => {
    entry.mounted -= 1;
    if (entry.mounted === 0 && entry.timer !== null) { window.clearInterval(entry.timer); entry.timer = null; }
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
