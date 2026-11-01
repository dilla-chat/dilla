import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore, type ReactNode } from 'react';
import { Button, ConfirmDialog, SettingsFooter } from '@dilla/ui';
import { requestNavigation, setNavigationGuard } from '../router.ts';
import { settingsEditing as copy } from '../strings/settingsEditing.ts';

export interface SettingsEditState {
  dirty: boolean;
  busy: boolean;
  canSave: boolean;
  save(): Promise<boolean>;
  reset(): void;
  /**
   * A section that saves as you type (Profile) sends its last change before the workspace lets it go: the guard awaits this,
   * leaves at once when it resolves true, and otherwise shows the unsaved-changes prompt.
   */
  flush?(): Promise<boolean>;
}

export interface SettingsEditFlags {
  dirty: boolean;
  busy: boolean;
  canSave: boolean;
}

export interface SettingsWorkspace {
  /** Primitive snapshot only; callbacks and full draft state stay in refs so inline state objects cannot rerender-loop. */
  edit: SettingsEditFlags | null;
  footer: ReactNode;
  confirmation: ReactNode;
  requestNavigation: typeof requestNavigation;
  clear(): void;
}

interface Registration {
  attach(owner: symbol, state: SettingsEditState): void;
  publish(owner: symbol, flags: SettingsEditFlags): void;
  detach(owner: symbol): void;
}

const WorkspaceContext = createContext<SettingsWorkspace | null>(null);
const EditRegistrationContext = createContext<Registration | null>(null);
const pendingOperations = new Map<symbol, boolean>();
const pendingListeners = new Set<() => void>();
const pendingSnapshot = (): number => [...pendingOperations.values()].filter(Boolean).length;
function subscribePending(listener: () => void): () => void {
  pendingListeners.add(listener);
  return () => { pendingListeners.delete(listener); };
}
function setPendingOperation(owner: symbol, pending: boolean): void {
  if (pendingOperations.get(owner) === pending) return;
  pendingOperations.set(owner, pending);
  for (const listener of pendingListeners) listener();
}
function clearPendingOperation(owner: symbol): void {
  if (!pendingOperations.delete(owner)) return;
  for (const listener of pendingListeners) listener();
}
const sameFlags = (a: SettingsEditFlags | null, b: SettingsEditFlags): boolean =>
  a !== null && a.dirty === b.dirty && a.busy === b.busy && a.canSave === b.canSave;

/** Owns the shared footer, unsaved-change prompt and browser departure guard for one settings workspace. */
export function SettingsEditProvider({ children }: { children: ReactNode }) {
  const pendingCount = useSyncExternalStore(subscribePending, pendingSnapshot, () => 0);
  const pendingCountRef = useRef(pendingCount);
  pendingCountRef.current = pendingCount;
  const [edit, setEdit] = useState<SettingsEditFlags | null>(null);
  const editRef = useRef<SettingsEditState | null>(null);
  const ownerRef = useRef<symbol | null>(null);
  const clearedOwnerRef = useRef<symbol | null>(null);
  const [prompt, setPrompt] = useState(false);
  const promptResolvers = useRef<Array<(allowed: boolean) => void>>([]);
  const [saveError, setSaveError] = useState(false);

  const registration = useMemo<Registration>(() => ({
    attach(owner, state) {
      ownerRef.current = owner;
      if (clearedOwnerRef.current === owner) {
        if (state.dirty || state.busy) { editRef.current = null; return; }
        clearedOwnerRef.current = null;
      }
      editRef.current = state;
    },
    publish(owner, flags) {
      if (ownerRef.current !== owner || editRef.current === null || clearedOwnerRef.current === owner) return;
      setEdit(current => sameFlags(current, flags) ? current : flags);
    },
    detach(owner) {
      if (ownerRef.current !== owner) return;
      ownerRef.current = null;
      editRef.current = null;
      clearedOwnerRef.current = null;
      setEdit(null);
      setSaveError(false);
    },
  }), []);

  const settlePrompt = useCallback((allowed: boolean) => {
    setPrompt(false);
    const pending = promptResolvers.current.splice(0);
    if (allowed) {
      editRef.current?.reset();
      setSaveError(false);
    }
    for (const resolve of pending) resolve(allowed);
  }, []);

  const clear = useCallback(() => {
    const owner = ownerRef.current;
    editRef.current?.reset();
    if (owner !== null) clearedOwnerRef.current = owner;
    editRef.current = null;
    setEdit(null);
    setSaveError(false);
    settlePrompt(false);
  }, [settlePrompt]);

  useEffect(() => {
    const guard = async (): Promise<boolean> => {
      const before = editRef.current;
      if (before?.flush !== undefined && !before.dirty && pendingCountRef.current === 0) {
        let landed = false;
        try { landed = await before.flush(); } catch { landed = false; }
        if (landed) return true;
        setPrompt(true);
        return new Promise(resolve => promptResolvers.current.push(resolve));
      }
      const state = editRef.current;
      if ((!state || (!state.dirty && !state.busy)) && pendingCountRef.current === 0) return true;
      setPrompt(true);
      return new Promise(resolve => promptResolvers.current.push(resolve));
    };
    const dispose = setNavigationGuard(guard);
    const resolvers = promptResolvers.current;
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      const state = editRef.current;
      if ((!state || (!state.dirty && !state.busy)) && pendingCountRef.current === 0) return;
      event.preventDefault();
      event.returnValue = '';
    };
    window.addEventListener('beforeunload', onBeforeUnload);
    return () => {
      dispose();
      window.removeEventListener('beforeunload', onBeforeUnload);
      const pending = resolvers.splice(0);
      for (const resolve of pending) resolve(false);
    };
  }, []);

  const save = useCallback(async () => {
    const state = editRef.current;
    if (!state || state.busy || !state.canSave) return;
    setSaveError(false);
    let saved = false;
    try { saved = await state.save(); } catch { saved = false; }
    setSaveError(!saved);
  }, []);
  const reset = useCallback(() => {
    const state = editRef.current;
    if (state?.busy) return;
    state?.reset();
    setSaveError(false);
  }, []);

  const workspace = useMemo<SettingsWorkspace>(() => {
    const dirtyOrBusy = Boolean(edit && (edit.dirty || edit.busy));
    const footer = dirtyOrBusy ? (
      <SettingsFooter status={saveError ? copy.saveFailed : copy.unsaved}>
        <Button type="button" disabled={edit?.busy} onClick={reset}>{copy.reset}</Button>
        <Button type="button" variant="accent" disabled={edit?.busy || !edit?.canSave} onClick={() => void save()}>{copy.save}</Button>
      </SettingsFooter>
    ) : null;
    const confirmation = (
      <ConfirmDialog open={prompt} tone="accent" title={copy.leaveTitle}
        body={edit?.busy || pendingCount > 0 ? copy.leaveBusyBody : copy.leaveBody} confirmLabel={copy.discard}
        busyLabel={copy.discard} cancelLabel={copy.keepEditing} busy={false} error={null}
        onConfirm={() => settlePrompt(true)} onClose={() => settlePrompt(false)} />
    );
    return { edit, footer, confirmation, requestNavigation, clear };
  }, [clear, edit, pendingCount, prompt, reset, save, saveError, settlePrompt]);
  return (
    <WorkspaceContext.Provider value={workspace}>
      <EditRegistrationContext.Provider value={registration}>{children}</EditRegistrationContext.Provider>
    </WorkspaceContext.Provider>
  );
}

/** Register this section's state with the nearest workspace; returns false for standalone settings forms. */
export function useSettingsEdit(state: SettingsEditState): boolean {
  const registration = useContext(EditRegistrationContext);
  const managed = useContext(WorkspaceContext) !== null;
  const owner = useRef(Symbol('settings-edit-owner')).current;
  const dirty = state.dirty;
  const busy = state.busy;
  const canSave = state.canSave;
  // Update operation closures during render without putting a fresh object into context state.
  registration?.attach(owner, state);
  useLayoutEffect(() => {
    registration?.publish(owner, { dirty, busy, canSave });
  }, [registration, owner, dirty, busy, canSave]);
  useLayoutEffect(() => () => registration?.detach(owner), [registration, owner]);
  return managed;
}

/** Shared workspace surfaces and helpers. Returns null outside a SettingsEditProvider. */
export function useSettingsWorkspace(): SettingsWorkspace | null {
  return useContext(WorkspaceContext);
}

/** Register an accepted operation that continues independently of the currently mounted settings section. */
export function useSettingsPendingOperation(pending: boolean): void {
  const owner = useRef(Symbol('settings-pending-operation')).current;
  useLayoutEffect(() => {
    setPendingOperation(owner, pending);
    return () => clearPendingOperation(owner);
  }, [owner, pending]);
}
