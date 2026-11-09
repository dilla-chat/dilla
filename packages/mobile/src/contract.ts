export interface MobileInfo { platform: 'ios' | 'android'; version: string; instance: string }
export type MobileCommand =
  | { type: 'badge'; count: number }
  | { type: 'notify'; id: string; title: string; body: string }
  | { type: 'dismiss-notification'; id: string }
  | { type: 'haptic'; kind: 'light' | 'medium' }
  | { type: 'switch-instance' };
export type MobileEvent = { type: 'notification-click'; id: string } | { type: 'resume' } | { type: 'pause' };
export interface MobileBridge {
  info(): Promise<MobileInfo>;
  send(command: MobileCommand): Promise<void>;
  onEvent(listener: (event: MobileEvent) => void): () => void;
  notificationPermission(): Promise<NotificationPermission>;
  requestNotificationPermission(): Promise<NotificationPermission>;
  browserAuth(request: import('../../desktop/src/browser-auth.ts').BrowserAuthRequest): Promise<import('../../desktop/src/browser-auth.ts').BrowserAuthResult>;
}
