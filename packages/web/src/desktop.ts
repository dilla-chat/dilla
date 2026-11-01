import type { DesktopBridge } from '../../desktop/src/contract.ts';
import type { NotificationCtor, NotificationHandle } from './notify.ts';

export function desktopBridge(): DesktopBridge | undefined {
  return (globalThis as unknown as { dillaDesktop?: DesktopBridge }).dillaDesktop;
}

const notifications = new WeakMap<DesktopBridge, Map<string, { retire(): void }>>();

/** Uses the same permission decision and notification policy as the browser notifier. */
export function desktopNotificationCtor(bridge: DesktopBridge, permission: () => NotificationPermission): NotificationCtor {
  let registry = notifications.get(bridge);
  if (!registry) { registry = new Map(); notifications.set(bridge, registry); }
  const active = registry;
  return class implements NotificationHandle {
    static get permission(): NotificationPermission { return permission(); }
    onclick: ((event: Event) => unknown) | null = null;
    private readonly id: string;
    private readonly unsubscribe: () => void;
    constructor(title: string, options: NotificationOptions = {}) {
      this.id = options.tag ?? `dilla:${crypto.randomUUID()}`;
      active.get(this.id)?.retire();
      this.unsubscribe = bridge.onEvent(event => {
        if (event.type === 'notification-click' && event.id === this.id) this.onclick?.(new Event('click'));
        if (event.type === 'notification-close' && event.id === this.id) this.retire();
      });
      active.set(this.id, this);
      void bridge.send({ type: 'notify', id: this.id, title: title.slice(0, 256), body: (options.body ?? '').slice(0, 4096) }).catch(() => {});
    }
    retire(): void { this.unsubscribe(); if (active.get(this.id) === this) active.delete(this.id); }
    close(): void {
      if (active.get(this.id) !== this) return;
      this.retire(); void bridge.send({ type: 'dismiss-notification', id: this.id }).catch(() => {});
    }
  };
}

let guard = { activeCall: false, unsavedDraft: false };
export function setDesktopGuard(next: typeof guard, bridge = desktopBridge()): void {
  guard = next;
  void bridge?.send({ type: 'guard', ...next }).catch(() => {});
}

function pendingInput(): boolean {
  return [...document.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>('textarea, input:not([type=checkbox]):not([type=radio]):not([type=hidden]):not([type=range])')]
    .some(input => !input.disabled && input.value.length > 0);
}

export function startDesktopLifecycle(bridge = desktopBridge()): () => void {
  if (!bridge) return () => {};
  setDesktopGuard({ activeCall: false, unsavedDraft: false }, bridge);
  const beforeUnload = (event: BeforeUnloadEvent): void => {
    if (guard.activeCall || guard.unsavedDraft || pendingInput()) { event.preventDefault(); event.returnValue = ''; }
  };
  window.addEventListener('beforeunload', beforeUnload);
  const unsubscribe = bridge.onEvent(event => {
    if (event.type === 'prepare-quit') {
      void bridge.send({ type: 'quit-ack', id: event.id, activeCall: guard.activeCall, unsavedDraft: guard.unsavedDraft || pendingInput() }).catch(() => {});
    }
  });
  return () => { unsubscribe(); window.removeEventListener('beforeunload', beforeUnload); };
}
