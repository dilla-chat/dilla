import { useState } from 'react';
import { expect, userEvent } from 'storybook/test';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { PollCard } from './PollCard.tsx';
import { MessageRow } from '../MessageRow/MessageRow.tsx';
import { MessageToolbar } from '../MessageToolbar/MessageToolbar.tsx';
import { Composer } from '../Composer/Composer.tsx';
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


function PointerFocusPoll() {
  const [choice, setChoice] = useState<number | null>(null);
  const [closed, setClosed] = useState(false);
  return <div style={{ maxWidth: 720 }}><Composer label="Message" placeholder="Message" maxLength={4000} value="" onChange={() => {}} onSend={() => {}} sendLabel="send" counterLabel={remaining => `${remaining} left`} />
    <MessageRow author="Glenn" time="21:02" body="Mira asked:" state="ok" tag="bot"
      toolbar={<MessageToolbar tabbable={false} label="message actions" moreLabel="more actions" items={[{ id: 'reply', label: 'reply', onAction: () => {} }]} />}
      card={<PollCard {...meta.args} closed={closed} canClose canVote={!closed}
        options={meta.args.options.map((option, index) => ({ ...option, mine: index === choice, winner: closed && index === 0 }))}
        onVote={setChoice} onClose={() => setClosed(true)} />} />
  </div>;
}
export const PointerFocus: Story = {
  render: () => <PointerFocusPoll />,
  play: async ({ canvasElement }) => {
    const composer = canvasElement.querySelector<HTMLTextAreaElement>('textarea')!;
    const answer = canvasElement.querySelector<HTMLButtonElement>('.d-poll-card__option')!;
    const close = canvasElement.querySelector<HTMLButtonElement>('.d-poll-card__meta button')!;
    const row = canvasElement.querySelector<HTMLElement>('.d-message-row')!;
    const toolbar = canvasElement.querySelector<HTMLElement>('.d-message-row__toolbar')!;
    composer.focus();
    await userEvent.click(answer);
    await expect(answer).toHaveAttribute('aria-pressed', 'true');
    await expect(composer).toHaveFocus();
    if (row.contains(document.activeElement) || getComputedStyle(toolbar).visibility !== 'hidden') throw new Error('A pointer vote must keep focus outside the row and leave its toolbar hidden.');
    await userEvent.click(close);
    await expect(canvasElement.querySelector('.d-poll-card')).toHaveAttribute('data-closed', 'true');
    await expect(composer).toHaveFocus();
    if (getComputedStyle(toolbar).visibility !== 'hidden') throw new Error('A pointer close must leave the message toolbar hidden.');
  },
};
