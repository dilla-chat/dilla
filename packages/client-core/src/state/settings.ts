// The device-local settings the worker accepts (L-TS-25): three key shapes and the values each admits.
// Anything else is refused, so the settings table never becomes a general store (architect ruling 14).

/** web3.6 E3: the one row the worker itself keeps in the same device-local table: the server a sign-up left at the factor
 *  step is to join, so a reload keeps it. Not a setting: isSettingKey refuses it, and pageSettings leaves it out. */
export const PENDING_JOIN_KEY = 'signup.pendingJoin';
export const PENDING_ROLE_PANEL_ACTION_PREFIX = 'rolePanel.pending.';

/** The settings slice: every row but the worker's own. */
export function pageSettings(rows: Record<string, string>): Record<string, string> {
  if (!(PENDING_JOIN_KEY in rows) && !Object.keys(rows).some(k => k.startsWith(PENDING_ROLE_PANEL_ACTION_PREFIX))) return rows;
  return Object.fromEntries(Object.entries(rows).filter(([k]) => k !== PENDING_JOIN_KEY && !k.startsWith(PENDING_ROLE_PANEL_ACTION_PREFIX)));
}

export const SETTINGS = {
  notifyDefault: 'notify.default',                           // 'dms-mentions' (default) | 'everything' | 'nothing'
  notifyChannel: (hex: string) => `notify.channel.${hex}`,   // 'all' | 'mentions' | 'nothing'
  muteChannel: (hex: string) => `mute.channel.${hex}`,       // '1' when muted; absent otherwise
  // web3.5: per-server defaults. The channel value still wins over the community value.
  notifyCommunity: (hex: string) => `notify.community.${hex}`, // 'all' | 'mentions' | 'nothing'
  muteCommunity: (hex: string) => `mute.community.${hex}`,     // '1' when the whole server is muted
  everyoneCommunity: (hex: string) => `everyone.community.${hex}`, // '1' = ignore @everyone and @here
  density: 'density',
  reduceMotion: 'reduceMotion',
  searchScope: 'searchScope',
  // web-3 (L-TS-54): voice devices and input, per-person volume, and the two sound switches.
  voiceInput: 'voice.input',                                 // a device id the browser gave; absent = system default
  voiceOutput: 'voice.output',
  voiceMode: 'voice.mode',                                   // 'vad' (default) | 'ptt'
  voicePttKey: 'voice.pttKey',                               // a KeyboardEvent.code; absent = no key (decision 2)
  voiceNoise: 'voice.noise',                                 // legacy 'on' (default), 'lightweight' | 'automatic' | 'off'
  voiceVolume: (hex: string) => `voice.volume.${hex}`,       // per user: '0'..'200' in steps of 10; absent = '100'
  voiceStreamVolume: (hex: string) => `voice.streamVolume.${hex}`, // #37 T5: per user, their shared screen's sound: '0'..'200'; absent = '100'
  // Product walk M5: this browser's own mute and deafen, kept across a reload: 'muted' | 'deafened'; absent = neither.
  voiceSelf: 'voice.self',
  // Signal task 10b (README §5.9 Voice): the input and output volume ('0'..'200' %, absent = '100'), automatic sensitivity
  // ('on' default | 'off'), the manual threshold ('-100'..'0' dB, absent = '-50'), push to talk's release delay ('0'..'1000'
  // ms in steps of 20, absent = '200'), echo cancellation and automatic gain ('on' default | 'off').
  voiceInputVolume: 'voice.inputVolume',
  voiceOutputVolume: 'voice.outputVolume',
  voiceSensitivityAuto: 'voice.sensitivityAuto',
  voiceSensitivity: 'voice.sensitivity',
  voicePttRelease: 'voice.pttRelease',
  voiceEcho: 'voice.echo',
  voiceAgc: 'voice.agc',
  soundVoice: 'sound.voice',                                 // join and leave sounds: 'on' (default) | 'off'
  soundRing: 'sound.ring',                                   // the ring: 'on' (default) | 'off'
  // Signal task 10 (README §5.9 Notifications): desktop notifications on this device: 'on' (default) | 'off'.
  notifyDesktop: 'notify.desktop',
  // web3.9 (R36): '1' when a category is collapsed on this device; absent = expanded.
  channelCollapsed: (categoryHex: string) => `channels.collapsed.${categoryHex}`,
} as const;

/** What an absent voice or sound key means (decision 8, round 2 R4: sounds are on by default). */
export const VOICE_DEFAULTS = { mode: 'vad', pttKey: null, noise: 'on', volume: '100', soundVoice: 'on', soundRing: 'on',
  inputVolume: '100', outputVolume: '100', sensitivityAuto: 'on', sensitivity: '-50', pttRelease: '200', echo: 'on', agc: 'on' } as const;

export type NotifyMode = 'all' | 'mentions' | 'nothing';

const HEX32 = /^[0-9a-f]{32}$/;
const NOTIFY_CHANNEL = 'notify.channel.';
const MUTE_CHANNEL = 'mute.channel.';
const DEFAULTS: readonly string[] = ['dms-mentions', 'everything', 'nothing'];
const MODES: readonly string[] = ['all', 'mentions', 'nothing'];
const NOTIFY_COMMUNITY = 'notify.community.';
const MUTE_COMMUNITY = 'mute.community.';
const EVERYONE_COMMUNITY = 'everyone.community.';
const VOICE_VOLUME = 'voice.volume.';
const VOICE_STREAM_VOLUME = 'voice.streamVolume.';
const CHANNEL_COLLAPSED = 'channels.collapsed.';
const DEVICE_ID = /^[A-Za-z0-9+/=_-]{1,128}$/;
const PTT_KEY = /^[A-Za-z][A-Za-z0-9]{0,31}$/;
const VOLUME = /^(0|[1-9]0|1[0-9]0|200)$/;
/** Task 10b: a whole percent 0..200 (the prototype's range has step 1). */
const PERCENT = /^(0|[1-9][0-9]?|1[0-9]{2}|200)$/;
/** Task 10b: a whole dB −100..0. */
const THRESHOLD_DB = /^(0|-[1-9][0-9]?|-100)$/;
/** Task 10b: 0..1000 ms in steps of 20 (the prototype's range). */
const RELEASE_MS = /^(0|[1-9][0-9]{0,2}|1000)$/;
const ON_OFF: readonly string[] = ['on', 'off'];
const FIXED_KEYS: ReadonlySet<string> = new Set([
  SETTINGS.notifyDefault, SETTINGS.density, SETTINGS.reduceMotion, SETTINGS.searchScope,
  SETTINGS.voiceInput, SETTINGS.voiceOutput, SETTINGS.voiceMode, SETTINGS.voicePttKey, SETTINGS.voiceNoise,
  SETTINGS.soundVoice, SETTINGS.soundRing, SETTINGS.voiceSelf, SETTINGS.notifyDesktop,
  SETTINGS.voiceInputVolume, SETTINGS.voiceOutputVolume, SETTINGS.voiceSensitivityAuto, SETTINGS.voiceSensitivity,
  SETTINGS.voicePttRelease, SETTINGS.voiceEcho, SETTINGS.voiceAgc,
]);

export function isSettingKey(key: string): boolean {
  if (FIXED_KEYS.has(key)) return true;
  if (key.startsWith(NOTIFY_CHANNEL)) return HEX32.test(key.slice(NOTIFY_CHANNEL.length));
  if (key.startsWith(MUTE_CHANNEL)) return HEX32.test(key.slice(MUTE_CHANNEL.length));
  if (key.startsWith(NOTIFY_COMMUNITY)) return HEX32.test(key.slice(NOTIFY_COMMUNITY.length));
  if (key.startsWith(MUTE_COMMUNITY)) return HEX32.test(key.slice(MUTE_COMMUNITY.length));
  if (key.startsWith(EVERYONE_COMMUNITY)) return HEX32.test(key.slice(EVERYONE_COMMUNITY.length));
  if (key.startsWith(VOICE_VOLUME)) return HEX32.test(key.slice(VOICE_VOLUME.length));
  if (key.startsWith(VOICE_STREAM_VOLUME)) return HEX32.test(key.slice(VOICE_STREAM_VOLUME.length));
  if (key.startsWith(CHANNEL_COLLAPSED)) return HEX32.test(key.slice(CHANNEL_COLLAPSED.length));
  return false;
}

/** The value set each key shape admits (notify.default: the three; notify.channel.*: the three modes; mute.channel.*: '1'). */
export function isSettingValue(key: string, value: string): boolean {
  if (!isSettingKey(key)) return false;
  if (key === SETTINGS.notifyDefault) return DEFAULTS.includes(value);
  if (key === SETTINGS.density) return value === 'regular' || value === 'compact';
  if (key === SETTINGS.reduceMotion) return value === 'system' || value === 'on' || value === 'off';
  if (key === SETTINGS.searchScope) return value === 'channel' || value === 'community' || value === 'all';
  if (key === SETTINGS.voiceInput || key === SETTINGS.voiceOutput) return DEVICE_ID.test(value);
  if (key === SETTINGS.voiceMode) return value === 'vad' || value === 'ptt';
  if (key === SETTINGS.voiceSelf) return value === 'muted' || value === 'deafened';
  if (key === SETTINGS.voicePttKey) return PTT_KEY.test(value);
  if (key === SETTINGS.voiceNoise) return ['on', 'off', 'automatic', 'lightweight'].includes(value);
  if (key === SETTINGS.soundVoice || key === SETTINGS.soundRing || key === SETTINGS.notifyDesktop) return ON_OFF.includes(value);
  if (key === SETTINGS.voiceSensitivityAuto || key === SETTINGS.voiceEcho || key === SETTINGS.voiceAgc) return ON_OFF.includes(value);
  if (key === SETTINGS.voiceInputVolume || key === SETTINGS.voiceOutputVolume) return PERCENT.test(value);
  if (key === SETTINGS.voiceSensitivity) return THRESHOLD_DB.test(value);
  if (key === SETTINGS.voicePttRelease) return RELEASE_MS.test(value) && Number(value) % 20 === 0;
  if (key.startsWith(VOICE_VOLUME) || key.startsWith(VOICE_STREAM_VOLUME)) return VOLUME.test(value);
  if (key.startsWith(NOTIFY_CHANNEL) || key.startsWith(NOTIFY_COMMUNITY)) return MODES.includes(value);
  return value === '1';
}

/** Channel override first, then the server's value (when `communityHex` is a valid id), then the device default. */
export function effectiveNotifyMode(settings: Record<string, string>, channelHex: string, kind: 0 | 3 | 4, communityHex?: string): NotifyMode {
  const override = settings[SETTINGS.notifyChannel(channelHex)];
  if (override !== undefined && MODES.includes(override)) return override as NotifyMode;
  if (communityHex !== undefined && HEX32.test(communityHex)) {
    const server = settings[SETTINGS.notifyCommunity(communityHex)];
    if (server !== undefined && MODES.includes(server)) return server as NotifyMode;
  }
  const stored = settings[SETTINGS.notifyDefault];
  const fallback = stored !== undefined && DEFAULTS.includes(stored) ? stored : 'dms-mentions';
  if (fallback === 'everything') return 'all';
  if (fallback === 'nothing') return 'nothing';
  return kind === 3 || kind === 4 ? 'all' : 'mentions';
}

/** True when the channel or (given `communityHex`) its server is muted. */
export function isMuted(settings: Record<string, string>, channelHex: string, communityHex?: string): boolean {
  if (settings[SETTINGS.muteChannel(channelHex)] === '1') return true;
  return communityHex !== undefined && HEX32.test(communityHex) && settings[SETTINGS.muteCommunity(communityHex)] === '1';
}

/** True when this server ignores @everyone and @here. */
export function suppressesEveryone(settings: Record<string, string>, communityHex: string): boolean {
  return HEX32.test(communityHex) && settings[SETTINGS.everyoneCommunity(communityHex)] === '1';
}
