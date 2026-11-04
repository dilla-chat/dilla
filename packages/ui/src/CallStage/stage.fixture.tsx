// Task 8 (restyle rule 3d): the voice stage as packages/web/src/screens/voice/VoiceView.tsx composes it in Signal, with the
// prototype's sample room (harbour: Ada and Kari share, Björn and Kari have cameras, Ole, Mira (you), Glenn the bot). The props
// are the ones the web passes: `bar` (the room and the stats line), the call bar's words (`labels.bar`), `labels.grid`, the
// tile ids `${device}:person` / `${device}:screen`, "{name} (you)" and "{name} · playback only", and the not-joined `preview`.
import { useEffect, useState, type ReactNode } from 'react';
import { CallStage, type CallStageProps } from './CallStage.tsx';
import { CallControls, type CallControlsProps } from '../CallControls/CallControls.tsx';
import type { SharePickerValue } from '../SharePicker/SharePicker.tsx';
import type { CallTileProps, TileMenu, VoiceParticipantView } from '../CallTile/CallTile.tsx';
import { ChannelHeader } from '../ChannelHeader/ChannelHeader.tsx';

const noop = () => {};
export const STAGE_STATE: CallTileProps['labels']['state'] = {
  speaking: 'speaking', muted: 'muted', deafened: 'deafened', notHeard: 'not heard yet', unverified: 'unverified stream',
  connecting: 'connecting', video: 'camera on', screen: 'sharing screen', web: 'web', bot: 'bot', noSound: 'no sound',
};
export const STAGE_LABELS: CallStageProps['labels'] = {
  code: 'call code', empty: 'Nobody is in voice yet.', tiles: 'people in voice', grid: 'Grid',
  codeHelp: 'The code changes whenever someone joins or leaves. Read it aloud: if every screen shows the same code, everyone is hearing the same set of devices.',
};
/** VoiceView `Controls` for a channel: the region, the names, why, and the call bar's words. */
export const BAR_CONTROL_LABELS: CallControlsProps['labels'] = {
  region: 'voice channel controls', mute: 'mute', deafen: 'deafen', camera: 'camera', share: 'share screen', leave: 'leave',
  why: { paused: 'paused', joining: 'connecting', noShare: 'not available in this browser' },
  bar: { mute: 'Mute', unmute: 'Unmute', deafen: 'Deafen', undeafen: 'Undeafen', camera: 'Camera', share: 'Share', stopShare: 'Stop sharing', leave: 'Leave' },
};

/**
 * Task 8c: the share picker as VoiceView's useSharePicker builds it on Chromium (every source, sound by source), with the web's
 * copy and its estimate (packages/web/src/call/share-options.ts: 1080p at 30 fps is 4.5 Mb/s; the prototype's table). The story
 * keeps its own state so the controls can be tried.
 */
const KBPS: Record<SharePickerValue['resolution'], number> = { '720p': 2500, '1080p': 4500, '1440p': 8000, source: 12000 };
const FPS_FACTOR: Record<SharePickerValue['fps'], number> = { 15: 0.6, 30: 1, 60: 1.6 };
export const PICKER_DEFAULT: SharePickerValue = { source: 'screen', resolution: '1080p', fps: 30, audio: true };
export function pickerProps(value: SharePickerValue, o: { open?: boolean; onChange?(v: SharePickerValue): void; tabOnly?: boolean; noSource?: boolean } = {}): NonNullable<CallControlsProps['picker']> {
  const kbps = KBPS[value.resolution] * FPS_FACTOR[value.fps];
  const heavy = kbps > 9000;
  const mbps = (kbps / 1000).toFixed(1);
  const tab = value.source === 'tab';
  return {
    open: o.open ?? true, onOpenChange: noop, quality: 'Stream quality', value, onChange: o.onChange ?? noop,
    sources: o.noSource === true ? [] : ['screen', 'window', 'tab'],
    audio: o.noSource === true ? 'none' : o.tabOnly === true && !tab ? 'note' : 'toggle',
    preview: value.source === 'screen' ? 'Display · 2560×1440' : tab ? 'Tab · you pick it next' : 'Window · you pick it next',
    estimate: { text: heavy ? `≈ ${mbps} Mb/s up · may drop frames on your link` : `≈ ${mbps} Mb/s up`, heavy },
    labels: {
      title: 'Share your screen', titleSharing: 'Stream quality', close: 'Close', source: 'Source', resolution: 'Resolution', frameRate: 'Frame rate',
      sources: { screen: 'Screen', window: 'Window', tab: 'Tab' }, native: 'Source', audio: 'Also share system audio',
      audioNote: tab ? 'Sound from this tab' : o.tabOnly === true ? 'Chrome on Linux can share sound only from a tab' : 'Sound from the whole system',
      go: 'Go live', apply: 'Apply',
    },
    onSubmit: noop,
  };
}

export function stageControls(more: Partial<CallControlsProps> = {}): ReactNode {
  return (
    <CallControls muted={false} deafened={false} camera={false} sharing={false} canCamera canShare busy={false} paused={false}
      labels={BAR_CONTROL_LABELS} shortcuts={{ mute: '⌘⇧M', deafen: '⌘⇧D' }} keyshortcuts={false}
      // Task 8c: a channel's call bar always carries the share picker (VoiceView's useSharePicker), closed until Share.
      picker={pickerProps(PICKER_DEFAULT, { open: false })}
      onMute={noop} onDeafen={noop} onCamera={noop} onShare={noop} onLeave={noop} {...more} />
  );
}

type StageTile = CallStageProps['tiles'][number];
const view = (name: string, more: Partial<VoiceParticipantView> = {}): VoiceParticipantView => ({
  deviceId: `d-${name}`, userId: `u-${name}`, name, state: 'ok', speaking: false, muted: false, deafened: false,
  video: false, screen: false, web: false, bot: false, ...more,
});
const labels = (name: string): CallTileProps['labels'] => ({ state: STAGE_STATE, volume: `volume for ${name}`, volumeValue: '{n} %' });

/**
 * The person menu VoiceView gives a tile (wave B task 4, the prototype's). Yourself: Mute and Deafen with their keycaps, Profile, Move
 * to the other voice channels, Leave voice. Another person: the volume, Profile, Message, Mention, Mute for me, Copy username, Edit
 * roles (the app draws Edit roles only with Manage roles: `roles: false` leaves it out). A bot has no Message. A shared screen keeps
 * the volume, Mute for me and Copy username.
 */
export function tileMenu(name: string, o: { own?: boolean; bot?: boolean; screen?: boolean; silent?: boolean; volume?: number; muted?: boolean; title?: string; mention?: boolean; roles?: boolean } = {}): TileMenu {
  const volume = o.volume ?? 100;
  const head = { person: { name: o.title ?? name, avatarName: name, where: 'in harbour', ...(o.bot === true ? { bot: true } : {}) }, label: `actions for ${name}`, onSelect: noop };
  if (o.own === true) {
    return { ...head, items: [
      { id: 'mute', label: 'Mute', icon: 'mic-off' as const, hint: 'Ctrl⇧M', keys: 'Control+Shift+M' }, { id: 'deafen', label: 'Deafen', icon: 'headphone-off' as const, hint: 'Ctrl⇧D', keys: 'Control+Shift+D' },
      { id: 'profile', label: 'Profile', icon: 'user' as const }, { id: 'leave', label: 'Leave voice', icon: 'phone-off' as const, danger: true, sep: true }],
    move: { label: 'Move to', rooms: [{ id: 'lounge', name: 'lounge', count: 2 }, { id: 'raid', name: 'raid', count: 0 }],
      roomLabel: r => `${r.name}, ${String(r.count)} in voice`, onMove: noop } };
  }
  const person = o.screen !== true && o.silent !== true;
  return {
    ...head,
    ...(o.silent === true ? {} : { volume: { label: o.screen === true ? 'Stream volume' : 'User volume', value: volume, valueText: `${String(volume)}%`, onChange: noop } }),
    items: [
      ...(person ? [{ id: 'profile', label: 'Profile', icon: 'user' as const }] : []),
      ...(person && o.bot !== true ? [{ id: 'message', label: 'Message', icon: 'message-square' as const }] : []),
      ...(person && o.mention !== false ? [{ id: 'mention', label: 'Mention', icon: 'at-sign' as const }] : []),
      ...(o.silent === true ? [] : [{ id: 'mute', label: 'Mute for me', icon: 'volume-x' as const, check: o.muted === true }]),
      { id: 'copy', label: 'Copy username', icon: 'copy' as const },
      ...(person && o.roles !== false ? [{ id: 'roles', label: 'Edit roles', icon: 'shield' as const }] : []),
    ],
  };
}

/** A person's tile as VoiceView's deviceTiles makes it: camera with a stream, else avatar; one id either way. */
export function personTile(name: string, more: Partial<VoiceParticipantView> & { own?: boolean; title?: string; stat?: string } = {}): StageTile {
  const { own = false, title, stat, ...v } = more;
  const vw = view(name, v);
  return { view: vw, own, volume: 100, media: null, labels: labels(name), id: `${vw.deviceId}:person`, ...(stat !== undefined ? { stat } : {}),
    source: vw.video ? 'camera' : 'avatar', title: title ?? name, fullscreenLabel: 'full screen', mirror: own && vw.video,
    menu: tileMenu(name, { own, bot: vw.bot, ...(title !== undefined ? { title } : {}) }) };
}
export function screenTile(name: string, stat?: string): StageTile {
  const vw = view(name, { screen: true });
  return { view: vw, own: false, volume: 100, media: null, labels: labels(name), id: `${vw.deviceId}:screen`, ...(stat !== undefined ? { stat } : {}),
    source: 'screen', title: `${name}’s screen`, fullscreenLabel: 'full screen', mirror: false, menu: tileMenu(name, { screen: true }) };
}

/** The prototype's harbour, joined: the two shares first, then the people in the room's order, Mira (you) and Glenn last. */
export function harbourTiles(speaking: string | null = null): StageTile[] {
  const s = (n: string) => (speaking === n ? { speaking: true } : {});
  return [
    // Task 8c: the stats VoiceView passes: a received screen's frame, the own microphone's codec, and (fix round 1) each person's
    // ping as their device reports it (dilla.rtt through dillad; the prototype's sample values), the bot's too (Ruling AU).
    screenTile('Ada', '1440p · 30 fps'), screenTile('Kari', '1440p · 30 fps'),
    personTile('Ada', { stat: '18 ms', ...s('Ada') }), personTile('Björn', { video: true, stat: '27 ms', ...s('Björn') }),
    personTile('Kari', { video: true, stat: '36 ms', ...s('Kari') }),
    personTile('Ole', { stat: '45 ms', ...s('Ole') }), personTile('Mira', { own: true, title: 'Mira (you)', stat: 'opus 48k', ...s('Mira') }),
    personTile('Glenn', { bot: true, title: 'Glenn · playback only', stat: '63 ms', ...s('Glenn') }),
  ];
}

/**
 * A stand-in picture drawn once on a canvas at story time: a share is the prototype's 135° stripe on the tile colour, a camera
 * the stripe over a dim radial glow of the person's hue. Its tracks stop on unmount.
 */
export function useDrawnStream(kind: 'screen' | 'camera', hue = 211): MediaStream | null {
  const [stream, setStream] = useState<MediaStream | null>(null);
  useEffect(() => {
    const canvas = document.createElement('canvas');
    canvas.width = 640; canvas.height = 360;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;
    ctx.fillStyle = '#0E1116'; ctx.fillRect(0, 0, 640, 360);
    if (kind === 'camera') {
      const g = ctx.createRadialGradient(320, 126, 0, 320, 126, 480);
      g.addColorStop(0, `hsl(${String(hue)} 30% 20%)`); g.addColorStop(0.75, '#0C0F13');
      ctx.fillStyle = g; ctx.fillRect(0, 0, 640, 360);
    }
    ctx.strokeStyle = 'rgba(255,255,255,0.03)'; ctx.lineWidth = 16 / Math.SQRT2;
    for (let x = -360; x < 1000; x += 16 * Math.SQRT2) { ctx.beginPath(); ctx.moveTo(x, 360); ctx.lineTo(x + 360, 0); ctx.stroke(); }
    const s = canvas.captureStream(1);
    setStream(s);
    return () => { s.getTracks().forEach(t => t.stop()); };
  }, [kind, hue]);
  return stream;
}

/** The tiles with the drawn pictures the web would attach (a share's stream, a camera's), as remote verified streams. */
export function WithPictures({ tiles, children }: { tiles: readonly StageTile[]; children: (tiles: StageTile[]) => ReactNode }) {
  const screen = useDrawnStream('screen');
  const bjorn = useDrawnStream('camera', 211);
  const kari = useDrawnStream('camera', 12);
  const media = (t: StageTile): MediaStream | null => t.source === 'screen' ? screen : t.source === 'camera' ? (t.view.name === 'Kari' ? kari : bjorn) : null;
  return <>{children(tiles.map(t => ({ ...t, media: media(t) })))}</>;
}

/** The voice header the web passes on desktop (Ruling Z: the name and its topic, no controls). */
export const harbourHeader = (topic: string) => <ChannelHeader kind="voice" name="harbour" topic={topic} />;

/** The stage of a joined channel, as VoiceView's Stage renders it, with its own focus state. */
export function HarbourStage({ tiles, focus = null, controls, header = true, stats = '24 ms · ↑ 38 ↓ 71 kb/s · hearing 5 of 5', topic = '6 in voice',
  name = 'harbour', nowPlaying }: {
  tiles: readonly StageTile[]; focus?: string | null; controls?: ReactNode; header?: boolean; stats?: string; topic?: string; name?: string;
  /** Task 8b: the now-playing bar VoiceView passes while the bot plays in this room (the prototype's harbour: `harbourStageBar`). */
  nowPlaying?: ReactNode;
}) {
  const [focusedId, setFocusedId] = useState<string | null>(focus);
  return (
    <WithPictures tiles={tiles}>
      {pictured => (
        <CallStage title={`voice ${name}`} tiles={pictured} {...(header ? { header: <ChannelHeader kind="voice" name={name} topic={topic} /> } : {})}
          focusedId={focusedId} onFocus={setFocusedId} code={null} notice={null} outgoing={null} nowPlaying={nowPlaying}
          bar={{ name, stats }} controls={controls ?? stageControls()} labels={STAGE_LABELS} />
      )}
    </WithPictures>
  );
}

/** The not-joined preview of harbour, as VoiceView passes it (who is inside, the room, Join voice). */
export function HarbourPreview({ header = true }: { header?: boolean }) {
  return (
    <CallStage title="voice harbour" tiles={[]} {...(header ? { header: harbourHeader('voice channel') } : {})} code={null} notice={null} outgoing={null}
      controls={null} labels={STAGE_LABELS}
      preview={{ name: 'harbour', text: 'Ada and Björn and Kari and Ole and Glenn are here', join: { label: 'Join voice', onJoin: noop },
        people: ['Ada', 'Björn', 'Kari', 'Ole', 'Glenn'].map(n => ({ id: n, name: n, speaking: false, bot: n === 'Glenn' })) }} />
  );
}
