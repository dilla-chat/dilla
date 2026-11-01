// Free of imports on purpose: the call bar's picker (entry code) reads the table and the support rules, and the session
// (lazy chunk) reads the same table for the screen track's encoding, so neither pulls the other's dependencies in.
// Signal task 8c (README §5.6, the prototype's share picker): what a screen share captures and how it is encoded.

/** The surface the browser's own picker opens on (`displaySurface`): the whole screen, a window or a browser tab. */
export type ShareSurface = 'monitor' | 'window' | 'browser';
export type ShareResolution = '720p' | '1080p' | '1440p' | 'source';
export type ShareFps = 15 | 30 | 60;
export interface ShareOptions { surface: ShareSurface; resolution: ShareResolution; fps: ShareFps; systemAudio: boolean }

/** The prototype's picker defaults: the screen, 1080p, 30 fps, with sound. */
export const DEFAULT_SHARE_OPTIONS: ShareOptions = { surface: 'monitor', resolution: '1080p', fps: 30, systemAudio: true };

export const SHARE_SURFACES: readonly ShareSurface[] = ['monitor', 'window', 'browser'];
export const SHARE_RESOLUTIONS: readonly ShareResolution[] = ['720p', '1080p', '1440p', 'source'];
export const SHARE_FPS: readonly ShareFps[] = [15, 30, 60];

/** The largest frame each resolution asks for (`max` constraints keep the surface's own aspect ratio); 'source' none. */
const FRAME: Record<Exclude<ShareResolution, 'source'>, { width: number; height: number }> = {
  '720p': { width: 1280, height: 720 },
  '1080p': { width: 1920, height: 1080 },
  '1440p': { width: 2560, height: 1440 },
};

/**
 * The encoder's target bitrate at 30 fps, in kbit/s (the prototype's table): the one source of truth for the screen track's
 * `maxBitrate` and for the picker's upload estimate.
 */
export const SHARE_TARGET_KBPS: Record<ShareResolution, number> = { '720p': 2500, '1080p': 4500, '1440p': 8000, source: 12000 };
/** The factor on the 30 fps target for the other frame rates (the prototype's 0.6 and 1.6). */
export const SHARE_FPS_FACTOR: Record<ShareFps, number> = { 15: 0.6, 30: 1, 60: 1.6 };
/** Above this, the estimate warns that the link may drop frames (the prototype's 9000 kbit/s). */
export const SHARE_HEAVY_KBPS = 9000;

/**
 * The screen track's `maxBitrate` in bit/s: the table's target for the resolution and frame rate, never above the instance's
 * `livekit.max_share_bitrate_kbps` (DEV-26: the server cannot enforce it, so the client applies it). `capBps` null: no cap known.
 */
export function shareBitrateBps(o: Pick<ShareOptions, 'resolution' | 'fps'>, capBps: number | null): number {
  const target = Math.round(SHARE_TARGET_KBPS[o.resolution] * SHARE_FPS_FACTOR[o.fps] * 1000);
  return capBps === null ? target : Math.min(target, capBps);
}

/** The picker's estimate: the encoding's own bitrate as "≈ 4.5 Mb/s up", and whether it is above the heavy line. */
export function shareEstimate(o: Pick<ShareOptions, 'resolution' | 'fps'>, capBps: number | null): { mbps: string; heavy: boolean } {
  const kbps = shareBitrateBps(o, capBps) / 1000;
  return { mbps: (kbps / 1000).toFixed(1), heavy: kbps > SHARE_HEAVY_KBPS };
}

/** The screen track's encoding (LiveKit's `screenShareEncoding`, the sender's top layer): the bitrate above and the frame rate. */
export function shareEncoding(o: Pick<ShareOptions, 'resolution' | 'fps'>, capBps: number | null): { maxBitrate: number; maxFramerate: number } {
  return { maxBitrate: shareBitrateBps(o, capBps), maxFramerate: o.fps };
}

/**
 * The video constraints of `getDisplayMedia` (and of `applyConstraints` on a running share, without `displaySurface`):
 * - `displaySurface`: which pane the browser's picker opens on (Chromium); the person still picks there;
 * - `width`/`height` `max` for 720p, 1080p and 1440p (the browser scales down, keeping the aspect ratio); none for 'source';
 * - `frameRate` `ideal` and `max` the chosen rate.
 */
export function shareVideoConstraints(o: ShareOptions, withSurface = true): MediaTrackConstraints & { displaySurface?: ShareSurface } {
  const frame = o.resolution === 'source' ? {} : { width: { max: FRAME[o.resolution].width }, height: { max: FRAME[o.resolution].height } };
  return { ...(withSurface ? { displaySurface: o.surface } : {}), ...frame, frameRate: { ideal: o.fps, max: o.fps } };
}

/** What the picker may offer on this browser: a control is drawn only where it changes what is captured. */
export interface ShareSupport {
  /** The source row (and its preview): Chromium honours `displaySurface` and can capture a tab; Firefox and Safari ignore it. */
  surface: boolean;
  /** Per source: whether the browser can capture its sound, so the system-audio toggle does something. */
  audio: Record<ShareSurface, boolean>;
  /** Chromium on Linux (and elsewhere without system sound): only a tab's sound; the audio row says so for a screen or a window. */
  tabOnly: boolean;
}

/**
 * The capture support of a browser (MDN browser-compat-data: `getDisplayMedia` `displaySurface` and `audio_capture_support`;
 * https://developer.chrome.com/docs/web-platform/screen-sharing-controls), from the same reading as `shareAudioAdvice`:
 * - Chromium (Chrome, Edge) honours `displaySurface` and captures tabs; it gives a tab's sound everywhere, a screen's and a
 *   window's system sound on Windows, ChromeOS and macOS, and only a tab's on Linux;
 * - Firefox and Safari ignore `displaySurface`, capture no tab and give no sound at all (Mozilla bug 1541425).
 */
export function shareSupport(env: { ua: string; uaPlatform?: string; brands?: string[] }): ShareSupport {
  const { ua } = env;
  const chromium = (env.brands?.some(b => /chrom|edge/i.test(b)) ?? false) || /Chrome\/|Edg\//.test(ua);
  if (!chromium) return { surface: false, audio: { monitor: false, window: false, browser: false }, tabOnly: false };
  const platform = env.uaPlatform !== undefined && env.uaPlatform !== '' ? env.uaPlatform : ua;
  const system = /Windows|CrOS|Chrom(?:e|ium) ?OS|Mac/i.test(platform);
  return { surface: true, audio: { monitor: system, window: system, browser: true }, tabOnly: !system };
}

/** The options a browser can act on: an unsupported source falls back to the screen, sound to off where none can come. */
export function supportedOptions(o: ShareOptions, s: ShareSupport): ShareOptions {
  const surface = s.surface ? o.surface : 'monitor';
  return { ...o, surface, systemAudio: o.systemAudio && (s.surface ? s.audio[surface] : false) };
}
