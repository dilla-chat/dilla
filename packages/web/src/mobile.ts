import type { MobileBridge } from '../../mobile/src/contract.ts';
import type { NotificationCtor, NotificationHandle } from './notify.ts';

export function mobileBridge(): MobileBridge | undefined {
  return (globalThis as unknown as { dillaMobile?: MobileBridge }).dillaMobile;
}

const permissions = new WeakMap<MobileBridge, { state: NotificationPermission }>();

/** The bridge's last known notification permission. Read once per bridge (startup), refreshed on `resume` and after a request. */
function tracked(bridge: MobileBridge): { state: NotificationPermission } {
  let entry = permissions.get(bridge);
  if (entry) return entry;
  const fresh = { state: 'default' as NotificationPermission };
  entry = fresh;
  permissions.set(bridge, fresh);
  const refresh = () => { void bridge.notificationPermission().then(s => { fresh.state = s; }).catch(() => {}); };
  refresh();
  bridge.onEvent(event => { if (event.type === 'resume') refresh(); });
  return fresh;
}

/** Starts tracking the permission at startup, so the first notice already sees the OS state. */
export function watchMobilePermission(bridge: MobileBridge): void { tracked(bridge); }

export function readMobilePermission(bridge: MobileBridge): NotificationPermission { return tracked(bridge).state; }

/** Triggers the native prompt; a failed request answers the permission as it stands. */
export async function askMobilePermission(bridge: MobileBridge): Promise<NotificationPermission> {
  const entry = tracked(bridge);
  try { entry.state = await bridge.requestNotificationPermission(); } catch { /* keep the known state */ }
  return entry.state;
}

const notifications = new WeakMap<MobileBridge, Map<string, { retire(): void }>>();

/** Foreground notifications through the native shell. The permission is the bridge's last known state.
 *  No ring or answer actions here: CallKit arrives in M3. */
export function mobileNotificationCtor(bridge: MobileBridge): NotificationCtor {
  let registry = notifications.get(bridge);
  if (!registry) { registry = new Map(); notifications.set(bridge, registry); }
  const active = registry;
  const known = tracked(bridge);
  return class implements NotificationHandle {
    static get permission(): NotificationPermission { return known.state; }
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
      if (known.state !== 'granted') return;
      void bridge.send({ type: 'notify', id: this.id, title: title.slice(0, 256), body: (options.body ?? '').slice(0, 4096) }).catch(() => {});
    }
    retire(): void { this.unsubscribe(); if (active.get(this.id) === this) active.delete(this.id); }
    close(): void {
      if (active.get(this.id) !== this) return;
      this.retire(); void bridge.send({ type: 'dismiss-notification', id: this.id }).catch(() => {});
    }
  };
}
