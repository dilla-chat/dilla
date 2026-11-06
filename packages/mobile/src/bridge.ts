// window.dillaMobile: the native side of the MobileBridge contract (contract.ts) for the bundled web client.
// Built as a classic script (dist/mobile-bridge.js) that runs before the client's module entry, so the
// client finds the bridge on its first read of globalThis.dillaMobile (packages/web/src/mobile.ts).
import { Capacitor, CapacitorHttp } from '@capacitor/core';
import { App } from '@capacitor/app';
import { Browser } from '@capacitor/browser';
import { Haptics, ImpactStyle } from '@capacitor/haptics';
import { LocalNotifications } from '@capacitor/local-notifications';
import { Preferences } from '@capacitor/preferences';
import type { MobileBridge, MobileCommand, MobileEvent, MobileInfo } from './contract.ts';
import { browserAuth } from './browser-auth.ts';
import { currentInstance } from './instances.ts';
import { nativeNotificationId, notificationPermission } from './native-ids.ts';

const PICKER = '/picker.html';

/** The instance this page runs against. With none chosen yet, the picker takes over and this never resolves,
 *  so the client stops at its first `info()` instead of starting a core worker without an instance. */
const instance: Promise<string> = currentInstance(Preferences).then(
  current => current ?? new Promise<string>(() => { location.replace(PICKER); }),
  () => new Promise<string>(() => { location.replace(PICKER); }),
);

const listeners = new Set<(event: MobileEvent) => void>();
function emit(event: MobileEvent): void {
  for (const listener of [...listeners]) { try { listener(event); } catch { /* one listener cannot stop the others */ } }
}
void App.addListener('pause', () => emit({ type: 'pause' }));
void App.addListener('resume', () => emit({ type: 'resume' }));
void LocalNotifications.addListener('localNotificationActionPerformed', action => {
  const id: unknown = action.notification.extra?.id;
  if (typeof id === 'string') emit({ type: 'notification-click', id });
});

function platform(): MobileInfo['platform'] {
  const name = Capacitor.getPlatform();
  if (name !== 'ios' && name !== 'android') throw new Error('Dilla mobile runs only inside the iOS or Android app');
  return name;
}

const text = (value: unknown, max: number) => typeof value === 'string' && value.length <= max;

async function send(command: MobileCommand): Promise<void> {
  const value = command as Record<string, unknown> | null;
  if (!value || typeof value !== 'object') throw new Error('Invalid mobile command');
  switch (value.type) {
    case 'notify': {
      if (!text(value.id, 256) || !text(value.title, 256) || !text(value.body, 4096)) throw new Error('Invalid mobile command');
      const id = value.id as string;
      await LocalNotifications.schedule({ notifications: [{ id: nativeNotificationId(id), title: value.title as string, body: value.body as string, extra: { id } }] });
      return;
    }
    case 'dismiss-notification': {
      if (!text(value.id, 256)) throw new Error('Invalid mobile command');
      const ids = [nativeNotificationId(value.id as string)];
      await LocalNotifications.cancel({ notifications: ids.map(id => ({ id })) });
      await LocalNotifications.removeDeliveredNotificationsById({ ids });
      return;
    }
    case 'haptic':
      if (value.kind !== 'light' && value.kind !== 'medium') throw new Error('Invalid mobile command');
      await Haptics.impact({ style: value.kind === 'light' ? ImpactStyle.Light : ImpactStyle.Medium });
      return;
    case 'badge':
      // No app-icon badge API among the core Capacitor plugins. The unread badge arrives with push (wave M2).
      if (!Number.isInteger(value.count) || (value.count as number) < 0) throw new Error('Invalid mobile command');
      return;
    case 'switch-instance':
      location.assign(PICKER);
      return;
    default:
      throw new Error('Invalid mobile command');
  }
}

const bridge: MobileBridge = {
  async info() {
    const [current, app] = await Promise.all([instance, App.getInfo()]);
    return { platform: platform(), version: app.version, instance: current };
  },
  send,
  onEvent(listener) {
    listeners.add(listener);
    return () => { listeners.delete(listener); };
  },
  async notificationPermission() { return notificationPermission((await LocalNotifications.checkPermissions()).display); },
  async requestNotificationPermission() { return notificationPermission((await LocalNotifications.requestPermissions()).display); },
  async browserAuth(request) {
    const current = await instance;
    try {
      return await browserAuth(current, request, { open: url => Browser.open({ url }), http: CapacitorHttp });
    } finally {
      // The sign-in page is done (or abandoned); bring the app back in front of it.
      void Browser.close().catch(() => {});
    }
  },
};

Object.defineProperty(window, 'dillaMobile', { value: Object.freeze(bridge), writable: false, configurable: false, enumerable: false });
