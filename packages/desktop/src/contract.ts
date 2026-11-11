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
  | { type: 'stop-screen-capture'; id: string }
  | { type: 'configure-screen-capture'; id: string; options: ScreenCaptureOptions }
  | { type: 'desktop-theme'; theme: import('../../design-tokens/src/tokens.ts').ThemeName }
  | { type: 'window-mode'; mode: import('./window-layout.ts').WindowMode; height: number | null }
  | { type: 'window-menu' }
  | { type: 'window-control'; action: 'minimize' | 'toggle-maximize' | 'close' }
  | { type: 'check-update' }
  | { type: 'restart-update' };

export type DesktopEvent = { type: 'notification-click' | 'notification-close'; id: string }
  | { type: 'notification-action'; id: string; action: 'answer' | 'decline' }
  | { type: 'app-audio-ended'; id: string }
  | { type: 'screen-capture-ended'; id: string }
  | { type: 'update'; phase: string; message?: string }
  | { type: 'prepare-quit'; id: string };

export interface DesktopInfo {
  platform?: string;
  version: string;
  instance: string;
  capabilities: { applicationAudio: boolean; browserAuth: boolean; notifications: boolean; screenCapture: true };
}

export interface ScreenCaptureOptions { width: number; height: number; frameRate: number }
export interface ScreenCaptureSource { id: string; displaySurface: 'monitor' | 'window' }

export interface AudioSource { id: string; label: string }

export interface DesktopBridge {
  /** process.platform of the shell ('darwin', 'win32', 'linux'), set by the preload. Display only, not a trust signal. */
  readonly platform?: string;
  /** The system region (BCP 47, e.g. 'sv-SE') read once by the main process, so dates follow the OS region rather than the UI language. */
  readonly locale?: string;
  info(): Promise<DesktopInfo>;
  send(command: DesktopCommand): Promise<void>;
  onEvent(listener: (event: DesktopEvent) => void): () => void;
  browserAuth?(request: import('./browser-auth.ts').BrowserAuthRequest): Promise<import('./browser-auth.ts').BrowserAuthResult>;
  requestScreenCapture?(options: ScreenCaptureOptions): Promise<ScreenCaptureSource | null>;
  requestAppAudio?(): Promise<AudioSource | null>;
  onAppAudio?(id: string, listener: (samples: Float32Array) => void): () => void;
}
