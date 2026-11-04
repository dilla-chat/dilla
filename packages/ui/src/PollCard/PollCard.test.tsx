import { useState } from 'react';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { PollCard, type PollCardProps } from './PollCard.tsx';
const props: PollCardProps = { question: 'Raid?', closed: false, multi: false, meta: '3 voters', canVote: true, canClose: true,
  labels: { closed: 'CLOSED', close: 'Close poll' }, onVote: vi.fn(), onClose: vi.fn(), options: [
    { text: 'Bridge', pct: Math.round(1 / 3 * 100), mine: true, winner: true, voters: Array.from({ length: 8 }, (_, i) => ({ name: `Voter ${i}` })) },
    { text: 'Harbour', pct: Math.round(2 / 3 * 100), mine: false, winner: false, voters: [] }] };
describe('PollCard', () => {
  it('shows rounded percentages and caps voter avatars at five', () => { render(<PollCard {...props} />); expect(screen.getByText('33%')).toBeInTheDocument(); expect(screen.getByText('67%')).toBeInTheDocument(); expect(screen.getAllByRole('img')).toHaveLength(5); });
  it('marks a winner only when closed and its percentage is positive', () => { const view = render(<PollCard {...props} />); expect(screen.getByText('Bridge').closest('button')).toHaveAttribute('data-winner', 'false'); view.rerender(<PollCard {...props} closed />); expect(screen.getByText('Bridge').closest('button')).toHaveAttribute('data-winner', 'true'); view.rerender(<PollCard {...props} closed options={[{ ...props.options[0], pct: 0 }]} />); expect(screen.getByText('Bridge').closest('button')).toHaveAttribute('data-winner', 'false'); });
  it('votes on a row and never calls onVote when closed', async () => { const onVote = vi.fn(); const view = render(<PollCard {...props} onVote={onVote} />); await userEvent.click(screen.getByText('Harbour')); expect(onVote).toHaveBeenCalledWith(1); view.rerender(<PollCard {...props} onVote={onVote} closed />); await userEvent.click(screen.getByText('Bridge')); expect(onVote).toHaveBeenCalledTimes(1); });
});

it('keeps vote focus while busy and blocks aria-disabled actions', async () => {
  const onVote = vi.fn(); const view = render(<PollCard {...props} onVote={onVote} />);
  const answer = screen.getByRole('button', { name: /Harbour/ }); answer.focus();
  view.rerender(<PollCard {...props} onVote={onVote} busy />);
  expect(answer).toHaveFocus(); expect(answer).not.toBeDisabled(); expect(answer).toHaveAttribute('aria-disabled', 'true');
  await userEvent.click(answer); expect(onVote).not.toHaveBeenCalled();
});
it('moves keyboard close focus to the heading after closing', async () => {
  const onClose = vi.fn(); const view = render(<PollCard {...props} onClose={onClose} />);
  screen.getByRole('button', { name: 'Close poll' }).focus();
  await userEvent.keyboard('{Enter}');
  view.rerender(<PollCard {...props} onClose={onClose} busy />);
  expect(screen.getByRole('button', { name: 'Close poll' })).not.toBeDisabled();
  screen.getByRole('button', { name: 'Close poll' }).blur(); // Native removal can drop focus to body before the layout effect.
  view.rerender(<PollCard {...props} closed />); expect(screen.getByRole('heading', { name: 'Raid?' })).toHaveFocus();
});
it('uses whole graphemes for voter initials', () => {
  render(<PollCard {...props} options={[{ ...props.options[0], voters: [{ name: '🧑‍🚀Ada' }] }]} />);
  expect(screen.getByRole('img', { name: '🧑‍🚀Ada' })).toHaveTextContent('🧑‍🚀A');
});
it('focuses the heading when a remote close disables the focused answer', () => {
  const view = render(<PollCard {...props} />); const answer = screen.getByRole('button', { name: /Harbour/ });
  answer.focus(); answer.blur(); view.rerender(<PollCard {...props} closed />);
  expect(screen.getByRole('heading', { name: 'Raid?' })).toHaveFocus();
});


function InteractivePoll() {
  const [choice, setChoice] = useState<number | null>(null);
  const [closed, setClosed] = useState(false);
  return <><label>Message<input /></label><PollCard {...props} closed={closed} onVote={setChoice} onClose={() => setClosed(true)} />
    <output>{choice === null ? 'No choice' : `Selected ${choice}`}</output></>;
}
it('mouse votes and Close preserve the outside composer focus while performing their actions', async () => {
  render(<InteractivePoll />);
  const user = userEvent.setup();
  const composer = screen.getByRole('textbox', { name: 'Message' });
  composer.focus();
  await user.click(screen.getByRole('button', { name: /Harbour/ }));
  expect(screen.getByText('Selected 1')).toBeInTheDocument();
  expect(composer).toHaveFocus();
  await user.click(screen.getByRole('button', { name: 'Close poll' }));
  expect(screen.getByText('CLOSED')).toBeInTheDocument();
  expect(composer).toHaveFocus();
});
it('mouse Close preserves body focus when no field was focused', async () => {
  render(<InteractivePoll />);
  // A prior keyboard visit must not leave a stale inside-card focus marker.
  const answer = screen.getByRole('button', { name: /Harbour/ });
  answer.focus(); answer.blur();
  expect(document.body).toHaveFocus();
  await userEvent.click(screen.getByRole('button', { name: 'Close poll' }));
  expect(screen.getByText('CLOSED')).toBeInTheDocument();
  expect(document.body).toHaveFocus();
});
it.each(['{Enter}', ' '])('keyboard %s votes from the focused answer and Close hands focus to the heading', async key => {
  render(<InteractivePoll />);
  const user = userEvent.setup();
  const answer = screen.getByRole('button', { name: /Harbour/ });
  answer.focus();
  await user.keyboard(key);
  expect(screen.getByText('Selected 1')).toBeInTheDocument();
  expect(answer).toHaveFocus();
  const close = screen.getByRole('button', { name: 'Close poll' });
  close.focus();
  await user.keyboard(key);
  expect(screen.getByText('CLOSED')).toBeInTheDocument();
  expect(screen.getByRole('heading', { name: 'Raid?' })).toHaveFocus();
});
