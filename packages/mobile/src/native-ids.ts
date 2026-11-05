/** Capacitor's display permission onto the web's NotificationPermission: the OS will still ask, so 'default'. */
export function notificationPermission(state: 'granted' | 'denied' | 'prompt' | 'prompt-with-rationale'): NotificationPermission {
  return state === 'granted' ? 'granted' : state === 'denied' ? 'denied' : 'default';
}

/** Local notification ids are 32-bit integers; the client's are string tags. FNV-1a, folded to 1..2^31-1,
 *  so `dismiss-notification` finds the notification `notify` posted under the same tag. */
export function nativeNotificationId(tag: string): number {
  let hash = 0x811c9dc5;
  for (const unit of new TextEncoder().encode(tag)) { hash ^= unit; hash = Math.imul(hash, 0x01000193) >>> 0; }
  return (hash % 0x7fffffff) + 1;
}
