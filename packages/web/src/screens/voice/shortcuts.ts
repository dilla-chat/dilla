/**
 * The mute and deafen shortcuts as the voice tooltips show them (plan 2026-10-15). Control or ⌘ with Shift works on
 * every platform (flow 06, decision 1); the tooltip writes the one this platform's keyboard shows: ⌘⇧M on Apple
 * platforms, Ctrl+Shift+M elsewhere.
 */
export function voiceShortcutText(apple: boolean = isApplePlatform()): { mute: string; deafen: string } {
  return apple ? { mute: '⌘⇧M', deafen: '⌘⇧D' } : { mute: 'Ctrl+Shift+M', deafen: 'Ctrl+Shift+D' };
}

/**
 * The same shortcuts as the prototype's keycaps (the call bar's, the menu's hint): ⌘⇧M on a Mac, Ctrl⇧M on Windows and Linux
 * (Jonas, wave B task 4 fix round 1). `aria` is the same key for `aria-keyshortcuts`.
 */
export function voiceKeycapText(apple: boolean = isApplePlatform()): { mute: string; deafen: string; ariaMute: string; ariaDeafen: string } {
  return apple ? { mute: '⌘⇧M', deafen: '⌘⇧D', ariaMute: 'Meta+Shift+M', ariaDeafen: 'Meta+Shift+D' }
    : { mute: 'Ctrl⇧M', deafen: 'Ctrl⇧D', ariaMute: 'Control+Shift+M', ariaDeafen: 'Control+Shift+D' };
}

interface PlatformSource { userAgentData?: { platform?: string }; platform?: string }

/**
 * Whether this is macOS or iOS: userAgentData's platform where the browser has one, else navigator.platform. An
 * empty UA-CH platform (Chromium's "unknown") is no answer, so navigator.platform decides then.
 */
export function isApplePlatform(nav: PlatformSource | undefined = typeof navigator === 'undefined' ? undefined : navigator): boolean {
  const hinted = nav?.userAgentData?.platform;
  const name = hinted !== undefined && hinted !== '' ? hinted : nav?.platform ?? '';
  return /^(mac|iphone|ipad|ipod)/i.test(name);
}
