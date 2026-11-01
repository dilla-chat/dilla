import { useEffect, useId, useRef, useState } from 'react';
import { newToastHostId, showToast, ToastHost } from '../Toast/Toast.tsx';
import { Avatar } from '../Avatar/Avatar.tsx';
import { Icon } from '../Icon/Icon.tsx';
import './NowPlaying.css';

export interface NowPlayingTrack { title: string; by: string; durationS: number }
export interface NowPlayingQueueItem { title: string; by: string; durationS?: number; /** The track playing now (the row is tinted). */ current?: boolean }

export interface NowPlayingLabels {
  /** The player's accessible name, e.g. 'Music player'. */
  region: string;
  /** The state line: 'Now playing', 'Paused', 'Stopped'. */
  playing: string; paused: string; stopped: string;
  /** The title while nothing plays: 'Nothing playing'. */
  idle: string;
  /** The controls' names. */
  play: string; pause: string; skip: string; stop: string;
  /** The progress bar's name ('Playback position') and its spoken value, with `{pos}` and `{dur}` ('{pos} of {dur}'). */
  position: string; positionValue: string;
  /** The card's queue toggle, with `{n}` ('Up next · {n}'), and the stage bar's count ('{n} up next'). */
  queue: string; upNext: string;
  /** The card's volume, with `{n}` ('{n}%'), and its spoken name ('Volume'). */
  volume: string; volumeName: string;
  /** The queue list's name ('Queue'). */
  queueList: string;
}

export interface NowPlayingProps {
  /** `card`: the player in the channel where /play ran; `compact`: the row inside the voice dock; `stage`: the bar on top of the voice stage. */
  variant: 'card' | 'compact' | 'stage';
  /** Absent: nothing plays (the idle card; the compact row and the stage bar are not drawn without a track). */
  track?: NowPlayingTrack;
  positionS: number;
  playing: boolean;
  /** The voice channel the player is in. */
  room?: string;
  /** The card's and the stage bar's second line, e.g. 'YouTube · requested by Ada · harbour' (or the idle sentence). */
  meta?: string;
  /** The playback volume, 0–100 (the card shows it). */
  volume?: number;
  /** The bot that plays (the dock row's and the stage bar's square avatar). */
  bot?: { name: string; initials?: string };
  /** You may control playback: in the same voice channel and allowed by the current DJ rule. */
  canControl: boolean;
  /** Without control, what a press says in a toast, e.g. 'Join harbour to control playback'. */
  blocked?: string;
  /** A message to show as a toast once per `id` (e.g. a control the Bot refused). */
  toast?: { text: string; id: number };
  queue: NowPlayingQueueItem[];
  /** The card's queue list, open or closed; uncontrolled without it. */
  queueOpen?: boolean;
  onQueueToggle?(open: boolean): void;
  onToggle(): void;
  onSkip(): void;
  onStop(): void;
  labels: NowPlayingLabels;
}

/** m:ss, as the prototype prints a position and a duration. */
export function playerTime(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  return `${String(Math.floor(s / 60))}:${String(s % 60).padStart(2, '0')}`;
}

const fill = (template: string, values: Record<string, string | number>): string =>
  template.replace(/\{(\w+)\}/g, (m, k: string) => (k in values ? String(values[k]) : m));

/** The prototype's three --live bars beside a playing state (d-bar 520ms, 130ms apart). */
const BARS = <span className="d-np__bars" aria-hidden="true"><span /><span /><span /></span>;

/**
 * The music player (Signal README §6.6, §9 NowPlayingProps; the prototype's np card, dock row and stage bar): three views of one
 * player state. The card holds a 52px art placeholder, the state line (with the --live bars while playing), the title and the
 * meta line, a 4px progress bar with times, play/pause, skip, stop, the volume and the Up next toggle that opens the queue. The
 * compact row sits inside the voice dock (the bot's 22px square, the title over "state · pos / dur", play/pause and skip, a
 * 3px bar). The stage bar is 48px on top of the voice stage (the bot's 28px square, the bars, title and meta over a 3px bar
 * with the times, play/pause, skip, stop, "n up next").
 *
 * Without control, the controls are aria-disabled and say why (`blocked`) as their description; a press shows it as a toast
 * for 2.4s (the prototype's toast) and announces it.
 */
export function NowPlaying({
  variant, track, positionS, playing: playingProp, meta, volume, bot, canControl, blocked = '', toast: toastProp, queue, queueOpen, onQueueToggle,
  onToggle, onSkip, onStop, labels,
}: NowPlayingProps) {
  const ids = useId();
  const [openOwn, setOpenOwn] = useState(false);
  const open = queueOpen ?? openOwn;
  const [hostId] = useState(newToastHostId);
  const show = showToast;
  const shown = useRef<number | null>(null);
  const toastId = toastProp?.id ?? null;
  const toastText = toastProp?.text ?? '';
  useEffect(() => {
    if (toastId === null || shown.current === toastId) return;
    shown.current = toastId;
    show(toastText);
  }, [toastId, toastText]);
  const playing = track !== undefined && playingProp;
  const duration = track?.durationS ?? 0;
  const position = track === undefined ? 0 : Math.min(Math.max(positionS, 0), duration);
  const pct = duration > 0 ? `${((position / duration) * 100).toFixed(2)}%` : '0%';
  const pos = playerTime(position);
  const dur = playerTime(duration);
  const state = track === undefined ? labels.stopped : playing ? labels.playing : labels.paused;
  const upNext = queue.filter(q => q.current !== true).length;

  const act = (run: () => void) => () => {
    if (canControl) { run(); return; }
    show(blocked);
  };
  const why = !canControl && blocked !== '' ? `${ids}-why` : undefined;
  const control = (cls: string, name: string, icon: Parameters<typeof Icon>[0]['name'], size: number, run: () => void, disabled = false) => (
    <button type="button" className={`d-np__btn ${cls}`} aria-label={name} aria-disabled={!canControl || disabled ? true : undefined}
      aria-describedby={why} onClick={disabled && canControl ? undefined : act(run)}>
      <Icon name={icon} size={size} />
    </button>
  );
  const toggleName = playing ? labels.pause : labels.play;
  const toggleIcon = playing ? 'pause' : 'play';
  const sizes = variant === 'card' ? { play: 15, skip: 15, stop: 14 } : variant === 'stage' ? { play: 14, skip: 14, stop: 13 } : { play: 13, skip: 13, stop: 13 };
  const toggle = control('d-np__toggle', toggleName, toggleIcon, sizes.play, onToggle, track === undefined);
  const skip = control('d-np__skip', labels.skip, 'skip-forward', sizes.skip, onSkip, track === undefined);
  const stop = control('d-np__stop', labels.stop, 'square', sizes.stop, onStop, track === undefined);
  const progress = (
    <span className="d-np__track" role="progressbar" aria-label={labels.position} aria-valuemin={0} aria-valuemax={Math.round(duration)}
      aria-valuenow={Math.round(position)} aria-valuetext={fill(labels.positionValue, { pos, dur })}>
      <span className="d-np__fill" style={{ width: pct }} />
    </span>
  );
  const botTile = bot !== undefined ? <Avatar name={bot.name} {...(bot.initials !== undefined ? { initials: bot.initials } : {})} shape="bot" size="sug" /> : null;
  const live = (
    <>
      {why !== undefined ? <span id={why} className="d-sr-only">{blocked}</span> : null}
      <ToastHost id={hostId} />
    </>
  );

  if (variant === 'compact') {
    if (track === undefined) return null;
    return (
      <div className="d-np d-np--compact" role="group" aria-label={labels.region} data-playing={playing ? 'true' : undefined}>
        <div className="d-np__row">
          {botTile}
          <span className="d-np__text">
            <span className="d-np__title">{track.title}</span>
            <span className="d-np__sub">{state} · {pos} / {dur}</span>
          </span>
          {toggle}
          {skip}
        </div>
        {progress}
        {live}
      </div>
    );
  }

  if (variant === 'stage') {
    if (track === undefined) return null;
    return (
      <div className="d-np d-np--stage" role="group" aria-label={labels.region} data-playing={playing ? 'true' : undefined}>
        {botTile}
        {playing ? BARS : null}
        <span className="d-np__text">
          <span className="d-np__line">
            <b className="d-np__title">{track.title}</b>
            {meta !== undefined ? <span className="d-np__meta">{meta}</span> : null}
          </span>
          <span className="d-np__line d-np__line--bar">
            {progress}
            <span className="d-np__times">{pos} / {dur}</span>
          </span>
        </span>
        {toggle}
        {skip}
        {stop}
        <span className="d-np__upnext">{fill(labels.upNext, { n: upNext })}</span>
        {live}
      </div>
    );
  }

  const listId = `${ids}-queue`;
  const setOpen = (next: boolean) => { if (queueOpen === undefined) setOpenOwn(next); onQueueToggle?.(next); };
  return (
    <div className="d-np d-np--card" role="group" aria-label={labels.region} data-playing={playing ? 'true' : undefined}>
      <div className="d-np__head">
        <span className="d-np__art" aria-hidden="true"><Icon name="music" size={18} /></span>
        <span className="d-np__text">
          <span className="d-np__state">{playing ? BARS : null}{state}</span>
          <span className="d-np__title">{track?.title ?? labels.idle}</span>
          {meta !== undefined ? <span className="d-np__meta">{meta}</span> : null}
        </span>
      </div>
      <div className="d-np__progress">
        {progress}
        <span className="d-np__times" aria-hidden="true"><span>{pos}</span><span>{dur}</span></span>
      </div>
      <div className="d-np__controls">
        {toggle}
        {skip}
        {stop}
        {volume !== undefined ? (
          <span className="d-np__volume"><Icon name="volume-2" size={13} />
            <span aria-hidden="true">{fill(labels.volume, { n: volume })}</span>
            <span className="d-sr-only">{labels.volumeName} {fill(labels.volume, { n: volume })}</span>
          </span>
        ) : null}
        <span className="d-np__spacer" />
        <button type="button" className="d-np__queue-toggle" aria-expanded={open} aria-controls={open ? listId : undefined}
          onClick={() => setOpen(!open)}>
          <Icon name="list-music" size={13} /><span>{fill(labels.queue, { n: upNext })}</span>
        </button>
      </div>
      {open ? (
        <ol id={listId} className="d-np__queue" aria-label={labels.queueList}>
          {queue.map((q, i) => (
            <li key={`${String(i)}:${q.title}`} className="d-np__item" data-current={q.current === true ? 'true' : undefined}
              aria-current={q.current === true ? 'true' : undefined}>
              <span className="d-np__n" aria-hidden="true">{i + 1}</span>
              <span className="d-np__item-title">{q.title}</span>
              <span className="d-np__by">{q.by}</span>
              <span className="d-np__dur">{q.durationS !== undefined ? playerTime(q.durationS) : ''}</span>
            </li>
          ))}
        </ol>
      ) : null}
      {live}
    </div>
  );
}
