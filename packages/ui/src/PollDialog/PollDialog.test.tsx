import { POLL_DIALOG_LABELS } from './PollDialog.fixture.tsx';
import { useState } from 'react';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { PollDialog, type PollDraft } from './PollDialog.tsx';
const empty: PollDraft = { question: '', answers: ['', ''], duration: '24h', multi: false };
function Live({ draft = empty, onPost = vi.fn() }: { draft?: PollDraft; onPost?: () => void }) { const [value, onChange] = useState(draft); return <PollDialog labels={POLL_DIALOG_LABELS} open channelName="general" bot={{ name: 'Glenn' }} value={value} error={null} canPost onChange={onChange} onPost={onPost} onCancel={() => {}} />; }
describe('PollDialog', () => {
  it('keeps remove inert at two answers', async () => { render(<Live />); await userEvent.click(screen.getAllByRole('button', { name: /Remove answer/ })[0]); expect(screen.getAllByRole('textbox')).toHaveLength(3); });
  it('hides Add answer at ten', () => { render(<Live draft={{ ...empty, answers: Array(10).fill('') }} />); expect(screen.queryByRole('button', { name: 'Add answer' })).not.toBeInTheDocument(); });
  it('marks the counter danger past 300 without truncating input', () => { render(<Live draft={{ ...empty, question: 'x'.repeat(301) }} />); expect(screen.getByText('301 / 300')).toHaveAttribute('data-danger', 'true'); });
  it('Post calls onPost', async () => { const onPost = vi.fn(); render(<Live draft={{ ...empty, question: 'Raid?', answers: ['Bridge', 'Harbour'] }} onPost={onPost} />); await userEvent.click(screen.getByRole('button', { name: 'Post poll' })); expect(onPost).toHaveBeenCalledOnce(); });
});

it('uses a radio group with roving arrow-key duration selection', async () => {
  render(<Live />); const selected = screen.getByRole('radio', { name: '24h' }); selected.focus();
  expect(screen.getByRole('radiogroup', { name: 'Open for' })).toBeInTheDocument();
  await userEvent.keyboard('{ArrowRight}'); expect(screen.getByRole('radio', { name: '3d' })).toHaveFocus();
  expect(screen.getByRole('radio', { name: '3d' })).toHaveAttribute('aria-checked', 'true');
  await userEvent.keyboard('{End}{ArrowRight}'); expect(screen.getByRole('radio', { name: '1h' })).toHaveFocus();
});
it.each([0, 2])('focuses the adjacent answer input after removing answer %s', async index => {
  render(<Live draft={{ ...empty, answers: ['A', 'B', 'C'] }} />);
  await userEvent.click(screen.getAllByRole('button', { name: /Remove answer/ })[index]);
  expect(screen.getByRole('textbox', { name: `Answer ${index === 0 ? 1 : 2}` })).toHaveFocus();
});
it('uses New poll when the channel is missing and shows exactly one Close control', () => {
  render(<PollDialog labels={POLL_DIALOG_LABELS} open channelName="" bot={{ name: 'Glenn' }} value={empty} error={null} canPost={false} onChange={() => {}} onPost={() => {}} onCancel={() => {}} />);
  expect(screen.getByRole('dialog', { name: 'New poll' })).toBeInTheDocument(); expect(screen.getAllByRole('button', { name: 'Close' })).toHaveLength(1);
});
it('uses whole graphemes in the bot tile', () => {
  render(<PollDialog labels={POLL_DIALOG_LABELS} open channelName="general" bot={{ name: '🧑‍🚀Ada' }} value={empty} error={null} canPost={false} onChange={() => {}} onPost={() => {}} onCancel={() => {}} />);
  expect(screen.getByTitle('🧑‍🚀Ada')).toHaveTextContent('🧑‍🚀A');
});

it('renders caller-owned labels', () => {
  render(<PollDialog labels={{ ...POLL_DIALOG_LABELS, title: 'Custom #{channel}', question: 'Prompt', post: 'Publish' }} open channelName="general" bot={{ name: 'Glenn' }} value={empty} error={null} canPost={false} onChange={() => {}} onPost={() => {}} onCancel={() => {}} />);
  expect(screen.getByRole('dialog', { name: 'Custom #general' })).toBeInTheDocument(); expect(screen.getByRole('textbox', { name: 'Prompt' })).toBeInTheDocument(); expect(screen.getByRole('button', { name: 'Publish' })).toBeInTheDocument();
});
