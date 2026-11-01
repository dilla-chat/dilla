// Per-person volume (L-TS-54 `voice.volume.<user>`, flow 06 § Tiles): 0..200 % in steps of 10, 100 by default. The
// remote audio plays through livekit’s webAudioMix, where each remote track has its own GainNode; the session sets it
// through RemoteParticipant.setVolume. Deafened or paused (RV-3), every gain is 0.

const VOLUME_KEY = /^voice\.volume\.([0-9a-f]{32})$/;
const STREAM_KEY = /^voice\.streamVolume\.([0-9a-f]{32})$/;
const VOLUME_VALUE = /^(0|[1-9]0|1[0-9]0|200)$/;

export type Volumes = Record<string, number>;

export function isVolume(v: number): boolean {
  return Number.isInteger(v) && v >= 0 && v <= 200 && v % 10 === 0;
}

/** `volumes` is the map of the source: the voice volumes for a microphone, the stream volumes for a screen's sound (T5: one
 *  gain path for both, the caller picks the map). */
/** Signal task 10b: the input and output volume of Settings → Voice: a whole percent 0..200. */
export function isPercent(v: number): boolean {
  return Number.isInteger(v) && v >= 0 && v <= 200;
}

/** Signal task 10b: `output` is Settings → Voice's output volume (0..200 %, 100 by default), multiplied in. */
export function gainOf(userId: string, volumes: Volumes, s: { deafened: boolean; paused: boolean; output?: number }): number {
  if (s.deafened || s.paused) return 0;
  const v = volumes[userId];
  const output = s.output !== undefined && isPercent(s.output) ? s.output : 100;
  return ((v !== undefined && isVolume(v) ? v : 100) / 100) * (output / 100);
}

/** The volumes the settings slice holds, keyed by user id; malformed entries are skipped. */
export function volumesFromSettings(settings: Record<string, string>): Volumes {
  return pick(settings, VOLUME_KEY);
}

/** T5: the stream volumes (`voice.streamVolume.<user>`), by user id. A stream volume of 0 is the stream mute. */
export function streamVolumesFromSettings(settings: Record<string, string>): Volumes {
  return pick(settings, STREAM_KEY);
}

function pick(settings: Record<string, string>, key: RegExp): Volumes {
  const out: Volumes = {};
  for (const [k, value] of Object.entries(settings)) {
    const m = key.exec(k);
    if (m && VOLUME_VALUE.test(value)) out[m[1]] = Number(value);
  }
  return out;
}
