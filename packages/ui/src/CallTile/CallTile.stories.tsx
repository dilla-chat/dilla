import { useEffect, useState } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { CallTile, type CallTileProps } from './CallTile.tsx';
import { tileMenu } from '../CallStage/stage.fixture.tsx';
import { expectVoiceTargets } from '../CallControls/CallControls.stories.tsx';
import { expectGlyphLine } from '../VoiceChannelRow/VoiceChannelRow.stories.tsx';

// Every string is L-COPY-05's, quoted (en.ts gains them in task 9).
const LABELS: CallTileProps['labels'] = {
  state: {
    speaking: 'speaking', muted: 'muted', deafened: 'deafened', notHeard: 'not heard yet', unverified: 'unverified stream',
    connecting: 'connecting', video: 'camera on', screen: 'sharing screen', web: 'web', bot: 'bot', noSound: 'no sound',
  },
  volume: 'volume for ada',
  volumeValue: '{n} %',
};
const VIEW: CallTileProps['view'] = {
  deviceId: 'd1', userId: 'u1', name: 'ada', state: 'ok', speaking: false, muted: false, deafened: false,
  video: false, screen: false, web: true, bot: false,
};

/** A 16:9 stream drawn on a canvas at story time (a stand-in camera); its tracks stop on unmount. */
function useDrawnStream(): MediaStream | null {
  const [stream, setStream] = useState<MediaStream | null>(null);
  useEffect(() => {
    const canvas = document.createElement('canvas');
    canvas.width = 320; canvas.height = 180;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;
    const gradient = ctx.createLinearGradient(0, 0, 320, 180);
    gradient.addColorStop(0, '#1A1E22'); gradient.addColorStop(1, '#363B41');
    ctx.fillStyle = gradient; ctx.fillRect(0, 0, 320, 180);
    ctx.fillStyle = '#A0A6A0'; ctx.beginPath(); ctx.arc(160, 80, 34, 0, Math.PI * 2); ctx.fill();
    ctx.fillRect(100, 124, 120, 56);
    const s = canvas.captureStream(5);
    setStream(s);
    return () => { s.getTracks().forEach(t => t.stop()); };
  }, []);
  return stream;
}

function WithCamera(props: CallTileProps) {
  const media = useDrawnStream();
  return <CallTile {...props} media={media} />;
}

const meta = {
  title: 'Voice/CallTile', component: CallTile,
  args: { view: VIEW, own: false, volume: 100, media: null, labels: LABELS },
  decorators: [(Story, ctx) => <ul style={{ width: (ctx.parameters.tileWidth as string | undefined) ?? '280px', boxSizing: 'border-box', margin: 0, padding: 0, listStyle: 'none',
    ...(ctx.parameters.tileHeight !== undefined ? { height: ctx.parameters.tileHeight as string, display: 'grid' } : {}) }}><Story /></ul>],
  // The volume range is a voice control: 40 px high, 44 px on a phone (flow 06).
  play: ({ canvasElement }) => { expectVoiceTargets(canvasElement, 'CallTile'); expectGlyphLine(canvasElement, 'CallTile'); },
} satisfies Meta<typeof CallTile>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Connected: Story = {};
export const Speaking: Story = { args: { view: { ...VIEW, speaking: true } } };
/** R25, Signal (task 8, the prototype's tile): speaking is the tile's inset 2px --ok edge and the avatar's 5px --ok ring, and there is no dot. */
export const SpeakingOnStage: Story = {
  args: { view: { ...VIEW, speaking: true }, onFocusTile: () => {} },
  play: ({ canvasElement }) => {
    const ring = canvasElement.querySelector<HTMLElement>('.d-call-tile__avatar-ring[data-speaking="true"]');
    const tile = canvasElement.querySelector<HTMLElement>('.d-call-tile[data-speaking="true"]');
    if (ring === null || tile === null) throw new Error('SpeakingOnStage: no ring');
    const edge = getComputedStyle(tile).boxShadow;
    if (!edge.includes('inset') || !edge.includes('2px') || !getComputedStyle(ring).boxShadow.includes('5px')) {
      throw new Error(`SpeakingOnStage: the tile edge is ${edge} and the ring ${getComputedStyle(ring).boxShadow}; want an inset 2px edge and a 5px ring`);
    }
    if (canvasElement.querySelector('.d-state-dot') !== null) throw new Error('SpeakingOnStage: a dot is drawn');
  },
};
/** The level is not 100: a quiet chip in the caption row says so. */
export const QuietVolume: Story = {
  args: { volume: 70, onFocusTile: () => {} },
  play: ({ canvasElement }) => {
    const chip = canvasElement.querySelector<HTMLElement>('.d-call-tile__caption .d-call-tile__chip');
    if (chip === null || chip.textContent !== '70 %') throw new Error('QuietVolume: no 70 % chip');
  },
};
export const MutedAndDeafened: Story = { args: { view: { ...VIEW, name: 'bo', muted: true, deafened: true }, labels: { ...LABELS, volume: 'volume for bo' } } };
export const Connecting: Story = { args: { view: { ...VIEW, state: 'connecting' } } };
export const NotHeard: Story = { args: { view: { ...VIEW, state: 'not-heard' } } };
/** An unverified stream: the tile says so and never attaches the stream, even when one is given. */
export const Unverified: Story = { args: { view: { ...VIEW, state: 'unverified', video: true } }, render: args => <WithCamera {...args} /> };
export const Own: Story = { args: { view: { ...VIEW, name: 'you', deviceId: 'd0', userId: 'u0' }, own: true } };
export const Camera: Story = { args: { view: { ...VIEW, video: true, speaking: true } }, render: args => <WithCamera {...args} /> };
/** #37: a remote screen that carries no sound says so with the no-sound glyph in its caption. */
export const ScreenWithoutSound: Story = {
  args: { view: { ...VIEW, screen: true }, source: 'screen', title: 'ada’s screen', silent: true, onFocusTile: () => {} },
  parameters: { tileHeight: '158px' },
  render: args => <WithCamera {...args} />,
  play: ({ canvasElement }) => {
    if (canvasElement.querySelector('[data-state="no-sound"]') === null) throw new Error('ScreenWithoutSound: no mark');
  },
};
/** #37: a remote screen with sound: no control on the tile; its own stream volume and mute are in its person menu (fix round 1). */
export const ScreenWithSound: Story = {
  args: { view: { ...VIEW, screen: true }, source: 'screen', title: 'ada’s screen', onFocusTile: () => {}, menu: tileMenu('ada', { screen: true }) },
  parameters: { tileHeight: '158px' },
  render: args => <WithCamera {...args} />,
};
export const SharingScreen: Story = { args: { view: { ...VIEW, screen: true } }, render: args => <WithCamera {...args} /> };
export const Bot: Story = { args: { view: { ...VIEW, name: 'relay', web: false, bot: true }, labels: { ...LABELS, volume: 'volume for relay' } } };
/** Product walk I3: the own tile's self-view, the camera mirrored. */
export const OwnSelfView: Story = { args: { view: { ...VIEW, name: 'you', deviceId: 'd0', userId: 'u0', video: true }, own: true, mirror: true }, render: args => <WithCamera {...args} /> };
/** Product walk M6: a volume above 100 %: the quiet chip in the caption says it. */
export const LouderVolume: Story = { args: { volume: 150, onFocusTile: () => {} } };

/** Every glyph and a long name in the 280 px tile: the name truncates, the glyphs stay on its line (R29). */
export const EveryGlyph: Story = {
  args: { view: { ...VIEW, name: 'augusta-ada-king-noel-countess-of-lovelace', muted: true, deafened: true, video: true, screen: true, speaking: true, state: 'unverified' } },
};
