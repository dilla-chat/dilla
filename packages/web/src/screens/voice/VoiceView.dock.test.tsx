import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { VoiceContext } from './VoiceView.tsx';
import { Dock } from './VoiceView.tsx';
import type { MusicPlayerView } from './music.tsx';

// Beta.5 (Jonas, "Always show the dock"): the dock and its Now playing row stay while the channel's own stage is on screen.
const labels = new Proxy({}, { get: () => 'x' }) as MusicPlayerView['labels'];
const music: MusicPlayerView = {
  track: { title: 'Sea shanties', artist: 'Artist', durationS: 214 } as never, positionS: 37, playing: true, canControl: true, queue: [],
  onToggle: () => undefined, onSkip: () => undefined, onStop: () => undefined, labels,
};
const ctx = (withMusic: boolean) => ({
  call: { channelId: 'c1', session: {}, state: { phase: 'in', muted: false, deafened: false, camera: false, sharing: false }, announcement: '', startAudio: () => undefined,
    pauseFocusPending: false, markPauseFocused: () => undefined, leave: () => undefined },
  slice: null, music: withMusic ? music : null,
}) as unknown as VoiceContext;

describe('the dock on the voice channel stage', () => {
  it('draws the call card and the compact Now playing, with no on-stage hiding', () => {
    const { container } = render(<Dock v={ctx(true)} where="general" server="harbour" stageShown />);
    expect(container.querySelector('.dw-voice-dock')?.hasAttribute('data-on-stage')).toBe(false);
    expect(container.querySelector('.d-voice-dock__row')).not.toBeNull();
    expect(container.querySelector('.d-np--compact')).not.toBeNull();
  });
});
