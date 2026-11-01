// Signal task 8c (README §5.6, the prototype's tile stats): what the stage's tiles say about their streams, read from the
// RTCPeerConnection stats of each track (a sender's outbound-rtp, a receiver's inbound-rtp). Nothing here leaves the page.
// A person's ping is not read here: each device reports its own RTT through the instance (session.ts, POST …/rtt), which
// writes it as that device's dilla.rtt attribute, and the tiles read it from there (PageCallTile.rttMs).

/** One read of the stage's stats; a value the browser did not report (yet) is absent, never guessed. */
export interface TileStats {
  /** The own microphone as sent: its codec ('opus') and clock rate (48000). */
  ownAudio: { codec: string; clockRate: number } | null;
  /** The own screen as sent (its top layer): frame height and rate, and whether its sound is published with it. */
  ownScreen: { height: number; fps: number; audio: boolean } | null;
  /** Each remote screen as received, by device id (the LiveKit identity). */
  screens: Record<string, { height: number; fps: number }>;
}

export const EMPTY_TILE_STATS: TileStats = { ownAudio: null, ownScreen: null, screens: {} };

type Report = Pick<RTCStatsReport, 'forEach' | 'get'>;
interface StatsTrack { getRTCStatsReport?(): Promise<RTCStatsReport | undefined> }
interface Stat { type?: string; kind?: string; codecId?: string; mimeType?: string; clockRate?: number; frameHeight?: number; framesPerSecond?: number }

const each = (r: Report, fn: (s: Stat) => void): void => { r.forEach((s: unknown) => { fn(s as Stat); }); };

/** The sent audio's codec: the outbound-rtp audio entry's codec (`audio/opus` → 'opus', with its clock rate). */
export function audioCodecOf(r: Report | undefined): TileStats['ownAudio'] {
  if (r === undefined) return null;
  let out: TileStats['ownAudio'] = null;
  each(r, (s) => {
    if (out !== null || s.type !== 'outbound-rtp' || s.kind !== 'audio' || s.codecId === undefined) return;
    const c = r.get(s.codecId) as Stat | undefined;
    const name = c?.mimeType?.split('/')[1];
    if (name !== undefined && name !== '' && typeof c?.clockRate === 'number') out = { codec: name.toLowerCase(), clockRate: c.clockRate };
  });
  return out;
}

/**
 * A video's frame as `type` (outbound-rtp for a sender, inbound-rtp for a receiver) reports it: the largest layer's frame
 * height and its frames per second, rounded. Null until the browser reports both.
 */
export function videoFrameOf(r: Report | undefined, type: 'outbound-rtp' | 'inbound-rtp'): { height: number; fps: number } | null {
  if (r === undefined) return null;
  let best: { height: number; fps: number } | null = null;
  each(r, (s) => {
    if (s.type !== type || s.kind !== 'video' || typeof s.frameHeight !== 'number' || typeof s.framesPerSecond !== 'number') return;
    if (s.frameHeight <= 0) return;
    if (best === null || s.frameHeight > best.height) best = { height: s.frameHeight, fps: Math.round(s.framesPerSecond) };
  });
  return best;
}

const statsOf = async (track: unknown): Promise<Report | undefined> => {
  const t = track as StatsTrack | undefined;
  try { return await t?.getRTCStatsReport?.(); } catch { return undefined; }
};

/** What `readTileStats` needs of the session: its own publications, the remote participants, and whether it ended. */
export interface TileStatsSource {
  ended(): boolean;
  micPub(): { track?: unknown } | null;
  screenPub(): { track?: unknown } | null;
  shareAudio(): boolean;
  remotes(): Array<{ identity: string; getTrackPublication(source: never): { track?: unknown } | undefined }>;
  screenSource: string;
}

/** One read: the own microphone and screen, and every remote screen, in parallel. A failed read is an absent value. */
export async function readTileStats(src: TileStatsSource): Promise<TileStats> {
  if (src.ended()) return EMPTY_TILE_STATS;
  const mic = src.micPub();
  const screen = src.screenPub();
  const remote = src.remotes().map((p) => ({ id: p.identity, track: p.getTrackPublication(src.screenSource as never)?.track }))
    .filter((p) => p.track !== undefined);
  const [micReport, screenReport, ...remoteReports] = await Promise.all([
    mic === null ? undefined : statsOf(mic.track),
    screen === null ? undefined : statsOf(screen.track),
    ...remote.map((p) => statsOf(p.track)),
  ]);
  const own = videoFrameOf(screenReport, 'outbound-rtp');
  const screens: TileStats['screens'] = {};
  remote.forEach((p, i) => {
    const frame = videoFrameOf(remoteReports[i], 'inbound-rtp');
    if (frame !== null) screens[p.id] = frame;
  });
  return { ownAudio: audioCodecOf(micReport), ownScreen: own === null ? null : { ...own, audio: src.shareAudio() }, screens };
}
