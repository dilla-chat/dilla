import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { BotCommandDialogs, type BotCommandDialogState, type BotCommandDialogValue } from './BotCommandDialogs.tsx';

const CHANNEL = '11'.repeat(16);
function announcementDialog() {
  const onSubmit = vi.fn<(value: BotCommandDialogValue) => void>(); const onCancel = vi.fn();
  const dialog: Extract<BotCommandDialogState, { kind: 'announcement' }> = { kind: 'announcement', communityId: '22'.repeat(16), channels: [{ id: CHANNEL, name: 'announcements' }], initial: null, prefill: 'Launch',
    mode: 'create', isCurrent: () => true, onSubmit, onCancel };
  return { dialog, onSubmit, onCancel };
}

describe('BotCommandDialogs', () => {
  it('rejects a one-off schedule in an elapsed minute before invoking the mutation callback', async () => {
    const user = userEvent.setup(); const { dialog, onSubmit } = announcementDialog();
    render(<BotCommandDialogs dialog={dialog} />);
    await user.clear(screen.getByLabelText('Date'));
    await user.type(screen.getByLabelText('Date'), '2020-01-01');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Choose a complete future minute in the selected time zone.');
    expect(onSubmit.mock.calls).toHaveLength(0);
  });

  it('closes a focused editor when its bound command host becomes stale', async () => {
    let current = true; const { dialog, onCancel } = announcementDialog(); dialog.isCurrent = () => current;
    const view = render(<BotCommandDialogs dialog={dialog} />);
    expect(await screen.findByRole('dialog', { name: 'Schedule an announcement' })).toBeInTheDocument();
    current = false;
    await vi.waitFor(() => expect(onCancel.mock.calls).toHaveLength(1));
    view.unmount();
  });
});
