import type { MobileBridge } from '../../mobile/src/contract.ts';
import type { NotificationCtor, NotificationHandle } from './notify.ts';

export function mobileBridge(): MobileBridge | undefined {
  return (globalThis as unknown as { dillaMobile?: MobileBridge }).dillaMobile;
}

const notifications = new WeakMap<MobileBridge, Map<string, { retire(): void }>>();

/** Foreground notifications through the native shell. The OS owns the permission prompt, so the page sees it as granted.
 *  No ring or answer actions here: CallKit arrives in M3. */
export function mobileNotificationCtor(bridge: MobileBridge): NotificationCtor {
  let registry = notifications.get(bridge);
  if (!registry) { registry = new Map(); notifications.set(bridge, registry); }
  const active = registry;
  return class implements NotificationHandle {
    static get permission(): NotificationPermission { return 'granted'; }
    onaction: ((action: 'answer' | 'decline') => void) | null = null;
    onclick: ((event: Event) => unknown) | null = null;
    private readonly id: string;
    private readonly unsubscribe: () => void;
    constructor(title: string, options: NotificationOptions = {}) {
      this.id = options.tag ?? `dilla:${crypto.randomUUID()}`;
      active.get(this.id)?.retire();
      this.unsubscribe = bridge.onEvent(event => {
        if (event.type === 'notification-click' && event.id === this.id) this.onclick?.(new Event('click'));
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
