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
