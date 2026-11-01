import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ConfirmDialog, type ConfirmDialogProps } from './ConfirmDialog.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

function props(over: Partial<ConfirmDialogProps> = {}): ConfirmDialogProps {
  return {
    open: true, title: 'Ban Astrid?', body: 'Astrid cannot join again until you unban them.', confirmLabel: 'ban', busyLabel: 'Banning…',
    cancelLabel: 'cancel', tone: 'danger', busy: false, error: null, onConfirm: vi.fn(), onClose: vi.fn(), ...over,
  };
}

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('ConfirmDialog', () => {
  it('is a dialog with the title and body, and nothing while closed', () => {
    const { rerender } = render(<ConfirmDialog {...props({ open: false })} />);
    expect(screen.queryByText('Ban Astrid?')).toBeNull();
    rerender(<ConfirmDialog {...props()} />);
    const dialog = screen.getByRole('dialog', { name: 'Ban Astrid?' });
    expect(dialog).toHaveTextContent('Astrid cannot join again until you unban them.');
  });

  it('puts focus on Cancel, never on the destructive button, when there is no field', () => {
    render(<ConfirmDialog {...props()} />);
    expect(screen.getByRole('button', { name: 'cancel' })).toHaveFocus();
    expect(screen.getByRole('button', { name: 'ban' })).not.toHaveFocus();
  });

  it('puts focus on the first field in children when there is one', () => {
    render(<ConfirmDialog {...props()}><input aria-label="reason" /><input aria-label="second" /></ConfirmDialog>);
    expect(screen.getByRole('textbox', { name: 'reason' })).toHaveFocus();
  });

  it('finds a select as the first field too', () => {
    render(<ConfirmDialog {...props()}><select aria-label="who"><option>a</option></select></ConfirmDialog>);
    expect(screen.getByRole('combobox', { name: 'who' })).toHaveFocus();
  });

  it('skips disabled and read-only fields when it looks for the first field', () => {
    render(<ConfirmDialog {...props()}><input aria-label="off" disabled /><input aria-label="ro" readOnly /><input aria-label="real" /></ConfirmDialog>);
    expect(screen.getByRole('textbox', { name: 'real' })).toHaveFocus();
  });

  it('with only disabled fields, focus goes to Cancel', () => {
    render(<ConfirmDialog {...props()}><input aria-label="off" disabled /></ConfirmDialog>);
    expect(screen.getByRole('button', { name: 'cancel' })).toHaveFocus();
  });

  it('the dialog is described by the body', () => {
    render(<ConfirmDialog {...props()} />);
    expect(screen.getByRole('dialog', { name: 'Ban Astrid?' })).toHaveAccessibleDescription('Astrid cannot join again until you unban them.');
  });

  it('when a busy run ends with an error, focus moves to Cancel; without an error it stays where it was', () => {
    const p = props();
    const { rerender } = render(<ConfirmDialog {...p}><input aria-label="reason" /></ConfirmDialog>);
    expect(screen.getByRole('textbox', { name: 'reason' })).toHaveFocus();
    rerender(<ConfirmDialog {...p} busy><input aria-label="reason" /></ConfirmDialog>);
    rerender(<ConfirmDialog {...p} busy={false} error="You cannot ban this member."><input aria-label="reason" /></ConfirmDialog>);
    expect(screen.getByRole('button', { name: 'cancel' })).toHaveFocus();
    // A busy run that ends without an error (the caller closes the dialog) moves nothing.
    screen.getByRole('textbox', { name: 'reason' }).focus();
    rerender(<ConfirmDialog {...p} busy error={null}><input aria-label="reason" /></ConfirmDialog>);
    rerender(<ConfirmDialog {...p} busy={false} error={null}><input aria-label="reason" /></ConfirmDialog>);
    expect(screen.getByRole('textbox', { name: 'reason' })).toHaveFocus();
  });

  it('an error set together with busy=false focuses Cancel; the same error shown again does not take focus back', () => {
    const p = props();
    const field = <input aria-label="reason" />;
    const { rerender } = render(<ConfirmDialog {...p}>{field}</ConfirmDialog>);
    rerender(<ConfirmDialog {...p} busy>{field}</ConfirmDialog>);
    rerender(<ConfirmDialog {...p} busy={false} error="You cannot ban this member.">{field}</ConfirmDialog>);
    expect(screen.getByRole('button', { name: 'cancel' })).toHaveFocus();
    screen.getByRole('textbox', { name: 'reason' }).focus();
    rerender(<ConfirmDialog {...p} busy={false} error="You cannot ban this member." attempt={2}>{field}</ConfirmDialog>);
    expect(screen.getByRole('textbox', { name: 'reason' })).toHaveFocus();
  });

  it('busy ends with no error, then a later error does not take focus', () => {
    const p = props();
    const field = <input aria-label="reason" />;
    const { rerender } = render(<ConfirmDialog {...p}>{field}</ConfirmDialog>);
    rerender(<ConfirmDialog {...p} busy>{field}</ConfirmDialog>);
    rerender(<ConfirmDialog {...p} busy={false}>{field}</ConfirmDialog>);
    rerender(<ConfirmDialog {...p} busy={false} error="Something unrelated.">{field}</ConfirmDialog>);
    expect(screen.getByRole('textbox', { name: 'reason' })).toHaveFocus();
  });

  it('confirm calls onConfirm; cancel and Escape call onClose', async () => {
    const user = userEvent.setup();
    const p = props();
    render(<ConfirmDialog {...p} />);
    await user.click(screen.getByRole('button', { name: 'ban' }));
    expect(p.onConfirm).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole('button', { name: 'cancel' }));
    await user.keyboard('{Escape}');
    expect(p.onClose).toHaveBeenCalledTimes(2);
  });

  it('confirm is disabled while canConfirm is false', async () => {
    const user = userEvent.setup();
    const p = props({ canConfirm: false });
    render(<ConfirmDialog {...p} />);
    const confirm = screen.getByRole('button', { name: 'ban' });
    expect(confirm).toBeDisabled();
    await user.click(confirm);
    expect(p.onConfirm).not.toHaveBeenCalled();
  });

  it('confirm is enabled when canConfirm is true or left out', () => {
    const { rerender } = render(<ConfirmDialog {...props()} />);
    expect(screen.getByRole('button', { name: 'ban' })).toBeEnabled();
    rerender(<ConfirmDialog {...props({ canConfirm: true })} />);
    expect(screen.getByRole('button', { name: 'ban' })).toBeEnabled();
  });

  it('while busy the confirm says so and is disabled', async () => {
    const user = userEvent.setup();
    const p = props({ busy: true });
    render(<ConfirmDialog {...p} />);
    const confirm = screen.getByRole('button', { name: 'Banning…' });
    expect(confirm).toBeDisabled();
    await user.click(confirm);
    expect(p.onConfirm).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: 'ban' })).toBeNull();
  });

  it('shows the error as an alert; a new attempt with the same text is a new alert', () => {
    const p = props({ error: 'You cannot ban this member.', attempt: 1 });
    const { rerender } = render(<ConfirmDialog {...p} />);
    const first = screen.getByRole('alert');
    expect(first).toHaveTextContent('You cannot ban this member.');
    rerender(<ConfirmDialog {...p} attempt={2} />);
    expect(screen.getByRole('alert')).not.toBe(first);
    rerender(<ConfirmDialog {...p} error={null} attempt={2} />);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('the same attempt keeps the same alert node', () => {
    const p = props({ error: 'You cannot ban this member.', attempt: 1 });
    const { rerender } = render(<ConfirmDialog {...p} />);
    const first = screen.getByRole('alert');
    rerender(<ConfirmDialog {...p} />);
    expect(screen.getByRole('alert')).toBe(first);
  });

  it.each(['signal', 'light', 'high-contrast'])('has no serious axe violations in %s, danger and accent', async theme => {
    document.documentElement.dataset.theme = theme;
    for (const over of [{}, { tone: 'accent' as const, error: 'Try again.' }]) {
      const { container, unmount } = render(<div className="d-root"><ConfirmDialog {...props(over)} /></div>);
      await expectNoAxeViolations(container);
      unmount();
    }
  });
});
