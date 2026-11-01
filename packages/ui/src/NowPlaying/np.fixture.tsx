// The prototype's player (README §6.6): Glenn plays "Sea shanties for rowing" (Ada, 3:34) in harbour at 0:37, volume 60,
// "Fjord ambience, 4 hours" (Björn, 4:12) up next. The props are the ones packages/web/src/screens/voice/music.tsx passes.
import { NowPlaying, type NowPlayingLabels, type NowPlayingProps } from './NowPlaying.tsx';

export const NP_LABELS: NowPlayingLabels = {
  region: 'Music player', playing: 'Now playing', paused: 'Paused', stopped: 'Stopped', idle: 'Nothing playing',
  play: 'Play', pause: 'Pause', skip: 'Skip', stop: 'Stop', position: 'Playback position', positionValue: '{pos} of {dur}',
  queue: 'Up next · {n}', upNext: '{n} up next', volume: '{n}%', volumeName: 'Volume', queueList: 'Queue',
};

const noop = () => {};
export const npHarbour: NowPlayingProps = {
  variant: 'card', track: { title: 'Sea shanties for rowing', by: 'Ada', durationS: 214 }, positionS: 37, playing: true, room: 'harbour',
  meta: 'YouTube · requested by Ada · harbour', volume: 60, bot: { name: 'Glenn', initials: 'GL' }, canControl: true,
  blocked: 'Join harbour to control playback',
  queue: [{ title: 'Sea shanties for rowing', by: 'Ada', durationS: 214, current: true }, { title: 'Fjord ambience, 4 hours', by: 'Björn', durationS: 252 }],
  onToggle: noop, onSkip: noop, onStop: noop, labels: NP_LABELS,
};

/** The dock's row and the stage's bar while you are in harbour with Glenn. */
export const harbourCompact = <NowPlaying {...npHarbour} variant="compact" />;
export const harbourStageBar = <NowPlaying {...npHarbour} variant="stage" />;
