import type { Meta, StoryObj } from '@storybook/react-vite';
import type { ReactNode } from 'react';
import { NowPlaying } from './NowPlaying.tsx';
import { npHarbour } from './np.fixture.tsx';
import { VoiceDock } from '../VoiceDock/VoiceDock.tsx';
import { DOCK_STATS_CONTRAST } from '../VoiceDock/dockStatsContrast.ts';

/** The card as a message in the conversation shows it: in the 30px avatar's column, the feed's width. */
function Feed({ children }: { children: ReactNode }) {
  return <div style={{ width: 760, padding: '8px 20px 8px 60px', background: 'var(--bg)' }}>{children}</div>;
}
/** The stage's body: 980px main pane, 14px in. */
function StageBody({ children }: { children: ReactNode }) {
  return <div style={{ width: 980, padding: 14, boxSizing: 'border-box', background: 'var(--bg)' }}>{children}</div>;
}

const meta = {
  title: 'Voice/NowPlaying', component: NowPlaying, globals: { theme: 'signal' }, args: npHarbour,
  // Ruling Y (scripts/prototype-contrast.ts): the dock row's state line and the current queue row's meta keep the prototype's colours.
  parameters: DOCK_STATS_CONTRAST,
  decorators: [(Story, ctx) => (ctx.args.variant === 'stage' ? <StageBody><Story /></StageBody> : <Feed><Story /></Feed>)],
} satisfies Meta<typeof NowPlaying>;
export default meta;
type Story = StoryObj<typeof meta>;

/** The card in #general (README §6.6): you are in harbour with control. */
export const Card: Story = {};
/** Up next opened: the current track tinted, then the queue with requester and duration. */
export const CardQueueOpen: Story = { args: { queueOpen: true } };
/** The dock's compact row, inside the voice dock as packages/web composes it (the sidebar's 244px). */
export const Compact: Story = {
  args: { variant: 'compact' },
  decorators: [Story => (
    <div style={{ width: 244, background: 'var(--bg-2)' }}>
      <VoiceDock phase="connected" where="harbour" muted={false} deafened={false} announcement="" onLeave={() => {}} music={<Story />}
        stats={{ rttMs: 24, upKbps: 38, downKbps: 71, quality: 'good' }}
        labels={{ region: 'voice controls', connecting: 'connecting', where: 'harbour · Midgard Crew', reconnecting: 'reconnecting', paused: 'paused',
          mute: 'mute', deafen: 'deafen', leave: 'leave', ping: '{ms} ms', pingSub: '<1 ms', pingNone: '— ms', rate: '↑ {up} ↓ {down} kb/s',
          stats: 'Ping {ms} milliseconds, {quality}. Sending {up}, receiving {down} kilobits per second.', quality: { good: 'good', poor: 'poor', bad: 'bad' } }} />
    </div>
  )],
};
/** The bar on top of the voice stage. */
export const Stage: Story = { args: { variant: 'stage' } };
/** Glenn idles in harbour after Stop: the card says so; the dock row and the stage bar are not drawn. */
export const Idle: Story = { args: { track: undefined, playing: false, positionS: 0, queue: [], meta: 'Glenn is idle in harbour. /play starts the next one.' } };
/** Not in harbour: the controls are aria-disabled and a press shows the toast "Join harbour to control playback". */
export const NoPermission: Story = { args: { canControl: false } };
