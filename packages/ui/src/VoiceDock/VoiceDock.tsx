import { useId, type ReactNode } from 'react';
import { VOICE_ICONS, voiceWhy, type VoiceWhyLabels } from '../CallControls/CallControls.tsx';
import './VoiceDock.css';
import { Icon } from '../Icon/Icon.tsx';
import { IconButton } from '../IconButton/IconButton.tsx';
import { useIconToolbar } from '../internal/iconToolbar.ts';
import { StateDot } from '../StateDot/StateDot.tsx';

export interface VoiceDockProps {
  phase: 'connecting' | 'connected' | 'reconnecting' | 'paused';
  /** The voice channel's or the DM's name: line 1 (MP5). Filled into `{where}` of `labels.paused`. */
  where: string;
  /** MP5: the latest link read, line 2 while connected: ping, what this device sends and receives, and the quality that
   *  colours it (`poor` warn, `bad` danger; the accessible text names it too). Absent before the first sample: "— ms". */
  stats?: { rttMs: number; /** Null: this direction could not be read; the dock shows a dash. */ upKbps: number | null; downKbps: number | null; quality: 'good' | 'poor' | 'bad' };
  /** Not drawn since web3.9 (R26): the user panel owns mute and deafen. Kept so existing callers still type-check. */
  muted: boolean;
  /** Not drawn since web3.9 (R26): see `muted`. */
  deafened: boolean;
  /** The camera is live (a pressed toggle). Shown only with `onCamera`. */
  camera?: boolean;
  /** This device shares its screen (a pressed toggle). Shown only with `onShare`. */
  sharing?: boolean;
  /** False while the call is paused or held (RV-3): the camera control is aria-disabled. (No caller checks for a camera device yet.) */
  canCamera?: boolean;
  /** False without screen capture, or while the call is paused (RV-3): the share control is aria-disabled. */
  canShare?: boolean;
  /** True while joining (the stage's `busy`): camera and share are aria-disabled. Leave stays live. */
  busy?: boolean;
  /** True while the call is paused or held; it picks the reason camera and share give. Absent, the paused phase or
   *  `!canCamera` stands for it. */
  paused?: boolean;
  /** Not drawn since web3.9 (R26). */
  shortcuts?: { mute: string; deafen: string };
  /** The latest polite announcement (join, leave, mute, deafen, the end of a pause, the outgoing ring); '' for none. */
  announcement: string;
  labels: {
    /** `voice.dock.region`: the control group's name. */
    region: string;
    connecting: string;
    /** `layout.dock.where`, filled by the caller ('{channel} · {server}'): line 1's tooltip and accessible name. Without it, `where`. */
    where?: string;
    reconnecting: string;
    /** `voice.dock.ping` ('{ms} ms'), `voice.dock.pingSub` ('<1 ms', a ping that rounds to 0: a real LAN reading, not a missing one), `voice.dock.pingNone` ('— ms') and `voice.dock.rate` ('↑ {up} ↓ {down} kb/s'): line 2 while connected. */
    ping: string; pingSub: string; pingNone: string; rate: string;
    /** `voice.dock.stats`: the stats' accessible text, with `{ms}`, `{quality}`, `{up}` and `{down}`; never announced. */
    stats: string;
    /** `voice.dock.quality.*`: the quality words that fill `{quality}`. */
    quality: { good: string; poor: string; bad: string };
    /** `voice.dock.paused`; a `{where}` in it is still filled. */
    paused: string;
    /** Not drawn since web3.9 (R26). */
    mute: string; deafen: string;
    leave: string;
    /** `voice.cameraOn`; needed with `onCamera`. */
    camera?: string;
    /** `voice.shareOn`; needed with `onShare`. */
    share?: string;
    /** The reasons an inert camera or share gives (`voice.why.*`), read as its description; none without it. */
    why?: VoiceWhyLabels;
  };
  /** Task 8b (README §5.4, §6.6): the compact now-playing row under the dock's row while the bot plays in this call (NowPlaying `compact`). */
  music?: ReactNode;
  /** False renders the two status lines and the status region, and no controls (the phone's call bar holds the controls). Default true. */
  controls?: boolean;
  /** Ignored since web3.9 (R26). */
  onMute?(): void;
  /** Ignored since web3.9 (R26). */
  onDeafen?(): void;
  /** With it, line 1 (the channel name) is a button: it opens Diagnostics (R26). */
  onStatus?(): void;
  /** Toggles the camera, as the stage's control does. Without it (or `labels.camera`) the dock shows no camera control. */
  onCamera?(): void;
  /** Toggles screen sharing, as the stage's control does. Without it (or `labels.share`) the dock shows no share control. */
  onShare?(): void;
  onLeave(): void;
}

/** Connected: the three --live bars (README §5.4, `d-bar` 600ms, staggered 200ms); still under reduced motion. */
const BARS = <span className="d-voice-dock__bars"><span /><span /><span /></span>;
const PHASE_GLYPH = {
  connecting: <StateDot tone="pending" />, connected: BARS,
  reconnecting: <StateDot tone="pending" />, paused: <Icon name="pause" size={14} />,
} as const;

/**
 * The voice dock above the user panel while this device is in voice (Signal, README §5.4): a card in the --live tints
 * (margin 0 8px 8px, padding 10px, radius 8px) holding one row: the three --live bars while connected (the phase marker
 * otherwise), the room name (12px/600 --ok-text; a button with `onStatus` that opens Diagnostics, the server is its tooltip
 * and accessible name) over the link stats ("24 ms · ↑ 38 ↓ 71 kb/s", 10.5px tabular --fg-2; the phase words while not
 * connected), then camera and share (when given) and leave as 28px icon buttons in one roving Tab stop
 * (`role="toolbar"`). Leave is the --danger glyph and fills --danger on hover. Mute and deafen live in the user panel.
 * A camera or share that cannot act is aria-disabled and says why as its description. Its one `role="status"` region
 * announces join, leave, mute, deafen and the end of a pause (flow 06).
 */
export function VoiceDock({
  phase, where, stats, camera = false, sharing = false, canCamera = true, canShare = true, busy = false, paused,
  announcement, labels, controls: showControls = true, music, onStatus, onCamera, onShare, onLeave,
}: VoiceDockProps) {
  const why = voiceWhy(labels.why, { busy, paused: paused ?? (phase === 'paused' || !canCamera), canCamera, canShare });
  const ids = useId();
  const bar = useIconToolbar();
  const full = labels.where ?? where;
  const line1 = (
    <>
      <span className="d-voice-dock__text" aria-hidden="true">{where}</span>
      <span className="d-sr-only">{full}</span>
    </>
  );
  const fill = (template: string, values: Record<string, string | number>): string =>
    template.replace(/\{(\w+)\}/g, (m, k: string) => (k in values ? String(values[k]) : m));
  const phaseWords = { connecting: labels.connecting, reconnecting: labels.reconnecting, paused: fill(labels.paused, { where }) };
  // Line 2: the stats while connected (visible text for sight, a spoken sentence that names the quality for a screen reader),
  // else the phase words. The visible numbers are aria-hidden: the sentence carries them once.
  const line2 = phase !== 'connected' ? phaseWords[phase] : stats === undefined ? (
    <span className="d-voice-dock__stats" data-quality="none">{labels.pingNone}</span>
  ) : (
    <span className="d-voice-dock__stats" data-quality={stats.quality}>
      <span aria-hidden="true">{stats.rttMs < 1 ? labels.pingSub : fill(labels.ping, { ms: stats.rttMs })} · {fill(labels.rate, { up: stats.upKbps ?? '—', down: stats.downKbps ?? '—' })}</span>
      <span className="d-sr-only">{fill(labels.stats, { ms: stats.rttMs, quality: labels.quality[stats.quality], up: stats.upKbps ?? '—', down: stats.downKbps ?? '—' })}</span>
    </span>
  );
  const cameraInert = busy || !canCamera;
  const shareInert = busy || !canShare;
  return (
    <div className="d-voice-dock" data-phase={phase}>
      <div className="d-voice-dock__row">
      <span className="d-voice-dock__glyph" aria-hidden="true">{PHASE_GLYPH[phase]}</span>
      <div className="d-voice-dock__status">
        {onStatus !== undefined
          ? <button type="button" className="d-voice-dock__line1" title={full} onClick={onStatus}>{line1}</button>
          : <p className="d-voice-dock__line1" title={full}>{line1}</p>}
        <p className="d-voice-dock__where">{line2}</p>
      </div>
      {showControls ? (
        <div ref={bar.ref} role="toolbar" aria-label={labels.region} className="d-voice-dock__controls" onKeyDown={bar.onKeyDown} onFocus={bar.onFocus}>
          {onCamera !== undefined && labels.camera !== undefined ? (
            <>
              <IconButton icon={camera ? VOICE_ICONS.camera.iconOn : VOICE_ICONS.camera.icon} size={28} label={labels.camera} pressed={camera}
                aria-disabled={cameraInert ? true : undefined} aria-describedby={cameraInert && why.camera ? `${ids}-camera` : undefined}
                onClick={cameraInert ? undefined : onCamera} />
              {cameraInert && why.camera ? <span id={`${ids}-camera`} className="d-sr-only">{why.camera}</span> : null}
            </>
          ) : null}
          {onShare !== undefined && labels.share !== undefined ? (
            <>
              <IconButton icon={sharing ? VOICE_ICONS.share.iconOn : VOICE_ICONS.share.icon} size={28} label={labels.share} pressed={sharing}
                aria-disabled={shareInert ? true : undefined} aria-describedby={shareInert && why.share ? `${ids}-share` : undefined}
                onClick={shareInert ? undefined : onShare} />
              {shareInert && why.share ? <span id={`${ids}-share`} className="d-sr-only">{why.share}</span> : null}
            </>
          ) : null}
          <IconButton icon={VOICE_ICONS.leave.icon} size={28} label={labels.leave} className="d-voice-dock__leave" onClick={onLeave} />
        </div>
      ) : null}
      </div>
      {music ?? null}
      <p className="d-sr-only" role="status">{announcement}</p>
    </div>
  );
}
