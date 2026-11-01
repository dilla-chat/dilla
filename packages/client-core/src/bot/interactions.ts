import type { CoreClient } from '../bridge';

const ID = /^[0-9a-f]{32}$/u;
const POLL_INTERVAL_MS = 200;
const REQUEST_TIMEOUT_MS = 30_000;

export interface BotInteractionResult {
  requestId: string;
  status: number;
  revision: string | null;
  data: Uint8Array;
  /** Live installation identity checked before dispatch and after response delivery. */
  readonly scope: BotInteractionScope;
}

export interface BotInteractionScope {
  readonly installationId: string;
  readonly botUserId: string | null;
  readonly revision: string;
}

export interface BotInteractionRequest {
  operation: number;
  channelId?: string | null;
  args: readonly (string | number | Uint8Array | null)[];
  revision?: string | null;
  requestId?: string;
  /** Optional capability-authority scope checked before dispatch and before returning. */
  expectedScope?: BotInteractionScope;
  /** Local callback before upload, after current installation authorization. Never sent on the wire. */
  onDispatch?: (scope: BotInteractionScope) => void;
}

export class BotInteractionUncertainError extends Error {
  constructor(readonly requestId: string, message: string,
    /** The worker upload call rejected; unresolved replies and response loss leave this false. */
    readonly uploadCallFailed = false) {
    super(message);
    this.name = 'BotInteractionUncertainError';
  }
}

export class BotInteractionRejectedError extends Error {
  constructor(readonly requestId: string) {
    super('The Bot interaction request was rejected before the server confirmed its upload.');
    this.name = 'BotInteractionRejectedError';
  }
}

export class BotInteractionBindingMismatchError extends Error {
  constructor() {
    super('The Bot interaction capability binding changed before dispatch.');
    this.name = 'BotInteractionBindingMismatchError';
  }
}

export class BotInteractionIdentityConflictError extends Error {
  constructor(readonly requestId: string) {
    super('The Bot interaction request id is already bound to a different operation or request body.');
    this.name = 'BotInteractionIdentityConflictError';
  }
}

export class BotInteractionUnverifiableRequestError extends Error {
  constructor(readonly requestId: string) {
    super('The Bot interaction request id already has a reconciled response, but its original request body is unknown. Use read() to reconcile it.');
    this.name = 'BotInteractionUnverifiableRequestError';
  }
}

export interface BotInteractionClient {
  request(input: BotInteractionRequest): Promise<BotInteractionResult>;
  read(requestId: string, operation: number): Promise<BotInteractionResult | null>;
}

const clients = new WeakMap<CoreClient, Map<string, BotInteractionClient>>();
const REQUEST_FINGERPRINT_LIMIT = 128;

/**
 * Shared typed transport for authenticated Bot interactions. Domain response statuses are returned
 * unchanged in `BotInteractionResult.status`; callers decide how to present each operation's status.
 * An uncertain upload or response timeout carries the original request id so reconciliation can
 * continue with `read` after reload without issuing a second mutation. The adapter never retries a
 * timed out mutation because the original request body and expiry are bound to that id in the core.
 */
export function createBotInteractionClient(client: CoreClient, communityId: string) {
  if (!ID.test(communityId)) throw new Error('Invalid community id.');
  const existing = clients.get(client)?.get(communityId);
  if (existing !== undefined) return existing;
  const results = new Map<string, BotInteractionResult>();
  const uncertain = new Map<string, BotInteractionUncertainError | BotInteractionRejectedError>();
  const requestFingerprints = new Map<string, { fingerprint: string; verified: boolean }>();
  // A shared read task includes the final authorization recheck. Store only that fully checked
  // promise so no coalesced waiter can observe the inner, not-yet-scoped response.
  const reads = new Map<string, Promise<BotInteractionResult | null>>();
  // Track each public read call from entry, including its initial authorization lookup. A set per
  // key makes cleanup owner-safe when concurrent reads overlap or a scope reset detaches old work.
  const activeReadOwners = new Map<string, Set<symbol>>();
  const requests = new Map<string, Promise<BotInteractionResult>>();
  const keyOf = (requestId: string, operation: number): string => `${requestId}:${operation}`;
  const resultCopy = (row: BotInteractionResult): BotInteractionResult => ({ ...row, data: row.data.slice(), scope: Object.freeze({ ...row.scope }) });
  let accountId = client.get?.('account')?.user?.id ?? null;
  let authorizationScope: string | null = null;
  let accessScopeKey: string | null = null;
  let scopeGeneration = 0;
  const clearScopedResults = (): void => {
    scopeGeneration++;
    results.clear(); uncertain.clear(); reads.clear(); activeReadOwners.clear(); requests.clear(); requestFingerprints.clear();
  };
  const updateAccountScope = (): void => {
    const nextAccount = client.get?.('account')?.user?.id ?? null;
    if (nextAccount !== accountId) {
      accountId = nextAccount;
      authorizationScope = null;
      clearScopedResults();
    }
  };
  const localAccessScope = (): { key: string } => {
    updateAccountScope();
    const account = client.get?.('account');
    if (account?.phase !== 'ready' || account.user === null || account.deviceId === null) {
      throw new Error('The Bot interaction transport requires a ready account and device.');
    }
    const communities = client.get?.('communities') ?? [];
    if (!communities.some(row => row.id === communityId)) throw new Error('The Bot interaction community is no longer accessible.');
    const permissions = client.get?.(`permissions:${communityId}`);
    return { key: JSON.stringify([account.instance?.id ?? null, account.user.id, account.deviceId,
      communities.find(row => row.id === communityId), permissions ?? null]) };
  };
  const observeAccessScope = (): void => {
    try {
      const current = localAccessScope().key;
      if (accessScopeKey !== null && current !== accessScopeKey) clearScopedResults();
      accessScopeKey = current;
    } catch {
      if (accessScopeKey !== null) clearScopedResults();
      accessScopeKey = null;
    }
  };
  client.subscribe?.('account', () => { updateAccountScope(); observeAccessScope(); });
  const ensureInstallationScope = async (): Promise<{ generation: number; scope: BotInteractionScope }> => {
    const before = localAccessScope();
    const generation = scopeGeneration;
    const installation = await client.call({ m: 'getBotCommandStatus', communityId }) as {
      installationId?: unknown; botUserId?: unknown; state?: unknown; revision?: unknown;
    } | null;
    let after: { key: string };
    try { after = localAccessScope(); }
    catch (error) { clearScopedResults(); accessScopeKey = null; authorizationScope = null; throw error; }
    if (generation !== scopeGeneration || before.key !== after.key) {
      clearScopedResults();
      throw new Error('The Bot interaction account or community access changed during authorization.');
    }
    const installKey = installation === null ? null : JSON.stringify([installation.installationId, installation.botUserId,
      installation.state, installation.revision]);
    const nextScope = JSON.stringify([after.key, installKey]);
    if (authorizationScope !== null && nextScope !== authorizationScope) clearScopedResults();
    authorizationScope = nextScope;
    accessScopeKey = after.key;
    if (installation === null || typeof installation.installationId !== 'string' || !ID.test(installation.installationId)
      || installation.state !== 3 || installation.botUserId !== null && (typeof installation.botUserId !== 'string' || !ID.test(installation.botUserId))
      || typeof installation.revision !== 'string' || !/^(?:0|[1-9]\d*)$/u.test(installation.revision)) {
      throw new Error('The Bot interaction installation is not active.');
    }
    return { generation: scopeGeneration, scope: {
      installationId: installation.installationId, botUserId: installation.botUserId, revision: installation.revision,
    } };
  };
  const sameScope = (left: BotInteractionScope, right: BotInteractionScope): boolean => left.installationId === right.installationId
    && left.botUserId === right.botUserId && left.revision === right.revision;
  const assertRequestScope = (generation: number): void => {
    const current = localAccessScope().key;
    if (generation !== scopeGeneration || current !== accessScopeKey) {
      clearScopedResults();
      throw new BotInteractionUncertainError('', 'The Bot interaction account or community access changed while the request was pending.');
    }
  };
  const rememberResult = (key: string, result: BotInteractionResult): void => {
    results.delete(key);
    results.set(key, result);
    while (results.size > 128) {
      const oldest = results.keys().next().value;
      if (oldest === undefined) break;
      results.delete(oldest);
    }
  };

  const requestKeyPrefix = (requestId: string): string => `${requestId}:`;
  const activeRequest = (requestId: string): boolean => {
    const prefix = requestKeyPrefix(requestId);
    return [...requests.keys()].some(key => key.startsWith(prefix));
  };
  const rememberFingerprint = (requestId: string, fingerprint: string): void => {
    const existing = requestFingerprints.get(requestId);
    if (existing !== undefined) {
      if (existing.fingerprint !== fingerprint) throw new BotInteractionIdentityConflictError(requestId);
      requestFingerprints.delete(requestId);
      requestFingerprints.set(requestId, existing);
      return;
    }
    if (requestFingerprints.size >= REQUEST_FINGERPRINT_LIMIT) {
      const evict = [...requestFingerprints.keys()].find(candidate => !activeRequest(candidate));
      if (evict === undefined) throw new Error('Too many Bot interaction requests are active.');
      requestFingerprints.delete(evict);
      const prefix = requestKeyPrefix(evict);
      for (const key of results.keys()) if (key.startsWith(prefix)) results.delete(key);
      for (const key of uncertain.keys()) if (key.startsWith(prefix)) uncertain.delete(key);
    }
    requestFingerprints.set(requestId, { fingerprint, verified: false });
  };
  const snapshotRequest = (input: BotInteractionRequest): BotInteractionRequest => {
    if (!Number.isSafeInteger(input.operation) || input.operation < 0 || !Array.isArray(input.args)) {
      throw new Error('Invalid Bot interaction request identity.');
    }
    const args = Object.freeze(input.args.map((value: BotInteractionRequest['args'][number]): BotInteractionRequest['args'][number] =>
      value instanceof Uint8Array ? new Uint8Array(value) : value));
    const expectedScope = input.expectedScope === undefined ? undefined : Object.freeze({ ...input.expectedScope });
    return Object.freeze({ operation: input.operation, channelId: input.channelId ?? null, args,
      revision: input.revision ?? null, onDispatch: input.onDispatch, ...(expectedScope === undefined ? {} : { expectedScope }) });
  };
  const requestFingerprint = (input: BotInteractionRequest): string => {
    const args = input.args.map(value => {
      if (value === null) return ['null'];
      if (typeof value === 'string') return ['string', value];
      if (typeof value === 'number') return ['number', Object.is(value, -0) ? '-0' : String(value)];
      if (value instanceof Uint8Array) return ['bytes', Array.from(value)];
      throw new Error('Invalid Bot interaction request argument.');
    });
    return JSON.stringify([input.operation, input.channelId ?? null, input.revision ?? null, args]);
  };

  const readOnce = async (requestId: string, operation: number, generation = scopeGeneration): Promise<Omit<BotInteractionResult, 'scope'> | null> => {
    if (!ID.test(requestId) || !Number.isSafeInteger(operation) || operation < 0) throw new Error('Invalid Bot interaction identity.');
    const key = keyOf(requestId, operation);
    const prior = results.get(key);
    if (prior !== undefined) return resultCopy(prior);
    const page = await client.call({ m: 'readBotInteractionResponses', communityId, operations: [operation], requestIds: [requestId] }) as {
      responses: (Omit<BotInteractionResult, 'scope'> & { operation: number })[]; through: string | null;
    };
    const matches = page.responses.filter(row => row.requestId === requestId && row.operation === operation);
    if (matches.length > 1) throw new Error('Duplicate Bot interaction response for the same request id.');
    const row = matches[0];
    if (row === undefined) return null;
    if (generation !== scopeGeneration) return null;
    if (page.through !== null) await client.call({ m: 'ackBotInteractionResponses', communityId, through: page.through });
    if (generation !== scopeGeneration) return null;
    return { requestId: row.requestId, status: row.status, revision: row.revision, data: row.data.slice() };
  };

  const read = async (requestId: string, operation: number): Promise<BotInteractionResult | null> => {
    const key = keyOf(requestId, operation);
    const owner = Symbol();
    const owners = activeReadOwners.get(key) ?? new Set<symbol>();
    owners.add(owner);
    activeReadOwners.set(key, owners);
    try {
      const initial = await ensureInstallationScope();
      const generation = initial.generation;
      const cached = results.get(key);
      if (cached !== undefined) return resultCopy(cached);
      const pending = reads.get(key);
      if (pending !== undefined) {
        const result = await pending;
        return result === null ? null : resultCopy(result);
      }
      const task = (async (): Promise<BotInteractionResult | null> => {
        const result = await readOnce(requestId, operation, generation);
        if (result === null) return null;
        const final = await ensureInstallationScope();
        if (final.generation !== generation) return null;
        const scoped = { ...result, scope: final.scope };
        rememberResult(key, scoped);
        return scoped;
      })();
      reads.set(key, task);
      try { const result = await task; return result === null ? null : resultCopy(result); }
      finally { if (reads.get(key) === task) reads.delete(key); }
    } finally {
      const currentOwners = activeReadOwners.get(key);
      if (currentOwners === owners) {
        owners.delete(owner);
        if (owners.size === 0) activeReadOwners.delete(key);
      }
    }
  };

  const executeRequest = async (input: BotInteractionRequest, requestId: string, fingerprint: string,
    generation: number, scope: BotInteractionScope): Promise<BotInteractionResult> => {
    if (!ID.test(requestId)) throw new Error('Invalid Bot interaction request id.');
    let uploadAccepted = false;
    let uploadCallFailed = false;
    try {
    input.onDispatch?.(Object.freeze({ ...scope }));
    const send = Promise.resolve().then(() => client.call({ m: 'humanInteractionRequest', communityId,
      channelId: input.channelId ?? null, requestId, operation: input.operation, args: input.args,
      revision: input.revision ?? null })).catch(error => {
        uploadCallFailed = true;
        throw error;
      }) as Promise<{ requestId: string; upload: string }>;
    let uploadTimer: ReturnType<typeof setTimeout> | undefined;
    const sent = await Promise.race([
      send,
      new Promise<never>((_, reject) => {
        uploadTimer = setTimeout(() => reject(new BotInteractionUncertainError(requestId, 'Timed out waiting for the Bot interaction upload result.')),
          REQUEST_TIMEOUT_MS);
      }),
    ]).finally(() => { if (uploadTimer !== undefined) clearTimeout(uploadTimer); });
    if (sent.requestId !== requestId) throw new BotInteractionUncertainError(requestId, 'The worker returned a different request id.');
    if (sent.upload === 'rejected') throw new BotInteractionRejectedError(requestId);
    if (sent.upload !== 'accepted') throw new BotInteractionUncertainError(requestId, 'The Bot interaction upload is still unresolved.');
    uploadAccepted = true;
    if (generation !== scopeGeneration) throw new BotInteractionUncertainError(requestId, 'The Bot installation or account changed while the request was pending.');
    const identity = requestFingerprints.get(requestId);
    if (identity === undefined || identity.fingerprint !== fingerprint) {
      throw new BotInteractionUncertainError(requestId, 'The Bot interaction request identity changed while the request was pending.');
    }
    identity.verified = true;

    const deadline = Date.now() + REQUEST_TIMEOUT_MS;
    while (Date.now() < deadline) {
      if (generation !== scopeGeneration) throw new BotInteractionUncertainError(requestId, 'The Bot installation or account changed while the request was pending.');
      const result = await readOnce(requestId, input.operation, generation);
      if (result !== null) {
        try {
          const final = await ensureInstallationScope();
          if (final.generation !== generation || !sameScope(final.scope, scope)) throw new Error('scope changed');
        } catch {
          throw new BotInteractionUncertainError(requestId, 'The Bot installation or account changed while the request was pending.');
        }
        try { assertRequestScope(generation); }
        catch { throw new BotInteractionUncertainError(requestId, 'The Bot interaction account or community access changed while the request was pending.'); }
        const scoped = { ...result, scope };
        rememberResult(keyOf(requestId, input.operation), scoped);
        return scoped;
      }
      const remaining = deadline - Date.now();
      if (remaining > 0) await new Promise<void>(resolve => setTimeout(resolve, Math.min(POLL_INTERVAL_MS, remaining)));
    }
    throw new BotInteractionUncertainError(requestId, 'Timed out waiting for the Bot interaction response.');
    } catch (error) {
      if (error instanceof BotInteractionUncertainError || error instanceof BotInteractionRejectedError
        || error instanceof BotInteractionBindingMismatchError || error instanceof BotInteractionIdentityConflictError
        || error instanceof BotInteractionUnverifiableRequestError) throw error;
      if (!uploadAccepted && isKnownNoEffectWorkerRejection(error)) throw new BotInteractionRejectedError(requestId);
      const detail = error instanceof Error ? error.message : 'The worker interaction transport failed after dispatch.';
      throw new BotInteractionUncertainError(requestId, `The Bot interaction may have been accepted, but its result could not be confirmed: ${detail}`, uploadCallFailed && !uploadAccepted);
    }
  };

  function isKnownNoEffectWorkerRejection(error: unknown): boolean {
    if (typeof error !== 'object' || error === null) return false;
    const code = 'code' in error ? String(error.code) : '';
    if (['E_FORBIDDEN', 'E_NOT_FOUND', 'E_CONFLICT', 'E_BAD_INPUT', 'E_BOT_PERMISSION', 'E_BOT_BAD_INPUT'].includes(code)) return true;
    const status = 'status' in error ? Number(error.status) : NaN;
    return Number.isInteger(status) && status >= 400 && status < 500;
  }

  const request = (input: BotInteractionRequest): Promise<BotInteractionResult> => {
    const requestId = input.requestId ?? [...crypto.getRandomValues(new Uint8Array(16))]
      .map(byte => byte.toString(16).padStart(2, '0')).join('');
    if (!ID.test(requestId)) return Promise.reject(new Error('Invalid Bot interaction request id.'));
    let snapshot: BotInteractionRequest;
    let fingerprint: string;
    try {
      snapshot = snapshotRequest(input);
      fingerprint = requestFingerprint(snapshot);
    } catch (error) { return Promise.reject(error instanceof Error ? error : new Error('Invalid Bot interaction request.')); }
    const key = keyOf(requestId, snapshot.operation);
    return (async () => {
      const initial = await ensureInstallationScope();
      const { generation, scope } = initial;
      if (snapshot.expectedScope !== undefined && !sameScope(scope, snapshot.expectedScope)) {
        throw new BotInteractionBindingMismatchError();
      }
      // A read-only reconciliation can recover the response after reload, but it cannot establish
      // which immutable request body the worker previously accepted for this id. The worker's
      // accepted-upload shortcut is id-only, so never turn that receipt into proof that this
      // caller's body produced the cached response.
      if (!requestFingerprints.has(requestId) && (results.has(key) || reads.has(key) || activeReadOwners.has(key))) {
        throw new BotInteractionUnverifiableRequestError(requestId);
      }
      rememberFingerprint(requestId, fingerprint);
      const identity = requestFingerprints.get(requestId);
      const prior = results.get(key);
      if (identity?.verified === true && prior !== undefined) return resultCopy(prior);
      const pending = requests.get(key);
      if (pending !== undefined) return pending.then(resultCopy);
      const failure = uncertain.get(key);
      if (failure !== undefined) throw failure;
      const task = executeRequest(snapshot, requestId, fingerprint, generation, scope);
      requests.set(key, task);
      void task.catch(error => {
        const currentIdentity = requestFingerprints.get(requestId);
        if (generation === scopeGeneration && currentIdentity?.fingerprint === fingerprint
          && (error instanceof BotInteractionUncertainError || error instanceof BotInteractionRejectedError)) {
          uncertain.delete(key);
          uncertain.set(key, error);
          while (uncertain.size > 128) {
            const oldest = uncertain.keys().next().value;
            if (oldest === undefined) break;
            uncertain.delete(oldest);
          }
        }
      });
      void task.finally(() => { if (requests.get(key) === task) requests.delete(key); }).catch(() => undefined);
      return task.then(resultCopy);
    })();
  };

  const adapter: BotInteractionClient = { request, read };
  const communityClients = clients.get(client) ?? new Map<string, BotInteractionClient>();
  communityClients.set(communityId, adapter);
  clients.set(client, communityClients);
  return adapter;
}
