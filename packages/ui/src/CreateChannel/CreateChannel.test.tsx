import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { CreateChannel, type CreateChannelProps } from './CreateChannel.tsx';
import meta from './CreateChannel.stories.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const labels: CreateChannelProps['labels'] = meta.args.labels;
const HINT = 'Shown as #ship-talk. Up to 100 bytes.';

function props(over: Partial<CreateChannelProps> = {}): CreateChannelProps {
  return { open: true, kind: 'text', onKind: vi.fn(), name: '', onName: vi.fn(), hint: HINT, error: null, busy: false, labels,
    onSubmit: vi.fn(), onClose: vi.fn(), ...over };
}

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('CreateChannel', () => {
  it('is a dialog with a kind radio group (text, voice, announcement) and a name field, where focus starts', () => {
    render(<CreateChannel {...props({ kinds: ['text', 'voice', 'announcement'] })} />);
    const dialog = screen.getByRole('dialog', { name: labels.title });
    const group = within(dialog).getByRole('radiogroup', { name: labels.kind });
    expect(within(group).getAllByRole('radio').map(r => [r.textContent, r.getAttribute('aria-checked')])).toEqual([
      [labels.text, 'true'], [labels.voice, 'false'], [labels.announcement, 'false'],
    ]);
    const field = screen.getByRole('textbox', { name: labels.name });
    expect(field).toHaveFocus();
    expect(field).toHaveAccessibleDescription(HINT);
  });

  it('names the kind group once: one label text, none visible in a second copy', () => {
    render(<CreateChannel {...props({ kinds: ['text', 'voice', 'announcement'] })} />);
    expect(screen.getAllByText(labels.kind)).toHaveLength(1);
    expect(document.querySelector('.d-create-channel__label')).toBeNull();
  });

  it('selection follows the arrows in the kind group; typing and Enter report to the caller', async () => {
    const user = userEvent.setup();
    const p = props({ name: 'ship' });
    render(<CreateChannel {...p} />);
    await user.click(screen.getByRole('radio', { name: labels.text }));
    await user.keyboard('{ArrowRight}');
    expect(p.onKind).toHaveBeenLastCalledWith('voice');
    await user.type(screen.getByRole('textbox', { name: labels.name }), '-talk{Enter}');
    expect(p.onName).toHaveBeenCalled();
    expect(p.onSubmit).toHaveBeenCalledTimes(1);
  });

  it('selecting announcement reports it, and the arrows reach it from voice', async () => {
    const user = userEvent.setup();
    const p = props({ kinds: ['text', 'voice', 'announcement'] });
    render(<CreateChannel {...p} />);
    await user.click(screen.getByRole('radio', { name: labels.announcement }));
    expect(p.onKind).toHaveBeenLastCalledWith('announcement');
    const q = props({ kind: 'voice', kinds: ['text', 'voice', 'announcement'] });
    render(<CreateChannel {...q} />);
    await user.click(screen.getAllByRole('radio', { name: labels.voice })[1]);
    await user.keyboard('{ArrowRight}');
    expect(q.onKind).toHaveBeenLastCalledWith('announcement');
  });

  it('shows text and voice only by default, and the kinds the caller lists otherwise', () => {
    const { unmount } = render(<CreateChannel {...props()} />);
    expect(screen.getAllByRole('radio').map(r => r.textContent)).toEqual([labels.text, labels.voice]);
    unmount();
    render(<CreateChannel {...props({ kinds: ['text', 'announcement'] })} />);
    expect(screen.getAllByRole('radio').map(r => r.textContent)).toEqual([labels.text, labels.announcement]);
  });

  it('shows a refusal as the field’s alert; while busy the submit says so and submits nothing', async () => {
    const user = userEvent.setup();
    const p = props({ name: 'ship', busy: true, error: 'You cannot add channels to this server.' });
    render(<CreateChannel {...p} />);
    expect(screen.getByRole('alert')).toHaveTextContent('You cannot add channels to this server.');
    const submit = screen.getByRole('button', { name: labels.busy });
    expect(submit).toHaveAttribute('aria-disabled', 'true');
    await user.click(submit);
    expect(p.onSubmit).not.toHaveBeenCalled();
  });

  it('a refusal moves focus back to the field, also after the submit button was pressed', async () => {
    const user = userEvent.setup();
    const p = props({ name: 'loot' });
    const { rerender } = render(<CreateChannel {...p} />);
    await user.click(screen.getByRole('button', { name: labels.submit }));
    rerender(<CreateChannel {...p} error="You cannot add channels to this server." />);
    expect(screen.getByRole('textbox', { name: labels.name })).toHaveFocus();
  });

  it('says it is busy in a status region; a new attempt with the same refusal is a new alert', () => {
    const p = props({ name: 'loot', error: 'That name was refused. Try another.', attempt: 1 });
    const { rerender } = render(<CreateChannel {...p} />);
    expect(screen.getByRole('status')).toHaveTextContent('');
    const first = screen.getByRole('alert');
    rerender(<CreateChannel {...p} attempt={2} />);
    expect(screen.getByRole('alert')).not.toBe(first);
    expect(screen.getByRole('textbox', { name: labels.name })).toHaveFocus();
    rerender(<CreateChannel {...p} error={null} busy />);
    expect(screen.getByRole('status')).toHaveTextContent(labels.busy);
  });

  it('Escape calls onClose', async () => {
    const user = userEvent.setup();
    const p = props();
    render(<CreateChannel {...p} />);
    await user.keyboard('{Escape}');
    expect(p.onClose).toHaveBeenCalledTimes(1);
  });

  it.each(['signal', 'light', 'high-contrast'])('has no serious axe violations in %s, text and voice', async theme => {
    document.documentElement.dataset.theme = theme;
    for (const over of [{}, { kind: 'announcement' as const, kinds: ['text', 'voice', 'announcement'] as const }, { kind: 'voice' as const, error: 'Give the channel a name.' }]) {
      const { container, unmount } = render(<div className="d-root"><CreateChannel {...props(over)} /></div>);
      await expectNoAxeViolations(container);
      unmount();
    }
  });
});

// useId values differ between two renders; the rest of the markup must not.
const norm = (html: string) => html.replace(/\b(id|for|form|aria-describedby|aria-labelledby)="[^"]*"/g, '$1="_"');
describe('CreateChannel children slot', () => {
  it('renders children inside the form, after the name field, and adds nothing without them', () => {
    const { unmount } = render(<CreateChannel {...props()} />);
    const bare = norm(document.querySelector('form')!.innerHTML);
    unmount();
    render(<CreateChannel {...props()}><label>private <input type="checkbox" /></label></CreateChannel>);
    const form = document.querySelector('form')!;
    const slot = screen.getByRole('checkbox', { name: 'private' });
    expect(form.contains(slot)).toBe(true);
    const field = screen.getByRole('textbox', { name: labels.name });
    expect(field.compareDocumentPosition(slot) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(norm(form.innerHTML.replace(/<label>private <input type="checkbox"><\/label>/, ''))).toBe(bare);
  });
});
