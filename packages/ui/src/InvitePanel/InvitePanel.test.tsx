import { describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { InvitePanel, type InvitePanelProps } from './InvitePanel.tsx';

const LABELS = { make: 'Make invite link', making: 'Making…', copy: 'Copy', link: 'Invite link', revoke: 'Revoke', list: 'Invites' };
function setup(over: Partial<InvitePanelProps> = {}) {
  const props: InvitePanelProps = {
    expiry: { label: 'Expires after', options: [{ value: '3600', label: '1 hour' }, { value: '0', label: 'never' }], value: '3600', onChange: vi.fn() },
    uses: { label: 'Uses', options: [{ value: '1', label: '1' }, { value: '0', label: '∞' }], value: '1', onChange: vi.fn() },
    made: null, making: false, onMake: vi.fn(), onCopy: vi.fn(), activeTitle: 'Active links — 1',
    rows: [{ id: 'a', code: 'midgard.gg/i/••••', uses: '3 / 10 uses', expires: 'expires in 5 days', revokeLabel: 'Revoke the link by Ada' }],
    onRevoke: vi.fn(), labels: LABELS, ...over,
  };
  render(<InvitePanel {...props} />);
  return props;
}

describe('InvitePanel (Signal task 10c)', () => {
  it('reports the choices and makes a link', async () => {
    const props = setup();
    await userEvent.click(within(screen.getByRole('radiogroup', { name: 'Expires after' })).getByRole('radio', { name: 'never' }));
    expect(props.expiry.onChange).toHaveBeenCalledWith('0');
    await userEvent.click(within(screen.getByRole('radiogroup', { name: 'Uses' })).getByRole('radio', { name: '∞' }));
    expect(props.uses.onChange).toHaveBeenCalledWith('0');
    await userEvent.click(screen.getByRole('button', { name: 'Make invite link' }));
    expect(props.onMake).toHaveBeenCalled();
  });
  it('does not make a second link while one is being made', async () => {
    const props = setup({ making: true });
    await userEvent.click(screen.getByRole('button', { name: 'Making…' }));
    expect(props.onMake).not.toHaveBeenCalled();
  });
  it('shows a made link once with Copy and its note, in place of the make button', async () => {
    const props = setup({ made: { url: 'https://midgard.gg/i/k7', note: 'It can be used 1 time and expires in 1 hour. The link is shown only once.' } });
    expect(screen.getByRole('textbox', { name: 'Invite link' })).toHaveValue('https://midgard.gg/i/k7');
    expect(screen.queryByRole('button', { name: 'Make invite link' })).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'Copy' }));
    expect(props.onCopy).toHaveBeenCalled();
    expect(screen.getByText(/shown only once/)).toBeInTheDocument();
  });
  it('lists the live links with Revoke named by the link, and only where revoke is allowed', async () => {
    const props = setup({ rows: [{ id: 'a', code: 'c', uses: '0 / 1 uses', expires: 'expires never', revokeLabel: 'Revoke the link by Ada' },
      { id: 'b', code: 'c', uses: '0 / ∞ uses', expires: 'expires never' }] });
    expect(screen.getByRole('heading', { name: 'Active links — 1' })).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: /^Revoke/ })).toHaveLength(1);
    await userEvent.click(screen.getByRole('button', { name: 'Revoke the link by Ada' }));
    expect(props.onRevoke).toHaveBeenCalledWith('a');
  });
  it('a refusal is an alert', () => {
    setup({ message: { text: 'You cannot make that link.', alert: true, n: 1 } });
    expect(screen.getByRole('alert')).toHaveTextContent('You cannot make that link.');
  });
});
