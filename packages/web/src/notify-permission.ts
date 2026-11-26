// The browser's notification permission, read and asked for from the page. askPermission is the one caller of
// requestPermission(), and only Settings → Notifications calls it, from the click of its button (Q30: Firefox
// refuses a request made without a gesture, and nothing asks on load).

export type PermissionState = NotificationPermission | 'unsupported';

export interface NotificationApi {
  readonly permission: NotificationPermission;
  requestPermission(): Promise<NotificationPermission>;
}

/** globalThis.Notification, or undefined where the browser has none. */
export function notificationApi(): NotificationApi | undefined {
  const n = (globalThis as { Notification?: unknown }).Notification;
  return typeof n === 'function' || (typeof n === 'object' && n !== null) ? n as NotificationApi : undefined;
}

export function readPermission(api: NotificationApi | undefined = notificationApi()): PermissionState {
  return api === undefined ? 'unsupported' : api.permission;
}

/** Asks the browser; a refused or failed request answers the permission as it stands. */
export async function askPermission(api: NotificationApi | undefined = notificationApi()): Promise<PermissionState> {
  if (api === undefined) return 'unsupported';
  try {
    return await api.requestPermission();
  } catch {
    return api.permission;
  }
}
