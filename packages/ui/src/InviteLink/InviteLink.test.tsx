import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { InviteLink, type InviteLinkProps } from './InviteLink.tsx';
import meta, { Copied } from './InviteLink.stories.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const labels: InviteLinkProps['labels'] = meta.args.labels;
const URL = Copied.args.url;

function props(over: Partial<InviteLinkProps> = {}): InviteLinkProps {
  return { open: true, state: 'ready', url: URL, status: '', limits: meta.args.limits, error: null, labels,
    onCopy: vi.fn(), onRetry: vi.fn(), onClose: vi.fn(), ...over };
}

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('InviteLink', () => {
  it('while the link is made says so, with no field and an empty status region', () => {
    render(<InviteLink {...props({ state: 'creating', url: '' })} />);
    expect(screen.getByRole('dialog', { name: labels.title })).toHaveTextContent(labels.creating);
    expect(screen.queryByRole('textbox')).toBeNull();
    expect(screen.getByRole('status')).toHaveTextContent('');
  });

  it('shows the link in a read-only field that selects all of it on focus, with its limits and a copy button', async () => {
    const user = userEvent.setup();
    const p = props();
    render(<InviteLink {...p} />);
    const field = screen.getByRole<HTMLInputElement>('textbox', { name: labels.link });
    expect(field).toHaveValue(URL);
    expect(field).toHaveAttribute('readonly');
    expect(field).toHaveAccessibleDescription(meta.args.limits);
    fireEvent.focus(field);
    expect([field.selectionStart, field.selectionEnd]).toEqual([0, URL.length]);
    await user.click(screen.getByRole('button', { name: labels.copy }));
    expect(p.onCopy).toHaveBeenCalledTimes(1);
  });

  it('says what the clipboard did in its status region', () => {
    const { rerender } = render(<InviteLink {...props()} />);
    const region = screen.getByRole('status');
    rerender(<InviteLink {...props({ status: Copied.args.status })} />);
    expect(screen.getByRole('status')).toBe(region);
    expect(region).toHaveTextContent('Invite link copied.');
  });

  it('a refused link is an alert with a retry', async () => {
    const user = userEvent.setup();
    const p = props({ state: 'failed', url: '', error: 'You cannot invite people to this server.' });
    render(<InviteLink {...p} />);
    expect(screen.getByRole('alert')).toHaveTextContent('You cannot invite people to this server.');
    expect(screen.queryByRole('textbox')).toBeNull();
    await user.click(screen.getByRole('button', { name: labels.retry }));
    expect(p.onRetry).toHaveBeenCalledTimes(1);
  });

  it('a new attempt with the same refusal is a new alert', () => {
    const p = props({ state: 'failed', url: '', error: 'You cannot invite people to this server.', attempt: 1 });
    const { rerender } = render(<InviteLink {...p} />);
    const first = screen.getByRole('alert');
    rerender(<InviteLink {...p} attempt={2} />);
    expect(screen.getByRole('alert')).not.toBe(first);
  });

  it.each(['signal', 'light', 'high-contrast'])('has no serious axe violations in %s, creating, ready and failed', async theme => {
    document.documentElement.dataset.theme = theme;
    for (const over of [{ state: 'creating' as const, url: '' }, { status: 'Invite link copied.' }, { state: 'failed' as const, error: 'x' }]) {
      const { container, unmount } = render(<div className="d-root"><InviteLink {...props(over)} /></div>);
      await expectNoAxeViolations(container);
      unmount();
    }
  });

  it('puts an extra action in the footer beside close, and none when there is no extra', async () => {
    const user = userEvent.setup();
    const onExtra = vi.fn();
    const { rerender } = render(<InviteLink {...props({ extra: <button type="button" onClick={onExtra}>more options</button> })} />);
    const footer = screen.getByRole('dialog', { name: labels.title }).querySelector('.d-dialog__footer') as HTMLElement;
    expect(footer).toContainElement(screen.getByRole('button', { name: 'more options' }));
    await user.click(screen.getByRole('button', { name: 'more options' }));
    expect(onExtra).toHaveBeenCalledTimes(1);
    rerender(<InviteLink {...props()} />);
    expect(screen.queryByRole('button', { name: 'more options' })).toBeNull();
  });
});
