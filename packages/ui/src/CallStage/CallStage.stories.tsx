import { useState } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { CallStage, type CallStageProps } from './CallStage.tsx';
import { CallControls, type CallControlsProps } from '../CallControls/CallControls.tsx';
import type { CallTileProps, VoiceParticipantView } from '../CallTile/CallTile.tsx';
import { nextFrame } from '../CallControls/CallControls.stories.tsx';
import { harbourStageBar } from '../NowPlaying/np.fixture.tsx';
import { HarbourPreview, HarbourStage, harbourTiles, personTile, STAGE_LABELS, STAGE_STATE, stageControls, tileMenu, WithPictures } from './stage.fixture.tsx';

// Every string is the shipped copy, quoted (packages/web/src/strings: voice.*, stage.*, callStage.*).
const CONTROL_LABELS: CallControlsProps['labels'] = {
  region: 'call controls', mute: 'mute', deafen: 'deafen', camera: 'camera', share: 'share screen', leave: 'leave',
};
const noop = () => {};
const dmControls = (more: Partial<CallControlsProps> = {}) => (
  <CallControls muted={false} deafened={false} camera={false} sharing={false} canCamera canShare busy={false} labels={CONTROL_LABELS}
    onMute={noop} onDeafen={noop} onCamera={noop} onShare={noop} onLeave={noop} {...more} />
);
function tile(name: string, more: Partial<VoiceParticipantView> = {}, own = false): CallTileProps {
  return {
    view: { deviceId: `d-${name}`, userId: `u-${name}`, name, state: 'ok', speaking: false, muted: false, deafened: false,
      video: false, screen: false, web: false, bot: false, ...more },
    own, volume: 100, media: null, labels: { state: STAGE_STATE, volume: `volume for ${name}`, volumeValue: '{n} %' },
  };
}
const PAUSED = 'ada’s identity changed, so this call is paused for you: nothing you say is sent and nothing is played. Compare safety numbers in Privacy to continue, or wait until ada leaves.';

/** The stage as the main pane holds it at the prototype's 1280×800 (the 300px rail and sidebar and the 44px top bar taken off): 980 × 756. */
const desk = (Story: () => React.JSX.Element) => (
  <div style={{ width: 980, height: 756, display: 'grid', gridTemplateRows: 'minmax(0, 1fr)', background: 'var(--bg)', margin: -16 }}><Story /></div>
);
/** The phone's voice pane at 390 wide (the shell's data-layout="phone"), between its header and its tab bar. */
const phone = (Story: () => React.JSX.Element) => (
  <div data-layout="phone" style={{ width: 390, height: 700, display: 'grid', gridTemplateRows: 'minmax(0, 1fr)', background: 'var(--bg)', margin: -16 }}><Story /></div>
);

const meta = {
  title: 'Voice/CallStage', component: CallStage,
  args: { title: 'voice harbour', tiles: [tile('ada', { speaking: true }), tile('bo', { muted: true }), tile('you', {}, true)], code: null,
    notice: null, controls: dmControls(), outgoing: null, labels: STAGE_LABELS },
} satisfies Meta<typeof CallStage>;
export default meta;
type Story = StoryObj<typeof meta>;

/** Ruling C: every stage tile is 16:9 and never stretched (height ≤ width × 9/16 + 1), inside the tiles' area. */
function expectSixteenNine(root: HTMLElement, story: string): void {
  for (const li of Array.from(root.querySelectorAll<HTMLElement>('li.d-call-stage__tile[data-stage]:not([data-focused="true"])'))) {
    const r = li.getBoundingClientRect();
    if (r.height > (r.width * 9) / 16 + 1 || Math.abs(r.width / r.height - 16 / 9) > 0.03) throw new Error(`${story}: a tile is ${String(r.width)} × ${String(r.height)}, not 16:9`);
  }
}
const settle = async () => { await nextFrame(); await nextFrame(); };

// ---- Signal §5.6: the joined stage (the web's Stage: header, bar, the call bar's words, Grid) ----

/** Two tiles (the prototype's stove: Liv and Mira, you speaking): min(46%, 560px) each, centred. */
export const Grid2: Story = {
  decorators: [desk],
  render: () => <HarbourStage name="stove" topic="2 in voice" stats="24 ms · ↑ 38 ↓ 71 kb/s · hearing 1 of 1"
    tiles={[personTile('Liv', { stat: '18 ms' }), personTile('Mira', { own: true, title: 'Mira (you)', stat: 'opus 48k', speaking: true })]} />,
  play: async ({ canvasElement }) => { await settle(); expectSixteenNine(canvasElement, 'Grid2'); },
};
export const Grid3: Story = {
  decorators: [desk],
  render: () => <HarbourStage tiles={[personTile('Ada'), personTile('Björn', { video: true }), personTile('Mira', { own: true, title: 'Mira (you)', stat: 'opus 48k' })]} />,
  play: async ({ canvasElement }) => { await settle(); expectSixteenNine(canvasElement, 'Grid3'); },
};
export const Grid5: Story = {
  decorators: [desk],
  render: () => <HarbourStage tiles={[personTile('Ada'), personTile('Björn', { video: true }), personTile('Kari', { video: true }), personTile('Ole'),
    personTile('Mira', { own: true, title: 'Mira (you)', stat: 'opus 48k', muted: true })]} />,
  play: async ({ canvasElement }) => { await settle(); expectSixteenNine(canvasElement, 'Grid5'); },
};
/** The prototype's harbour after Grid: two shares and six people, min(31.5%, 340px) each, the last row centred. */
export const GridHarbour: Story = {
  decorators: [desk],
  render: () => <HarbourStage tiles={harbourTiles()} focus={null} nowPlaying={harbourStageBar} />,
  play: async ({ canvasElement }) => { await settle(); expectSixteenNine(canvasElement, 'GridHarbour'); },
};
/** A speaker: the 2px --ok edge, the avatar's 5px ring and scale(1.06), the caption's meter. */
export const SpeakingTile: Story = {
  decorators: [desk],
  render: () => <HarbourStage name="stove" topic="2 in voice" stats="24 ms · ↑ 38 ↓ 71 kb/s · hearing 1 of 1"
    tiles={[personTile('Liv', { speaking: true, stat: '18 ms' }), personTile('Mira', { own: true, title: 'Mira (you)', stat: 'opus 48k' })]} />,
};
/** A share focused (the prototype's harbour as you join): 16:9 at the row's height, Grid and full screen, the 188px filmstrip. */
export const FocusShare: Story = {
  decorators: [desk],
  render: () => <HarbourStage tiles={harbourTiles('Björn')} focus="d-Ada:screen" nowPlaying={harbourStageBar} />,
  play: async ({ canvasElement }) => { await settle(); expectSixteenNine(canvasElement, 'FocusShare'); },
};
/** Review 1: a focused share whose owner speaks: the 0.55 --ok edge and the caption's meter (the prototype's focusShare). */
export const FocusShareSpeaking: Story = {
  decorators: [desk],
  render: () => <HarbourStage tiles={harbourTiles('Ada').map(t => (t.source === 'screen' && t.view.name === 'Ada' ? { ...t, view: { ...t.view, speaking: true } } : t))} focus="d-Ada:screen" nowPlaying={harbourStageBar} />,
};
/** A person focused (Ole): the 120px avatar. */
export const FocusPerson: Story = {
  decorators: [desk],
  render: () => <HarbourStage tiles={harbourTiles()} focus="d-Ole:person" nowPlaying={harbourStageBar} />,
  play: async ({ canvasElement }) => { await settle(); expectSixteenNine(canvasElement, 'FocusPerson'); },
};
/** Not joined: the overlapping 48px faces, the room, who is there and the green Join voice. */
export const NotJoinedPreview: Story = { decorators: [desk], render: () => <HarbourPreview /> };
/** The phone (390): one 16:9 tile per row, a focused share over the 150px filmstrip, the call bar's icons centred. */
export const Phone: Story = {
  decorators: [phone],
  render: () => <HarbourStage header={false} tiles={harbourTiles('Björn')} focus="d-Ada:screen" nowPlaying={harbourStageBar} />,
};
export const PhoneGrid: Story = {
  decorators: [phone],
  render: () => <HarbourStage header={false} tiles={harbourTiles()} focus={null} nowPlaying={harbourStageBar} />,
  play: async ({ canvasElement }) => { await settle(); expectSixteenNine(canvasElement, 'PhoneGrid'); },
};

// ---- States the prototype does not draw (the web's notices, the DM call box) ----

function Notice(props: Partial<CallStageProps>) {
  return (
    <WithPictures tiles={harbourTiles().slice(2, 5)}>
      {tiles => <CallStage title="voice harbour" tiles={tiles} focusedId={null} onFocus={noop} code={null} notice={null}
        outgoing={null} bar={{ name: 'harbour', stats: '24 ms · ↑ 38 ↓ 71 kb/s · hearing 2 of 2' }} controls={stageControls()} labels={STAGE_LABELS} {...props} />}
    </WithPictures>
  );
}
/** A DM call box's call code (a channel's is in its voice row's menu, fix round 1): the disclosure in the head, opened. */
export const CodeOpen: Story = {
  args: { title: 'ada', tiles: [tile('ada'), tile('you', {}, true)], code: '284719306451873920465128390715' },
  play: ({ canvasElement }) => { canvasElement.querySelector('summary')?.click(); },
};
/** RV-3: paused on this device. The banner and its action, no media in any tile, camera and share aria-disabled. */
export const Paused: Story = {
  decorators: [desk],
  render: () => <Notice notice={{ kind: 'paused', text: PAUSED, action: { label: 'compare safety numbers', onAction: noop }, focusOnce: true }}
    controls={stageControls({ canCamera: false, canShare: false, paused: true })} />,
};
/** Blocked at join: the notice takes focus once; the preview holds no Join voice until the cause is gone. */
export const BlockedJoin: Story = {
  decorators: [desk],
  render: () => <CallStage title="voice harbour" tiles={[]} code={null} controls={null} outgoing={null} labels={STAGE_LABELS}
    notice={{ kind: 'blocked', text: 'A device in this voice failed the identity check. Joining is paused until it is removed.', action: null, focusOnce: true }}
    preview={{ name: 'harbour', text: 'Ada is here', join: null, people: [{ id: 'a', name: 'Ada', speaking: false }] }} />,
};
/** A transient notice (`voice.micDenied`): listen-only, the tiles and their video stay. */
export const MicDenied: Story = {
  decorators: [desk],
  render: () => <Notice notice={{ kind: 'info', text: 'The microphone is blocked. You can listen; allow the microphone in the browser to talk.', action: null, focusOnce: false }} />,
};
export const Unverified: Story = {
  decorators: [desk],
  render: () => <HarbourStage tiles={[personTile('Ada', { state: 'unverified' }), personTile('Björn', { state: 'not-heard' }), personTile('Mira', { own: true, title: 'Mira (you)', stat: 'opus 48k' })]} />,
};
/** Joining: the call bar's controls stay aria-disabled while the tiles connect. */
export const Joining: Story = {
  decorators: [desk],
  render: () => <HarbourStage tiles={[personTile('Ada', { state: 'connecting' }), personTile('Mira', { own: true, title: 'Mira (you)', stat: 'opus 48k', state: 'connecting' })]}
    controls={stageControls({ busy: true })} />,
};
/** A person at 70 %: the quiet chip in the caption (the range is in the tile's person menu, fix round 1). */
export const VolumeChip: Story = {
  decorators: [desk],
  render: () => <HarbourStage tiles={[{ ...personTile('Ada'), volume: 70 }, personTile('Mira', { own: true, title: 'Mira (you)', stat: 'opus 48k' })]} />,
};
/** #37: remote screens: one without sound (the mark); a screen's own volume and mute are in its person menu (fix round 1). */
export const ScreensSound: Story = {
  decorators: [desk],
  render: () => <HarbourStage tiles={[{ ...harbourTiles()[0], silent: true, menu: tileMenu('Ada', { silent: true }) }, harbourTiles()[1],
    personTile('Mira', { own: true, title: 'Mira (you)', stat: 'opus 48k' })]} />,
};
/** A DM call ringing others (the compact box: no stage mode, the icon toolbar). */
export const DmRinging: Story = {
  args: { title: 'ada', outgoing: { text: 'ringing ada' }, tiles: [tile('you', {}, true)], code: null },
};
export const DmDeclined: Story = { args: { title: 'ada', outgoing: { text: 'ada declined.' }, tiles: [tile('you', {}, true)], code: null } };
/** The DM call box with a pressed mute (the icon toolbar keeps its labels and aria-pressed). */
export const DmTogglesPressed: Story = { args: { controls: dmControls({ muted: true, deafened: true, camera: true, sharing: true }) } };

/** The grid's Grid button clears the focus (a live story: click a tile, then Grid). */
export const FocusLive: Story = {
  decorators: [desk],
  render: function Render() {
    const [f, setF] = useState<string | null>('d-Ole:person');
    return (
      <WithPictures tiles={harbourTiles()}>
        {tiles => <CallStage title="voice harbour" tiles={tiles} focusedId={f} onFocus={setF} code={null} notice={null} outgoing={null}
          bar={{ name: 'harbour', stats: '24 ms · ↑ 38 ↓ 71 kb/s · hearing 5 of 5' }} controls={stageControls()} labels={STAGE_LABELS} />}
      </WithPictures>
    );
  },
  play: async ({ canvasElement }) => {
    await settle();
    const grid = canvasElement.querySelector<HTMLButtonElement>('.d-call-tile__grid');
    if (grid === null) throw new Error('FocusLive: no Grid button on the focused tile');
    grid.click();
    await settle();
    if (canvasElement.querySelector('.d-call-stage[data-focused-stage]') !== null) throw new Error('FocusLive: Grid left the stage focused');
  },
};
