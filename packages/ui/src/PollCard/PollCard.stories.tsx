import type { Meta, StoryObj } from '@storybook/react-vite';
import { PollCard } from './PollCard.tsx';
const meta = { title: 'Conversation/PollCard', component: PollCard, globals: { theme: 'signal' }, args: {
  question: 'What should we raid on Friday?', closed: false, multi: false, meta: 'Open for 24 hours · pick one · 3 voters', canVote: true, canClose: false,
  options: [{ text: 'the bridge', pct: 67, mine: false, winner: false, voters: [{ name: 'Ada' }, { name: 'Kari' }] }, { text: 'the harbour', pct: 33, mine: false, winner: false, voters: [{ name: 'Björn' }] }],
  labels: { closed: 'CLOSED', close: 'Close poll' }, onVote: () => {}, onClose: () => {},
} } satisfies Meta<typeof PollCard>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Open: Story = {};
export const Voted: Story = { args: { options: [{ ...meta.args.options[0], mine: true }, meta.args.options[1]] } };
export const MultiChoice: Story = { args: { multi: true, meta: 'Open for 24 hours · pick any · 3 voters', options: meta.args.options.map((option, index) => ({ ...option, pct: 50, mine: true, voters: index === 1 ? [{ name: 'Ada' }, { name: 'Björn' }] : option.voters })) } };
export const Closed: Story = { args: { closed: true, canVote: false, meta: 'Final results · pick one · 3 voters', options: [{ ...meta.args.options[0], winner: true }, meta.args.options[1]] } };
export const Creator: Story = { args: { canClose: true } };
