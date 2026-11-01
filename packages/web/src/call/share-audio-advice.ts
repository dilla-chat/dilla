// Free of imports on purpose: the call screen's entry code reads it, and it must not pull in the media stack (livekit).
/** What the notice for a share without sound can truthfully advise on this browser (#37). */
export type ShareAudioAdvice = 'unsupported' | 'tab-only' | 'tab-or-system' | 'app';

/**
 * Firefox-family browsers and Safari capture no sound from getDisplayMedia; Chromium on Windows, ChromeOS and macOS can
 * share a screen's system sound; Chromium on Linux (and anywhere else) only a tab's. See `acquireScreen`.
 */
export function shareAudioAdvice(env: { ua: string; uaPlatform?: string; brands?: string[]; desktop?: boolean }): ShareAudioAdvice {
  if (env.desktop) return 'app';
  const { ua } = env;
  const chromium = (env.brands?.some(b => /chrom|edge/i.test(b)) ?? false) || /Chrome\/|Edg\//.test(ua);
  if (!chromium) {
    if (/Firefox\/|Gecko\//.test(ua) || /Safari\//.test(ua)) return 'unsupported';
    return 'tab-only';
  }
  const platform = env.uaPlatform !== undefined && env.uaPlatform !== '' ? env.uaPlatform : ua;
  if (/Windows|CrOS|Chrom(?:e|ium) ?OS|Mac/i.test(platform)) return 'tab-or-system';
  return 'tab-only';
}
