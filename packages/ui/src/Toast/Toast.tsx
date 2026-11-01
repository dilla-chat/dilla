import { useEffect, useSyncExternalStore } from 'react';
import { createPortal } from 'react-dom';
import { Icon } from '../Icon/Icon.tsx';
import './Toast.css';

/**
 * The prototype's single toast stack, one live region for the page (task 8b fix round 2; shared with the share picker's
 * "You're live" in task 8c): every mounted host registers, the first one renders, and any caller shows a toast through
 * showToast for 2.4 s.
 */
const toasts = {
  current: null as { text: string; n: number } | null,
  hosts: [] as number[],
  version: 0,
  timer: null as ReturnType<typeof setTimeout> | null,
  listeners: new Set<() => void>(),
  emit() { this.version++; for (const l of [...this.listeners]) l(); },
};
let nextHost = 0;
/** A fresh id for a ToastHost (useState(newToastHostId)). */
export function newToastHostId(): number { return ++nextHost; }
export function showToast(text: string): void {
  if (toasts.timer !== null) clearTimeout(toasts.timer);
  toasts.current = { text, n: (toasts.current?.n ?? 0) + 1 };
  toasts.timer = setTimeout(() => { toasts.current = null; toasts.timer = null; toasts.emit(); }, 2400);
  toasts.emit();
}
const subscribeToasts = (l: () => void) => { toasts.listeners.add(l); return () => { toasts.listeners.delete(l); }; };
const toastVersion = () => toasts.version;

/** The shared toast region, rendered by the first mounted host only. */
export function ToastHost({ id }: { id: number }) {
  useSyncExternalStore(subscribeToasts, toastVersion, toastVersion);
  useEffect(() => {
    toasts.hosts.push(id);
    toasts.emit();
    return () => { toasts.hosts = toasts.hosts.filter(h => h !== id); toasts.emit(); };
  }, [id]);
  if (toasts.hosts[0] !== id || typeof document === 'undefined') return null;
  const toast = toasts.current;
  return createPortal(
    <div className="d-np-toasts" role="status" aria-live="polite">
      {toast !== null ? <div key={toast.n} className="d-np-toast"><Icon name="check" size={14} stroke={2.6} />{toast.text}</div> : null}
    </div>, document.body);
}

