import { useState } from 'react';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { PollDialog, type PollDraft } from './PollDialog.tsx';
const empty: PollDraft = { question: '', answers: ['', ''], duration: '24h', multi: false };
function Live({ draft = empty, onPost = vi.fn() }: { draft?: PollDraft; onPost?: () => void }) { const [value, onChange] = useState(draft); return <PollDialog open channelName="general" bot={{ name: 'Glenn' }} value={value} error={null} canPost onChange={onChange} onPost={onPost} onCancel={() => {}} />; }
describe('PollDialog', () => {
  it('keeps remove inert at two answers', async () => { render(<Live />); await userEvent.click(screen.getAllByRole('button', { name: /Remove answer/ })[0]); expect(screen.getAllByRole('textbox')).toHaveLength(3); });
  it('hides Add answer at ten', () => { render(<Live draft={{ ...empty, answers: Array(10).fill('') }} />); expect(screen.queryByRole('button', { name: 'Add answer' })).not.toBeInTheDocument(); });
  it('marks the counter danger past 300 without truncating input', () => { render(<Live draft={{ ...empty, question: 'x'.repeat(301) }} />); expect(screen.getByText('301 / 300')).toHaveAttribute('data-danger', 'true'); });
  it('Post calls onPost', async () => { const onPost = vi.fn(); render(<Live draft={{ ...empty, question: 'Raid?', answers: ['Bridge', 'Harbour'] }} onPost={onPost} />); await userEvent.click(screen.getByRole('button', { name: 'Post poll' })); expect(onPost).toHaveBeenCalledOnce(); });
});
