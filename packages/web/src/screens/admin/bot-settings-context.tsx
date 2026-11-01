import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type Dispatch, type ReactNode, type SetStateAction } from 'react';
import { useCore } from '../../core/context.tsx';
import { errorOf } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { useSettingsPendingOperation } from '../../settings/edit-state.tsx';

interface BotPageRegistration {
  communityId: string;
  startOperation(scope: string, input: Omit<BotInteractionOperation, 'scope' | 'requestId' | 'phase' | 'response' | 'error' | 'acked' | 'uploadStatus'>): string | null;
  retryOperation(scope: string): void;
  waitOperation(scope: string, requestId: string): Promise<BotInteractionOperation | null>;
  clearOperation(scope: string): void;
  operation(scope: string): BotInteractionOperation | null;
  draft<T>(key: string, fallback: T): T;
  saveDraft<T>(key: string, value: T): void;
  beginMutation(): () => void;
}

export interface BotInteractionResponse {
  deliveryId: string; requestId: string; communityId: string; operation: number; channelId: string | null;
  status: number; revision: string | null; data: Uint8Array;
}
export interface BotInteractionOperation {
  scope: string; requestId: string; communityId: string; operation: number;
  args: readonly (string | number | Uint8Array | null)[]; revision: string | null; stepUp?: string;
  phase: 'sending' | 'waiting' | 'failed' | 'response'; response: BotInteractionResponse | null; error: string | null; acked: boolean;
  uploadStatus: string | null; metadata?: unknown;
}
interface OperationInput { communityId: string; operation: number; args: readonly (string | number | Uint8Array | null)[]; revision: string | null; stepUp?: string; metadata?: unknown }

const BotPageContext = createContext<BotPageRegistration | null>(null);

/** Keeps Bot drafts and pending operations alive while the active settings page changes. */
export function BotSettingsProvider({ communityId, children }: { communityId: string; children: ReactNode }): React.JSX.Element {
  const client = useCore();
  const access = useSlice(`permissions:${communityId}`);
  const [operations, setOperations] = useState<Record<string, BotInteractionOperation>>({});
  const [pendingMutations, setPendingMutations] = useState(0);
  const [drafts, setDrafts] = useState<Record<string, unknown>>({});
  const previousCommunity = useRef(communityId);
  const liveOperations = useRef(operations);
  liveOperations.current = operations;
  const operationWaiters = useRef(new Map<string, Array<(result: BotInteractionOperation | null) => void>>());
  const authorized = access?.loaded === true && access.manageCommunity === true;
  const pendingOperations = Object.values(operations).some(item => item.phase === 'sending' || item.phase === 'waiting'
    || item.phase === 'response' && !item.acked);
  useSettingsPendingOperation(pendingOperations || pendingMutations > 0);
  const dispatch = useCallback((item: BotInteractionOperation) => {
    setOperations(current => ({ ...current, [item.scope]: { ...item, phase: 'sending', uploadStatus: null, error: null } }));
    void client.call({ m: 'humanInteractionRequest', communityId: item.communityId, channelId: null, requestId: item.requestId,
      operation: item.operation, args: item.args, revision: item.revision, ...(item.stepUp === undefined ? {} : { stepUp: item.stepUp }) })
      .then(value => {
        const result = value as { requestId?: unknown; upload?: unknown };
        if (result.requestId !== item.requestId || result.upload === 'rejected') throw new Error('request rejected');
        setOperations(current => current[item.scope]?.requestId !== item.requestId ? current : ({ ...current,
          [item.scope]: { ...current[item.scope], phase: 'waiting', uploadStatus: String(result.upload), error: null } }));
      }).catch(error => {
        const parsed = errorOf(error);
        const definite = parsed.status === 403 || parsed.code === 'E_FORBIDDEN' || parsed.code === 'E_CORE_STATE';
        setOperations(current => current[item.scope]?.requestId !== item.requestId ? current : ({ ...current,
          [item.scope]: { ...current[item.scope], phase: definite ? 'failed' : 'waiting', uploadStatus: 'unknown', error: 'upload' } }));
      });
  }, [client]);
  const startOperation = useCallback((scope: string, input: OperationInput): string | null => {
    const existing = liveOperations.current[scope];
    if (!authorized || existing?.phase === 'sending' || existing?.phase === 'waiting' || existing?.phase === 'response') return null;
    const requestId = [...crypto.getRandomValues(new Uint8Array(16))].map(byte => byte.toString(16).padStart(2, '0')).join('');
    const operation: BotInteractionOperation = { ...input, scope, requestId, phase: 'sending', response: null, error: null, acked: false, uploadStatus: null };
    liveOperations.current = { ...liveOperations.current, [scope]: operation };
    dispatch(operation);
    return requestId;
  }, [authorized, dispatch]);
  const retryOperation = useCallback((scope: string): void => {
    const item = liveOperations.current[scope];
    if (!authorized || item === undefined || item.phase !== 'waiting' && item.phase !== 'failed' || item.response !== null) return;
    dispatch(item);
  }, [authorized, dispatch]);
  const waitOperation = useCallback((scope: string, requestId: string): Promise<BotInteractionOperation | null> => {
    const item = liveOperations.current[scope];
    if (item === undefined || item.requestId !== requestId) return Promise.resolve(null);
    if (item.phase === 'failed' || item.phase === 'response' && item.acked) return Promise.resolve(item);
    return new Promise(resolve => {
      const key = `${scope}:${requestId}`;
      operationWaiters.current.set(key, [...(operationWaiters.current.get(key) ?? []), resolve]);
    });
  }, []);
  const clearOperation = useCallback((scope: string): void => {
    setOperations(current => { const next = { ...current }; delete next[scope]; return Object.keys(next).every(key => next[key] === current[key]) && Object.keys(next).length === Object.keys(current).length ? current : next; });
  }, []);
  const getOperation = useCallback((scope: string): BotInteractionOperation | null => operations[scope] ?? null, [operations]);
  const getDraft = useCallback(<T,>(key: string, fallback: T): T => (drafts[key] as T | undefined) ?? fallback, [drafts]);
  const saveDraft = useCallback(<T,>(key: string, value: T): void => setDrafts(current => ({ ...current, [key]: value })), []);
  const beginMutation = useCallback(() => {
    let finished = false;
    setPendingMutations(count => count + 1);
    return () => { if (!finished) { finished = true; setPendingMutations(count => Math.max(0, count - 1)); } };
  }, []);

  useEffect(() => {
    if (previousCommunity.current !== communityId) {
      previousCommunity.current = communityId;
      setOperations({}); setDrafts({}); setPendingMutations(0);
    }
  }, [communityId]);

  useEffect(() => {
    for (const item of Object.values(operations)) {
      if (item.phase !== 'failed' && !(item.phase === 'response' && item.acked)) continue;
      const key = `${item.scope}:${item.requestId}`;
      const waiters = operationWaiters.current.get(key);
      if (waiters === undefined) continue;
      operationWaiters.current.delete(key);
      for (const resolve of waiters) resolve(item);
    }
  }, [operations]);

  useEffect(() => () => {
    for (const waiters of operationWaiters.current.values()) for (const resolve of waiters) resolve(null);
    operationWaiters.current.clear();
  }, []);

  useEffect(() => {
    if (!authorized) {
      setOperations(current => Object.keys(current).length === 0 ? current : {});
      setDrafts(current => Object.keys(current).length === 0 ? current : {});
      setPendingMutations(0);
      for (const waiters of operationWaiters.current.values()) for (const resolve of waiters) resolve(null);
      operationWaiters.current.clear();
      return;
    }
    const waiting = Object.values(operations).filter(item => item.phase === 'waiting' || item.phase === 'response' && !item.acked);
    if (waiting.length === 0) return;
    let canceled = false;
    let timer: number | undefined;
    const poll = async (): Promise<void> => {
      const communities = [...new Set(waiting.map(item => item.communityId))];
      const results = await Promise.allSettled(communities.map(async cid => {
        const owned = waiting.filter(item => item.communityId === cid);
        const requestIds = [...new Set(owned.map(item => item.requestId))].slice(0, 100);
        return { cid, requestIds, owned,
        page: await client.call({ m: 'readBotInteractionResponses', communityId: cid,
          operations: [...new Set(owned.map(item => item.operation))], requestIds }) as {
          responses: BotInteractionResponse[]; through: string | null; blocked: boolean;
        } }; }));
      if (canceled) return;
      for (let index = 0; index < results.length; index += 1) {
        const result = results[index];
        if (result.status !== 'fulfilled') {
          const cid = communities[index];
          setOperations(current => {
            const next = { ...current };
            for (const item of waiting) if (item.communityId === cid && next[item.scope]?.requestId === item.requestId && next[item.scope].error !== 'read')
              next[item.scope] = { ...next[item.scope], error: 'read' };
            return Object.keys(next).every(key => next[key] === current[key]) && Object.keys(next).length === Object.keys(current).length ? current : next;
          });
          continue;
        }
        const { cid, page, requestIds, owned } = result.value;
        setOperations(current => {
          const next = { ...current };
          for (const item of owned) if (next[item.scope]?.requestId === item.requestId && next[item.scope].error === 'read')
            next[item.scope] = { ...next[item.scope], error: null };
          return Object.keys(next).every(key => next[key] === current[key]) && Object.keys(next).length === Object.keys(current).length ? current : next;
        });
        const delivered = page.responses.filter(row => requestIds.includes(row.requestId)
          && row.communityId === cid && row.channelId === null
          && owned.some(item => item.requestId === row.requestId && item.operation === row.operation));
        if (delivered.length === 0) continue;
        setOperations(current => {
          const next = { ...current };
          for (const row of delivered) {
            const item = Object.values(current).find(candidate => candidate.requestId === row.requestId
              && candidate.communityId === row.communityId && candidate.operation === row.operation);
            if (item !== undefined && item.response === null) next[item.scope] = { ...item, phase: 'response', response: row, acked: false };
          }
          return Object.keys(next).every(key => next[key] === current[key]) && Object.keys(next).length === Object.keys(current).length ? current : next;
        });
        // This is an opaque, read-bound ACK capability, never a delivery cursor.
        // Persist exact request responses above before consuming selected rows.
        if (page.through === null) continue;
        try {
          await client.call({ m: 'ackBotInteractionResponses', communityId: cid, through: page.through });
          setOperations(current => {
            const next = { ...current };
            for (const row of delivered) {
              const item = Object.values(current).find(candidate => candidate.requestId === row.requestId
                && candidate.communityId === row.communityId && candidate.operation === row.operation);
              if (item !== undefined && !item.acked && item.response?.requestId === row.requestId) next[item.scope] = { ...item, acked: true };
            }
            return Object.keys(next).every(key => next[key] === current[key]) && Object.keys(next).length === Object.keys(current).length ? current : next;
          });
        } catch { /* re-read the same request IDs and retry with a fresh lease */ }
      }
      if (!canceled) timer = window.setTimeout(() => { void poll(); }, 750);
    };
    void poll().catch(() => { if (!canceled) timer = window.setTimeout(() => { void poll(); }, 1500); });
    return () => { canceled = true; if (timer !== undefined) window.clearTimeout(timer); };
  }, [authorized, client, operations]);
  const value = useMemo<BotPageRegistration>(() => ({
    communityId,
    startOperation, retryOperation, waitOperation, clearOperation, operation: getOperation, draft: getDraft, saveDraft,
    beginMutation,
  }), [communityId, startOperation, retryOperation, waitOperation, clearOperation, getOperation, getDraft, saveDraft, beginMutation]);
  return <BotPageContext.Provider value={value}>{children}</BotPageContext.Provider>;
}

export function useBotInteractionOperation(scope: string): Pick<BotPageRegistration, 'startOperation' | 'retryOperation' | 'waitOperation' | 'clearOperation' | 'operation'> | null {
  const context = useContext(BotPageContext);
  return useMemo(() => context === null ? null : ({
    startOperation: (name: string, input: OperationInput) => context.startOperation(name, input),
    retryOperation: (name: string) => context.retryOperation(name),
    waitOperation: (name: string, requestId: string) => context.waitOperation(name, requestId),
    clearOperation: (name: string) => context.clearOperation(name),
    operation: (name: string) => name === scope ? context.operation(name) : null,
  }), [context, scope]);
}

export function useBotMutation(): (() => () => void) | null {
  const context = useContext(BotPageContext);
  return context === null ? null : () => context.beginMutation();
}

export function useBotDraftState<T>(communityId: string, key: string, fallback: T): [T, Dispatch<SetStateAction<T>>] {
  const context = useContext(BotPageContext);
  const storageKey = `${communityId}:${key}`;
  const [value, setValue] = useState<T>(() => context?.draft(storageKey, fallback) ?? fallback);
  const valueRef = useRef(value);
  const saveDraftRef = useRef<(key: string, value: unknown) => void>(() => undefined);
  saveDraftRef.current = context === null ? () => undefined : (key, next) => context.saveDraft(key, next);
  valueRef.current = value;
  const save = useCallback((action: SetStateAction<T>) => {
    const next = typeof action === 'function' ? (action as (previous: T) => T)(valueRef.current) : action;
    valueRef.current = next;
    setValue(next);
    saveDraftRef.current?.(storageKey, next);
  }, [storageKey]);
  return [value, save];
}
