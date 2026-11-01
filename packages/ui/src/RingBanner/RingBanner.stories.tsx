import type { Meta, StoryObj } from '@storybook/react-vite';
import { RingBanner } from './RingBanner.tsx';
import { expectVoiceTargets } from '../CallControls/CallControls.stories.tsx';

// Every string is L-COPY-05's, quoted and filled (en.ts gains them in task 9).
const meta = {
  title: 'Voice/RingBanner', component: RingBanner,
  args: {
    rings: [{ callId: 'c1', text: 'ada is calling', busy: false, name: 'ada' }],
    labels: { region: 'incoming calls', answer: 'answer', decline: 'decline' },
    onAnswer: () => {}, onDecline: () => {},
  },
  decorators: [Story => <div style={{ maxWidth: 960 }}><Story /></div>],
  play: ({ canvasElement }) => { expectVoiceTargets(canvasElement, 'RingBanner'); },
} satisfies Meta<typeof RingBanner>;
export default meta;
type Story = StoryObj<typeof meta>;

/** Normal, and quiet (a muted DM, R1): the same banner; only the page's sound and notification differ. */
export const Incoming: Story = {};
/** Busy: this device is in voice; answering leaves it. No sound, no notification. */
export const Busy: Story = { args: { rings: [{ callId: 'c1', text: 'ada is calling. Answering leaves harbour.', busy: true, name: 'ada' }] } };
/** Two rings at once (a group DM rings each other participant independently, R6). */
export const TwoRings: Story = {
  args: { rings: [{ callId: 'c1', text: 'ada is calling', busy: false, name: 'ada' }, { callId: 'c2', text: 'bo is calling', busy: false, name: 'bo' }] },
};
