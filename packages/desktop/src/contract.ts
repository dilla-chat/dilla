/** This is the entire privileged surface. Encryption keys and authentication tokens never cross it. */
export type DesktopCommand =
  | { type: 'badge'; count: number }
  | { type: 'guard'; activeCall: boolean; unsavedDraft: boolean }
  | { type: 'quit-ack'; id: string; activeCall: boolean; unsavedDraft: boolean }
  | { type: 'autostart'; enabled: boolean }
  | { type: 'notify'; id: string; title: string; body: string }
  | { type: 'dismiss-notification'; id: string }
  | { type: 'stop-app-audio'; id: string }
  | { type: 'check-update' }
  | { type: 'restart-update' };

export type DesktopEvent = { type: 'notification-click' | 'notification-close'; id: string }
  | { type: 'app-audio-ended'; id: string }
  | { type: 'update'; phase: string; message?: string }
  | { type: 'prepare-quit'; id: string };

export interface DesktopInfo {
  version: string;
  instance: string;
  capabilities: { applicationAudio: boolean; browserAuth: boolean; notifications: boolean; screenCapture: true };
}

export interface AudioSource { id: string; label: string }

export interface DesktopBridge {
  info(): Promise<DesktopInfo>;
  send(command: DesktopCommand): Promise<void>;
  onEvent(listener: (event: DesktopEvent) => void): () => void;
  requestAppAudio?(): Promise<AudioSource | null>;
  onAppAudio?(id: string, listener: (samples: Float32Array) => void): () => void;
}
