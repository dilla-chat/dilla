/** The browser sign-in ceremony types, as in the former Electron shell (tag desktop-v0.1.0-beta.5, packages/desktop/src/browser-auth.ts). */
export type BrowserAuthRequest = { kind: 'get' | 'create' | 'oidc'; options: string };
export type BrowserAuthResult = { response?: string; assertion?: string; needs: number; error?: string };
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
  browserAuth(request: BrowserAuthRequest): Promise<BrowserAuthResult>;
}
