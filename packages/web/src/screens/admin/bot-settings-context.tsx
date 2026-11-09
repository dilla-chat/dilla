import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type Dispatch, type ReactNode, type SetStateAction } from 'react';
import { BotOperationRefusedError, createBotOperations } from '@dilla/client-core';
import { useCore } from '../../core/context.tsx';
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
  // Bot v2 Task 22: an operation is a sealed command (an app operation, 100 + its v1 operation): the Bot's answer comes back
  // to this send, so there is nothing to poll or acknowledge. No answer in time leaves it waiting, with Retry, which resends
  // the same request id: the Bot answers it again from its cache, never running it twice.
  const dispatch = useCallback((item: BotInteractionOperation) => {
    setOperations(current => ({ ...current, [item.scope]: { ...item, phase: 'sending', uploadStatus: null, error: null } }));
    void createBotOperations(client, item.communityId).request({ operation: item.operation, args: item.args, revision: item.revision,
      requestId: item.requestId }).then(result => {
      const response: BotInteractionResponse = { deliveryId: item.requestId, requestId: item.requestId, communityId: item.communityId,
        operation: item.operation, channelId: null, status: result.status, revision: result.revision, data: result.data };
      setOperations(current => current[item.scope]?.requestId !== item.requestId ? current : ({ ...current,
        [item.scope]: { ...current[item.scope], phase: 'response', uploadStatus: 'accepted', response, acked: true, error: null } }));
    }).catch((error: unknown) => {
      const definite = error instanceof BotOperationRefusedError;
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
    if (authorized) return;
    setOperations(current => Object.keys(current).length === 0 ? current : {});
    setDrafts(current => Object.keys(current).length === 0 ? current : {});
    setPendingMutations(0);
    for (const waiters of operationWaiters.current.values()) for (const resolve of waiters) resolve(null);
    operationWaiters.current.clear();
  }, [authorized]);
  const value = useMemo<BotPageRegistration>(() => ({
    communityId,
    startOperation, retryOperation, waitOperation, clearOperation, operation: getOperation, draft: getDraft, saveDraft,
    beginMutation,
  }), [communityId, startOperation, retryOperation, waitOperation, clearOperation, getOperation, getDraft, saveDraft, beginMutation]);
  return <BotPageContext.Provider value={value}>{children}</BotPageContext.Provider>;
}

/** The shared operations of the Bot settings pages, which are sealed commands (Task 22). `v1` (automatic moderation, which
 *  Task 23 deletes) gets null: that page keeps its own v1 interaction requests. */
export function useBotInteractionOperation(scope: string, transport: 'sealed' | 'v1' = 'sealed'): Pick<BotPageRegistration, 'startOperation' | 'retryOperation' | 'waitOperation' | 'clearOperation' | 'operation'> | null {
  const context = useContext(BotPageContext);
  return useMemo(() => context === null || transport === 'v1' ? null : ({
    startOperation: (name: string, input: OperationInput) => context.startOperation(name, input),
    retryOperation: (name: string) => context.retryOperation(name),
    waitOperation: (name: string, requestId: string) => context.waitOperation(name, requestId),
    clearOperation: (name: string) => context.clearOperation(name),
    operation: (name: string) => name === scope ? context.operation(name) : null,
  }), [context, scope, transport]);
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
