/** This is the entire privileged surface. Encryption keys and persistent authentication tokens never cross it. */
export type DesktopCommand =
  | { type: 'badge'; count: number }
  | { type: 'guard'; activeCall: boolean; unsavedDraft: boolean }
  | { type: 'quit-ack'; id: string; activeCall: boolean; unsavedDraft: boolean }
  | { type: 'autostart'; enabled: boolean }
  | { type: 'notify-ring'; id: string; title: string; body: string }
  | { type: 'notify'; id: string; title: string; body: string }
  | { type: 'dismiss-notification'; id: string }
  | { type: 'stop-app-audio'; id: string }
  | { type: 'desktop-theme'; theme: import('../../design-tokens/src/tokens.ts').ThemeName }
  | { type: 'window-menu' }
  | { type: 'check-update' }
  | { type: 'restart-update' };

export type DesktopEvent = { type: 'notification-click' | 'notification-close'; id: string }
  | { type: 'notification-action'; id: string; action: 'answer' | 'decline' }
  | { type: 'app-audio-ended'; id: string }
  | { type: 'update'; phase: string; message?: string }
  | { type: 'prepare-quit'; id: string };

export interface DesktopInfo {
  platform?: string;
  version: string;
  instance: string;
  capabilities: { applicationAudio: boolean; browserAuth: boolean; notifications: boolean; screenCapture: true };
}

export interface AudioSource { id: string; label: string }

export interface DesktopBridge {
  info(): Promise<DesktopInfo>;
  send(command: DesktopCommand): Promise<void>;
  onEvent(listener: (event: DesktopEvent) => void): () => void;
  browserAuth?(request: import('./browser-auth.ts').BrowserAuthRequest): Promise<import('./browser-auth.ts').BrowserAuthResult>;
  requestAppAudio?(): Promise<AudioSource | null>;
  onAppAudio?(id: string, listener: (samples: Float32Array) => void): () => void;
}
