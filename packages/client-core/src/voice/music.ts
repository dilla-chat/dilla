import type { CoreClient } from '../bridge';
import { decode, type CborValue } from '../cbor';
import { BotInteractionBindingMismatchError, BotInteractionRejectedError, BotInteractionUncertainError, createBotInteractionClient, type BotInteractionResult, type BotInteractionScope } from '../bot/interactions';

export interface VoiceMusicTrack {
  id: string;
  title: string;
  artist: string;
  album?: string;
  artworkUrl?: string;
  durationMs: number;
  /** The provider the snapshot names for a queued or current track ('youtube' or 'spotify'); absent for search results. */
  provider?: 'youtube' | 'spotify';
  /** The 32-hex user id of the member who queued it (snapshot row slot 7), when the Bot recorded one. */
  requesterId?: string;
}

export interface VoiceMusicQueueItem { id: string; track: VoiceMusicTrack; }
export interface VoiceMusicSearchResult extends VoiceMusicTrack { input: string; }
export interface VoiceMusicPermissions { control: boolean; manageQueue: boolean; }
export type VoiceMusicErrorCode = 'forbidden' | 'conflict' | 'spotify-unavailable' | 'spotify-busy' | 'unavailable' | 'operation-unavailable';
export class VoiceMusicError extends Error {
  constructor(readonly code: VoiceMusicErrorCode, message: string) {
    super(message);
    this.name = 'VoiceMusicError';
  }
}
export class VoiceMusicAcceptedUnconfirmedError extends Error {
  constructor(readonly requestId: string, cause: unknown) {
    super('The Bot accepted the music operation, but the resulting state could not be confirmed.', { cause });
    this.name = 'VoiceMusicAcceptedUnconfirmedError';
  }
}
export type VoiceMusicState =
  | { status: 'unavailable'; reason: 'authenticated-transport-missing' | 'bot-unavailable' }
  | { status: 'available'; track: VoiceMusicTrack | null; queue: readonly VoiceMusicQueueItem[];
      /** Total upcoming items when only a bounded prefix fits the response. */
      queueTotal?: number; positionMs: number;
      volume: number; paused: boolean; shuffle: boolean; repeat: 'off' | 'one' | 'all'; permissions: VoiceMusicPermissions;
      /** Ruling AI: the Bot's elapsed ms since `positionMs` was the playhead, as of the snapshot (absent from an older Bot), and
       *  this device's clock (Date.now) when the snapshot arrived. The playhead runs on from both without trusting either clock. */
      clock?: { elapsedMs: number; receivedAtMs: number } };

/**
 * Ruling AI: the playhead to show at `nowMs` (this device's Date.now): the snapshot's position plus the Bot's elapsed time at
 * the snapshot plus the local time since it arrived, while playing, clamped to the track. Paused, idle or from an older Bot
 * (no stamp): the snapshot's position as it is.
 */
export function musicPlayheadMs(state: Extract<VoiceMusicState, { status: 'available' }>, nowMs: number): number {
  const duration = state.track?.durationMs ?? 0;
  if (state.track === null || state.paused || state.clock === undefined) return Math.min(state.positionMs, duration || state.positionMs);
  const position = state.positionMs + state.clock.elapsedMs + Math.max(0, nowMs - state.clock.receivedAtMs);
  return duration > 0 ? Math.min(position, duration) : position;
}

export interface VoiceMusicAdapter {
  state(channelId: string): VoiceMusicState;
  refresh(channelId: string): Promise<void>;
  subscribe(channelId: string, onChange: () => void): () => void;
  pause(channelId: string, paused: boolean, attempt?: VoiceMusicAttempt): void | Promise<void>;
  skip(channelId: string, attempt?: VoiceMusicAttempt): void | Promise<void>;
  stop(channelId: string, attempt?: VoiceMusicAttempt): void | Promise<void>;
  seek(channelId: string, positionMs: number, attempt?: VoiceMusicAttempt): void | Promise<void>;
  setVolume(channelId: string, volume: number, attempt?: VoiceMusicAttempt): void | Promise<void>;
  setShuffle(channelId: string, enabled: boolean, attempt?: VoiceMusicAttempt): void | Promise<void>;
  setRepeat(channelId: string, repeat: 'off' | 'one' | 'all', attempt?: VoiceMusicAttempt): void | Promise<void>;
  remove(channelId: string, queueItemId: string, attempt?: VoiceMusicAttempt): void | Promise<void>;
  move(channelId: string, queueItemId: string, destinationIndex: number, attempt?: VoiceMusicAttempt): void | Promise<void>;
  clearQueue(channelId: string, attempt?: VoiceMusicAttempt): void | Promise<void>;
  search(channelId: string, query: string, attempt?: VoiceMusicAttempt): Promise<readonly VoiceMusicSearchResult[]>;
  enqueue(channelId: string, input: string, attempt?: VoiceMusicAttempt): void | Promise<void>;
  /** Jonas 2026-10-28: Glenn posts "{you} started music in {room}" with the live card in the text channel where /play ran. The Bot
   *  posts only for the requester of the playing track, once per track, where the requester may post. */
  announce(channelId: string, textChannelId: string, attempt?: VoiceMusicAttempt): Promise<void>;
}
export interface VoiceMusicAttempt { requestId: string; expectedScope?: BotInteractionScope; }

const ID = /^[0-9a-f]{32}$/;
const EMPTY: VoiceMusicState = { status: 'unavailable', reason: 'authenticated-transport-missing' };
const adapters = new WeakMap<CoreClient, Map<string, VoiceMusicAdapter>>();

/** One adapter per authenticated client/community, shared by all voice panels in that community. */
export function createVoiceMusicAdapter(client: CoreClient, communityId: string): VoiceMusicAdapter {
  const existing = adapters.get(client)?.get(communityId);
  if (existing !== undefined) return existing;
  if (!ID.test(communityId)) throw new Error('Invalid community id.');
  const states = new Map<string, VoiceMusicState>();
  const revisions = new Map<string, string>();
  const staleChannels = new Set<string>();
  const listeners = new Map<string, Set<() => void>>();
  const refreshStarted = new Map<string, number>();
  const refreshTasks = new Map<string, Promise<void>>();
  const refreshing = new Set<string>();
  const interactions = createBotInteractionClient(client, communityId);
  const channelScopes = new Map<string, BotInteractionScope>();
  interface MusicAttemptRecord { channelId: string; operation: number; args: readonly (string | number | Uint8Array | null)[];
    revision: string; scope: BotInteractionScope | undefined; sent: boolean; acceptedRevision: string | null; }
  const attempts = new Map<string, MusicAttemptRecord>();
  let timer: ReturnType<typeof setInterval> | null = null;

  const publish = (channelId: string, state: VoiceMusicState) => {
    states.set(channelId, state);
    for (const listener of [...(listeners.get(channelId) ?? [])]) listener();
  };
  const applySnapshot = (channelId: string, snapshot: ReturnType<typeof decodeSnapshot>, scope: BotInteractionScope): void => {
    const revision = revisions.get(channelId);
    if (sameInteractionIdentity(channelScopes.get(channelId), scope) && revision !== undefined
      && BigInt(revision) > BigInt(snapshot.revision)) return;
    channelScopes.set(channelId, scope);
    revisions.set(channelId, snapshot.revision);
    publish(channelId, snapshot.state);
  };
  const ensurePolling = () => {
    if (timer === null) {
      timer = setInterval(() => {
        for (const channelId of listeners.keys()) {
          if (states.get(channelId)?.status !== 'available' && Date.now() - (refreshStarted.get(channelId) ?? 0) >= 5_000) {
            refreshSubscribed(channelId);
          }
        }
      }, 500);
    }
  };
  const request = async (channelId: string, operation: number, args: readonly (string | number | Uint8Array | null)[], revision: string | null, attempt?: VoiceMusicAttempt) => {
    if (!ID.test(channelId)) throw new Error('Invalid voice channel id.');
    const result = await interactions.request({ requestId: attempt?.requestId ?? newRequestId(), operation, channelId, args, revision,
      expectedScope: attempt?.expectedScope });
    const response: InteractionResponse = { requestId: result.requestId, channelId, operation, status: result.status, revision: result.revision, data: result.data };
    if (operation === 10 && response.status === 0) {
      const snapshot = decodeSnapshot(response.data);
      if (response.revision === null || snapshot.revision !== response.revision) throw new Error('Music snapshot revision mismatch.');
      applySnapshot(channelId, snapshot, result.scope);
    } else if (operation === 10) publish(channelId, { status: 'unavailable', reason: 'bot-unavailable' });
    return response;
  };
  const refresh = (channelId: string): Promise<void> => {
    const existing = refreshTasks.get(channelId);
    if (existing !== undefined) return existing;
    const task = (async () => {
      const response = await request(channelId, 10, [], null);
      if (response.status !== 0) throw responseError(response.status);
      if (response.revision === null || BigInt(response.revision) > BigInt(revisions.get(channelId) ?? '0')) {
        throw new Error('The Bot music snapshot revision does not match its authenticated operation response.');
      }
      const state = states.get(channelId);
      if (state?.status !== 'available') throw new Error('The Bot did not return a music snapshot.');
      staleChannels.delete(channelId);
    })().finally(() => { if (refreshTasks.get(channelId) === task) refreshTasks.delete(channelId); });
    refreshTasks.set(channelId, task);
    return task;
  };
  const refreshSubscribed = (channelId: string): void => {
    if (refreshing.has(channelId)) return;
    refreshing.add(channelId);
    refreshStarted.set(channelId, Date.now());
    void refresh(channelId).catch(() => publish(channelId, { status: 'unavailable', reason: 'bot-unavailable' }))
      .finally(() => { refreshing.delete(channelId); });
  };
  const mutate = async (channelId: string, operation: number, args: readonly (string | number | Uint8Array | null)[], attempt?: VoiceMusicAttempt) => {
    const immediate = operation === 13 || operation === 15;
    let record = attempt === undefined ? undefined : attempts.get(attempt.requestId);
    if (record !== undefined && (record.channelId !== channelId || record.operation !== operation)) {
      throw new Error('The music attempt id is already bound to another operation.');
    }
    if (record === undefined && attempt !== undefined && !immediate) {
      let prior: BotInteractionResult | null;
      try { prior = await interactions.read(attempt.requestId, operation); }
      catch (error) { throw new VoiceMusicAcceptedUnconfirmedError(attempt.requestId, error); }
      if (prior !== null) {
        if (prior.status === 5 && prior.revision === null) throw definiteMusicFailure(operation);
        if (prior.status === 5) throw new VoiceMusicAcceptedUnconfirmedError(attempt.requestId, new Error('The Bot reported an ambiguous persisted outcome.'));
        if (prior.status !== 0) throw responseError(prior.status, operation === 12 && isSpotifyInput(args[0]));
        try {
          const receipt = decode(prior.data);
          if (!Array.isArray(receipt) || receipt.length !== 0 || prior.revision === null || !/^(?:0|[1-9]\d*)$/u.test(prior.revision))
            throw new Error('Malformed music mutation receipt.');
        } catch (error) {
          try { await refresh(channelId); } catch { /* the request ID remains reserved for a later reconciliation */ }
          throw new VoiceMusicAcceptedUnconfirmedError(attempt.requestId, error);
        }
        try { await refresh(channelId); }
        catch (error) { throw new VoiceMusicAcceptedUnconfirmedError(attempt.requestId, error); }
        if (BigInt(revisions.get(channelId) ?? '0') < BigInt(prior.revision))
          throw new VoiceMusicAcceptedUnconfirmedError(attempt.requestId, new Error('The accepted music revision is not yet visible.'));
        return;
      }
    }
    if (record === undefined) {
      const current = states.get(channelId);
      if (!immediate && (current?.status !== 'available' || staleChannels.has(channelId))) await refresh(channelId);
      const before = states.get(channelId);
      if (!immediate && before?.status !== 'available') throw new Error('Music state is unavailable.');
      if ((operation === 21 || operation === 22) && !(before?.status === 'available' && before.queue.some(item => item.id === args[0])))
        throw new Error('That queue item is no longer available. Refresh the queue and choose it again.');
      if (operation === 22 && args[1] !== null && args[1] !== undefined
        && !(before?.status === 'available' && before.queue.some(item => item.id === args[1] && item.id !== args[0])))
        throw new Error('The destination queue item is no longer available. Refresh the queue and choose it again.');
      const scope = attempt?.expectedScope ?? channelScopes.get(channelId);
      if (!immediate && scope === undefined) throw new Error('The Bot music state has no authenticated installation scope.');
      // Bind every press to the item the person saw, including an empty queue.
      // An unavailable panel retains the legacy unconditional skip contract.
      const boundArgs = operation === 15 && before?.status === 'available' ? [before.track?.id ?? ''] : args;
      record = { channelId, operation, args: Object.freeze(boundArgs.map(value => value instanceof Uint8Array ? value.slice() : value)),
        revision: immediate ? '0' : revisions.get(channelId) ?? '0', scope: scope === undefined ? undefined : Object.freeze({ ...scope }), sent: false, acceptedRevision: null };
      if (attempt !== undefined) {
        attempts.set(attempt.requestId, record);
        while (attempts.size > 128) attempts.delete(attempts.keys().next().value!);
      }
    }
    let result: InteractionResponse;
    if (attempt !== undefined && record.sent) {
      let reconciled: BotInteractionResult | null;
      try { reconciled = await interactions.read(attempt.requestId, operation); }
      catch (error) { throw new VoiceMusicAcceptedUnconfirmedError(attempt.requestId, error); }
      if (reconciled === null) throw new VoiceMusicAcceptedUnconfirmedError(attempt.requestId, new Error('No durable result is available yet.'));
      if (record.scope !== undefined && !sameInteractionIdentity(reconciled.scope, record.scope)) {
        try { await refresh(channelId); } catch { /* a new read still uses the live installation scope */ }
        throw new VoiceMusicAcceptedUnconfirmedError(attempt.requestId, new Error('The original music attempt is bound to a previous Bot installation.'));
      }
      record.scope = reconciled.scope;
      result = { requestId: reconciled.requestId, channelId, operation, status: reconciled.status, revision: reconciled.revision, data: reconciled.data };
    } else {
      if (attempt !== undefined) record.sent = true;
      try {
        const sentRecord = record;
        const resultValue = await interactions.request({ requestId: attempt?.requestId ?? newRequestId(), operation, channelId,
          args: record.args, revision: record.revision, expectedScope: record.scope,
          onDispatch: scope => { sentRecord.scope = scope; } });
        record.scope ??= resultValue.scope;
        result = { requestId: resultValue.requestId, channelId, operation, status: resultValue.status, revision: resultValue.revision, data: resultValue.data };
      } catch (error) {
        if (attempt !== undefined && (error instanceof BotInteractionRejectedError || error instanceof BotInteractionBindingMismatchError)) attempts.delete(attempt.requestId);
        if (immediate && attempt !== undefined && !(error instanceof BotInteractionRejectedError || error instanceof BotInteractionBindingMismatchError)
          && (!(error instanceof BotInteractionUncertainError) || error.uploadCallFailed)) {
          // A reload loses the in-memory args/expiry. The durable send intent
          // refuses a changed resend; recover its authenticated receipt instead.
          // An unresolved upload reply or a lost response stays uncertain until retry.
          let prior: BotInteractionResult | null;
          try { prior = await interactions.read(attempt.requestId, operation); }
          catch (readError) { throw new VoiceMusicAcceptedUnconfirmedError(attempt.requestId, readError); }
          if (prior === null) throw new VoiceMusicAcceptedUnconfirmedError(attempt.requestId, error);
          if (record.scope !== undefined && !sameInteractionIdentity(prior.scope, record.scope))
            throw new VoiceMusicAcceptedUnconfirmedError(attempt.requestId, new Error('The original music attempt is bound to a previous Bot installation.'));
          record.scope = prior.scope;
          result = { requestId: prior.requestId, channelId, operation, status: prior.status, revision: prior.revision, data: prior.data };
        } else {
          if (error instanceof Error && 'requestId' in error) throw error;
          if (attempt !== undefined) throw new VoiceMusicAcceptedUnconfirmedError(attempt.requestId, error);
          throw error;
        }
      }
    }
    if (result.status === 5 && result.revision === null) {
      if (attempt !== undefined) attempts.delete(attempt.requestId);
      throw definiteMusicFailure(operation);
    }
    if (result.status === 5) {
      record.sent = true;
      throw new VoiceMusicAcceptedUnconfirmedError(attempt?.requestId ?? result.requestId, new Error('The Bot reported an ambiguous persisted outcome.'));
    }
    if (result.status === 4) {
      if (attempt !== undefined) attempts.delete(attempt.requestId);
      // A conflict means our cached revision is stale. Refresh before rejecting so the next action can proceed.
      staleChannels.add(channelId);
      await refresh(channelId);
      throw responseError(result.status);
    }
    if (result.status !== 0) {
      if (attempt !== undefined) attempts.delete(attempt.requestId);
      throw responseError(result.status, operation === 12 && isSpotifyInput(record.args[0]));
    }
    if (immediate && result.data.length > 1) {
      try {
        const snapshot = decodeSnapshot(result.data);
        if (result.revision === null || snapshot.revision !== result.revision) throw new Error('Music control snapshot revision mismatch.');
        if (record.scope === undefined) throw new Error('Music control snapshot scope missing.');
        applySnapshot(channelId, snapshot, record.scope);
        staleChannels.delete(channelId);
        if (attempt !== undefined) attempts.delete(attempt.requestId);
        return;
      } catch (error) {
        throw new VoiceMusicAcceptedUnconfirmedError(attempt?.requestId ?? result.requestId, error);
      }
    }
    try {
      const receipt = decode(result.data);
      if (!Array.isArray(receipt) || receipt.length !== 0 || result.revision === null || !/^(?:0|[1-9]\d*)$/u.test(result.revision)
        || BigInt(result.revision) <= BigInt(record.revision))
        throw new Error('Malformed music mutation receipt.');
    } catch (error) {
      record.sent = true;
      record.acceptedRevision = result.revision;
      try { await refresh(channelId); } catch { /* keep the malformed receipt uncertain; a later call may observe state */ }
      throw new VoiceMusicAcceptedUnconfirmedError(attempt?.requestId ?? result.requestId, error);
    }
    record.acceptedRevision = result.revision;
    // A successful operation response is not the accepted state. Wait for an authenticated state read
    // after the operation result so the panel only settles when the server-confirmed snapshot arrives.
    try { await refresh(channelId); }
    catch (error) { throw new VoiceMusicAcceptedUnconfirmedError(attempt?.requestId ?? result.requestId, error); }
    if (record.scope === undefined || !sameInteractionIdentity(channelScopes.get(channelId), record.scope))
      throw new VoiceMusicAcceptedUnconfirmedError(attempt?.requestId ?? result.requestId, new Error('The Bot installation changed before the accepted music state could be confirmed.'));
    if (record.acceptedRevision !== null) {
      const after = states.get(channelId);
      if (after?.status !== 'available' || BigInt(revisions.get(channelId) ?? '0') < BigInt(record.acceptedRevision)) {
        throw new VoiceMusicAcceptedUnconfirmedError(attempt?.requestId ?? result.requestId,
          new Error('The Bot operation succeeded but its resulting music state is not yet visible.'));
      }
    }
    if (attempt !== undefined) attempts.delete(attempt.requestId);
  };
  const adapter: VoiceMusicAdapter = {
    state: channelId => states.get(channelId) ?? EMPTY,
    refresh,
    subscribe(channelId, onChange) {
      const channelListeners = listeners.get(channelId) ?? new Set<() => void>();
      channelListeners.add(onChange);
      listeners.set(channelId, channelListeners);
      ensurePolling();
      refreshSubscribed(channelId);
      return () => {
        channelListeners.delete(onChange);
        if (channelListeners.size === 0) { listeners.delete(channelId); refreshStarted.delete(channelId); }
        if (listeners.size === 0 && timer !== null) { clearInterval(timer); timer = null; }
      };
    },
    pause: (id, paused, attempt) => mutate(id, paused ? 13 : 14, [], attempt),
    skip: (id, attempt) => mutate(id, 15, [], attempt),
    stop: (id, attempt) => mutate(id, 16, [], attempt),
    seek: (id, ms, attempt) => mutate(id, 17, [safeInteger(ms, 'seek position')], attempt),
    setVolume: (id, value, attempt) => mutate(id, 18, [bounded(value, 0, 100, 'volume')], attempt),
    setShuffle: (id, enabled, attempt) => {
      const current = states.get(id);
      return current?.status === 'available' && current.shuffle === enabled && (attempt === undefined || !attempts.has(attempt.requestId))
        ? undefined : mutate(id, 19, [], attempt);
    },
    setRepeat: (id, repeat, attempt) => mutate(id, 20, [repeat === 'off' ? 0 : repeat === 'one' ? 1 : 2], attempt),
    remove: (id, queueId, attempt) => mutate(id, 21, [queueId], attempt),
    clearQueue: (id, attempt) => mutate(id, 23, [], attempt),
    search: async (id, query, attempt) => {
      const response = await request(id, 11, [validatedInput(query, 200, 'search query')], null, attempt);
      if (response.status !== 0) throw responseError(response.status);
      return decodeSearchResults(response.data);
    },
    enqueue: (id, input, attempt) => mutate(id, 12, [validatedInput(input, 2048, 'queue input')], attempt),
    announce: async (id, textChannelId, attempt) => {
      if (!ID.test(id) || !ID.test(textChannelId)) throw new Error('Invalid music announcement channel.');
      const result = await interactions.request({ requestId: attempt?.requestId ?? newRequestId(), operation: 25, channelId: id, args: [textChannelId],
        revision: '0', ...(attempt?.expectedScope === undefined ? {} : { expectedScope: attempt.expectedScope }) });
      if (result.status !== 0) throw responseError(result.status);
    },
    move: (id, queueId, destinationIndex, attempt) => {
      const existing = attempt === undefined ? undefined : attempts.get(attempt.requestId);
      if (existing !== undefined) return mutate(id, 22, existing.args, attempt);
      const state = states.get(id);
      if (state?.status !== 'available') return Promise.reject(new Error('Music state is unavailable.'));
      const destination = safeInteger(destinationIndex, 'queue position');
      if (destination >= state.queue.length) return Promise.reject(new Error('Choose a position within the current queue.'));
      const queue = state.queue.filter(item => item.id !== queueId);
      const target = Math.min(queue.length, destination);
      const before = queue[target];
      return mutate(id, 22, [queueId, before === undefined ? null : before.id], attempt);
    },
  };
  const communityAdapters = adapters.get(client) ?? new Map<string, VoiceMusicAdapter>();
  communityAdapters.set(communityId, adapter);
  adapters.set(client, communityAdapters);
  return adapter;
}

/** Compatibility export remains unavailable until a page supplies its authenticated CoreClient. */
export const voiceMusicAdapter: VoiceMusicAdapter = {
  state: () => EMPTY, refresh: () => Promise.reject(new Error('Authenticated voice music transport is unavailable.')), subscribe: () => () => undefined,
  pause: unavailable, skip: unavailable, stop: unavailable, seek: unavailable, setVolume: unavailable,
  setShuffle: unavailable, setRepeat: unavailable, remove: unavailable, move: unavailable, clearQueue: unavailable,
  search: unavailable, enqueue: unavailable, announce: unavailable,
};

interface InteractionResponse { requestId: string; channelId: string | null; operation: number; status: number; revision: string | null; data: Uint8Array; }
function unavailable(): Promise<never> { return Promise.reject(new Error('Authenticated voice music transport is unavailable.')); }
function sameInteractionIdentity(left: BotInteractionScope | undefined, right: BotInteractionScope): boolean {
  return left !== undefined && left.installationId === right.installationId && left.botUserId === right.botUserId;
}
function responseError(status: number, spotifyInput = false): Error {
  if (spotifyInput && status === 5) return new VoiceMusicError('spotify-unavailable', 'Spotify is unavailable or not linked for this community.');
  if (spotifyInput && status === 6) return new VoiceMusicError('spotify-busy', 'Spotify playback is already active in another voice channel.');
  return status === 2 ? new VoiceMusicError('forbidden', 'You do not have permission to control this Bot.')
    : status === 4 ? new VoiceMusicError('conflict', 'Music state changed. Refresh and try again.')
      : new VoiceMusicError('operation-unavailable', 'The Bot could not do that right now. Try again.');
}
function isSpotifyInput(value: unknown): boolean {
  if (typeof value !== 'string') return false;
  if (/^spotify:(?:track|album|playlist):/iu.test(value.trim())) return true;
  try {
    const host = new URL(value).hostname.toLowerCase();
    return host === 'spotify.link' || host === 'spotify.com' || host.endsWith('.spotify.com');
  } catch { return false; }
}
function newRequestId(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  if (bytes.every(value => value === 0)) bytes[15] = 1;
  return toHex(bytes);
}
function toHex(bytes: Uint8Array): string { return Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join(''); }
function safeInteger(value: number, label: string): number {
  if (!Number.isSafeInteger(value) || value < 0) throw new Error(`Invalid ${label}.`);
  return value;
}
function bounded(value: number, min: number, max: number, label: string): number {
  const number = safeInteger(value, label);
  if (number < min || number > max) throw new Error(`Invalid ${label}.`);
  return number;
}
function field(row: CborValue[], index: number): CborValue {
  const value = row[index];
  if (value === undefined) throw new Error('Malformed music snapshot.');
  return value;
}
function array(value: CborValue, length?: number): CborValue[] {
  if (!Array.isArray(value) || (length !== undefined && value.length !== length)) throw new Error('Malformed music snapshot.');
  return value;
}
function uint(value: CborValue, max = Number.MAX_SAFE_INTEGER): number {
  if (typeof value !== 'bigint' || value > BigInt(max)) throw new Error('Malformed music snapshot.');
  return Number(value);
}
function text(value: CborValue): string {
  if (typeof value !== 'string') throw new Error('Malformed music snapshot.');
  return value;
}
function track(value: CborValue): VoiceMusicTrack | null {
  if (value === null) return null;
  const row = array(value);
  // Fix round 2: a newer Bot may append slots; the eight this client reads must be there, the rest is ignored.
  if (row.length < 8) throw new Error('Malformed music snapshot.');
  const provider = text(field(row, 1));
  const canonicalId = text(field(row, 2));
  const itemId = text(field(row, 0));
  const supported = provider === 'youtube' && /^[A-Za-z0-9_-]{11}$/u.test(canonicalId)
    || provider === 'spotify' && /^spotify:track:[A-Za-z0-9]{22}$/u.test(canonicalId);
  if (!supported || !ID.test(itemId)) throw new Error('Unsupported music track.');
  const album = text(field(row, 5));
  const result: VoiceMusicTrack = { id: itemId, title: text(field(row, 3)), artist: text(field(row, 4)), durationMs: uint(field(row, 6)),
    provider: provider === 'spotify' ? 'spotify' : 'youtube' };
  if (album !== '') result.album = album;
  // Slot 7 is the requester the Bot recorded (a user id; other markers, such as a search placeholder, are not a person).
  const requester = row[7];
  if (typeof requester === 'string' && ID.test(requester)) result.requesterId = requester;
  return result;
}
function decodeSnapshot(data: Uint8Array): { state: VoiceMusicState; revision: string } {
  const row = array(decode(data));
  // v1 has 10 slots; a Bot with Ruling AI adds slot 10 (Unix ms when slot 4 was the playhead, 0 unknown) and 11 (its Unix ms
  // now). Fix round 2: tolerant of later additions — at least 10, the stamp read only when both of its slots are there.
  if (row.length < 10) throw new Error('Malformed music snapshot.');
  if (uint(field(row, 0), 1) !== 1) throw new Error('Unsupported music snapshot version.');
  const revisionValue = field(row, 1);
  if (typeof revisionValue !== 'bigint') throw new Error('Malformed music snapshot.');
  const revision = revisionValue.toString();
  const current = track(field(row, 2));
  const queue = array(field(row, 3)).map(value => {
    const decoded = track(value);
    if (decoded === null) throw new Error('Malformed music queue.');
    return { id: decoded.id, track: decoded };
  });
  const queueTotal = row.length > 12 ? uint(field(row, 12), 500) : queue.length;
  if (queueTotal < queue.length) throw new Error('Malformed music queue total.');
  const repeat = uint(field(row, 8), 2);
  const canControl = uint(field(row, 9), 1) === 1;
  return { revision, state: { status: 'available', track: current, queue, ...(queueTotal > queue.length ? { queueTotal } : {}), positionMs: uint(field(row, 4)), paused: uint(field(row, 5), 1) === 1,
    volume: uint(field(row, 6), 100), shuffle: uint(field(row, 7), 1) === 1,
    repeat: repeat === 0 ? 'off' : repeat === 1 ? 'one' : 'all', permissions: { control: canControl, manageQueue: canControl },
    ...playheadClock(row) } };
}
function playheadClock(row: CborValue[]): { clock?: { elapsedMs: number; receivedAtMs: number } } {
  if (row.length < 12) return {};
  const at = uint(field(row, 10));
  const now = uint(field(row, 11));
  if (at === 0 || now < at) return {};
  return { clock: { elapsedMs: now - at, receivedAtMs: Date.now() } };
}
function decodeSearchResults(data: Uint8Array): VoiceMusicSearchResult[] {
  const result = array(decode(data), 2);
  if (uint(field(result, 0), 1) !== 1) throw new Error('Unsupported music search response.');
  return array(field(result, 1)).map(value => {
    const row = array(value, 8);
    const id = text(field(row, 0));
    const provider = text(field(row, 1));
    const canonicalId = text(field(row, 2));
    const requesterId = text(field(row, 7));
    if (provider !== 'youtube' || !/^[A-Za-z0-9_-]{11}$/u.test(canonicalId) || id !== `yt:${canonicalId}`) {
      throw new Error('Unsupported music search result.');
    }
    if (requesterId !== '') throw new Error('Malformed music search result.');
    const album = text(field(row, 5));
    const track: VoiceMusicSearchResult = {
      id, title: text(field(row, 3)), artist: text(field(row, 4)), durationMs: uint(field(row, 6)),
      input: `https://www.youtube.com/watch?v=${canonicalId}`,
    };
    if (album !== '') track.album = album;
    return track;
  });
}
function validatedInput(value: string, limit: number, label: string): string {
  const trimmed = value.trim();
  if (trimmed.length === 0 || new TextEncoder().encode(trimmed).length > limit || /[\u0000-\u001f\u007f]/u.test(trimmed)) {
    throw new Error(`Invalid music ${label}.`);
  }
  return trimmed;
}

function definiteMusicFailure(operation: number): VoiceMusicError {
  return operation === 12
    ? new VoiceMusicError('unavailable', 'The Bot could not load that track.')
    : new VoiceMusicError('operation-unavailable', 'The Bot could not do that right now. Try again.');
}
