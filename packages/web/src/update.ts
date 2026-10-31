export interface UpdateWatcher {
  realtime?: { state(): { ready: boolean; entry: string | null }; subscribe(listener: () => void): () => void };
  currentEntry: string;
  fetch: (input: string, init: RequestInit) => Promise<Response>;
  changed(entry: string): void;
  /** A live call caps unresolved-hint retry backoff at thirty seconds. */
  callActive?: () => boolean;
}

export function watchUpdates(options: UpdateWatcher): () => void {
  let active = true;
  let checking = false;
  let announced = false;
  let queued = false;
  const validEntry = (entry: unknown): entry is string => typeof entry === 'string' && entry.length <= 512
    && /^\/assets\/[A-Za-z0-9_./-]+\.js$/.test(entry) && !entry.includes('..');
  const initial = options.realtime?.state();
  let ready = initial?.ready ?? false;
  let pendingHint = ready && validEntry(initial?.entry) && initial.entry !== options.currentEntry ? initial.entry : null;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;
  let retryDelay = 1000;
  const retryHint = () => {
    if (!active || announced || !ready || pendingHint === null || retryTimer !== null) return;
    const ceiling = options.callActive?.() ? 30_000 : 300_000;
    const delay = Math.max(1000, Math.min(retryDelay, ceiling));
    retryDelay = Math.min(retryDelay * 2, Math.max(1000, ceiling));
    retryTimer = setTimeout(() => { retryTimer = null; void check(); }, delay);
  };
  const check = async () => {
    if (!active || checking || announced) return;
    checking = true;
    try {
      const response = await options.fetch('/app-version.json', { cache: 'no-store', credentials: 'same-origin' });
      if (!response.ok) return;
      const value: unknown = await response.json();
      const entry = typeof value === 'object' && value !== null && 'entry' in value ? value.entry : null;
      if (!validEntry(entry)) return;
      if (entry === pendingHint) pendingHint = null;
      if (active && entry !== options.currentEntry) {
        announced = true;
        pendingHint = null;
        if (retryTimer !== null) { clearTimeout(retryTimer); retryTimer = null; }
        options.changed(entry);
      }
    } catch { /* Unresolved hints retry; otherwise explicit lifecycle events reconcile. */ }
    finally { checking = false; if (queued) { queued = false; void check(); } else retryHint(); }
  };
  let lastHint = pendingHint ?? options.currentEntry;
  const reconcile = () => { if (checking) queued = true; else void check(); };
  const unsubscribe = options.realtime?.subscribe(() => {
    const state = options.realtime!.state();
    const reconnected = state.ready && !ready;
    ready = state.ready;
    const hint = state.ready && validEntry(state.entry) && state.entry !== lastHint;
    if (hint) {
      lastHint = state.entry!;
      pendingHint = state.entry === options.currentEntry ? null : state.entry;
      retryDelay = 1000;
    }
    if (reconnected || hint) reconcile();
  });
  const onVisible = () => { if (document.visibilityState === 'visible') void check(); };
  const onOnline = () => { void check(); };
  document.addEventListener('visibilitychange', onVisible);
  window.addEventListener('online', onOnline);
  void check();
  return () => {
    active = false;
    unsubscribe?.();
    if (retryTimer !== null) clearTimeout(retryTimer);
    document.removeEventListener('visibilitychange', onVisible);
    window.removeEventListener('online', onOnline);
  };
}
