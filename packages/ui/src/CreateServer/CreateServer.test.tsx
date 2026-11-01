import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { CreateServer, type CreateServerProps } from './CreateServer.tsx';
import meta, { Partial as PartialStory, PartialVoice } from './CreateServer.stories.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const labels: CreateServerProps['labels'] = meta.args.labels;

function props(over: Partial<CreateServerProps> = {}): CreateServerProps {
  return { open: true, name: '', onName: vi.fn(), error: null, busy: false, partial: null, labels,
    onSubmit: vi.fn(), onRetry: vi.fn(), onClose: vi.fn(), ...over };
}

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('CreateServer', () => {
  it('is a dialog named by its title with one labelled field, where focus starts', () => {
    render(<CreateServer {...props()} />);
    const dialog = screen.getByRole('dialog', { name: labels.title });
    expect(dialog).toHaveTextContent(labels.body);
    const field = screen.getByRole('textbox', { name: labels.name });
    expect(field).toHaveFocus();
    expect(field).toHaveAccessibleDescription(labels.hint);
    expect(screen.getAllByRole('textbox')).toHaveLength(1);
  });

  it('reports typing, and submits with Enter in the field and with the footer button', async () => {
    const user = userEvent.setup();
    const p = props({ name: 'Midgard' });
    render(<CreateServer {...p} />);
    await user.type(screen.getByRole('textbox', { name: labels.name }), 'x');
    expect(p.onName).toHaveBeenLastCalledWith('Midgardx');
    await user.keyboard('{Enter}');
    await user.click(screen.getByRole('button', { name: labels.submit }));
    expect(p.onSubmit).toHaveBeenCalledTimes(2);
    expect(p.onRetry).not.toHaveBeenCalled();
  });

  it('shows a refusal as the field’s alert and description', () => {
    render(<CreateServer {...props({ error: 'Give the server a name.' })} />);
    const field = screen.getByRole('textbox', { name: labels.name });
    expect(field).toHaveAttribute('aria-invalid', 'true');
    expect(field).toHaveAccessibleDescription(`${labels.hint} Give the server a name.`);
    expect(screen.getByRole('alert')).toHaveTextContent('Give the server a name.');
  });

  it('while busy says so on an aria-disabled submit and submits nothing more', async () => {
    const user = userEvent.setup();
    const p = props({ name: 'Midgard', busy: true });
    render(<CreateServer {...p} />);
    const submit = screen.getByRole('button', { name: labels.busy });
    expect(submit).toHaveAttribute('aria-disabled', 'true');
    await user.click(submit);
    await user.type(screen.getByRole('textbox', { name: labels.name }), '{Enter}');
    expect(p.onSubmit).not.toHaveBeenCalled();
  });

  it('after a partial create shows the notice in place of the field, and its submit retries', async () => {
    const user = userEvent.setup();
    const p = props({ partial: PartialStory.args.partial });
    render(<CreateServer {...p} />);
    expect(screen.queryByRole('textbox')).toBeNull();
    expect(screen.getByRole('alert')).toHaveTextContent(PartialStory.args.partial);
    await user.click(screen.getByRole('button', { name: labels.retry }));
    expect(p.onRetry).toHaveBeenCalledTimes(1);
    expect(p.onSubmit).not.toHaveBeenCalled();
  });

  it('a refusal moves focus back to the field, also after the submit button was pressed', async () => {
    const user = userEvent.setup();
    const p = props({ name: '   ' });
    const { rerender } = render(<CreateServer {...p} />);
    await user.click(screen.getByRole('button', { name: labels.submit }));
    expect(p.onSubmit).toHaveBeenCalledTimes(1);
    rerender(<CreateServer {...p} error="Give the server a name." />);
    expect(screen.getByRole('textbox', { name: labels.name })).toHaveFocus();
  });

  it('when the notice replaces the field, focus moves to the retry; while it runs the notice stays and nothing more is sent', async () => {
    const user = userEvent.setup();
    const p = props({ name: 'Midgard Crew' });
    const { rerender } = render(<CreateServer {...p} />);
    expect(screen.getByRole('textbox', { name: labels.name })).toHaveFocus();
    rerender(<CreateServer {...p} partial={PartialVoice.args.partial} />);
    expect(screen.getByRole('button', { name: labels.retry })).toHaveFocus();
    rerender(<CreateServer {...p} partial={PartialVoice.args.partial} busy />);
    expect(screen.getByRole('alert')).toHaveTextContent(PartialVoice.args.partial);
    await user.click(screen.getByRole('button', { name: labels.busy }));
    expect(p.onRetry).not.toHaveBeenCalled();
  });

  it('says it is busy in a status region that stands from the first render', () => {
    const { rerender } = render(<CreateServer {...props({ name: 'Midgard' })} />);
    const region = screen.getByRole('status');
    expect(region).toHaveTextContent('');
    rerender(<CreateServer {...props({ name: 'Midgard', busy: true })} />);
    expect(screen.getByRole('status')).toBe(region);
    expect(region).toHaveTextContent(labels.busy);
  });

  it('a new attempt with the same refusal or notice is a new alert', () => {
    const p = props({ name: 'Midgard', error: 'That name was refused. Try another.', attempt: 1 });
    const { rerender } = render(<CreateServer {...p} />);
    const first = screen.getByRole('alert');
    rerender(<CreateServer {...p} attempt={2} />);
    expect(screen.getByRole('alert')).not.toBe(first);
    expect(screen.getByRole('textbox', { name: labels.name })).toHaveFocus();
    rerender(<CreateServer {...p} error={null} partial={PartialVoice.args.partial} attempt={3} />);
    const notice = screen.getByRole('alert');
    rerender(<CreateServer {...p} error={null} partial={PartialVoice.args.partial} attempt={4} />);
    expect(screen.getByRole('alert')).not.toBe(notice);
  });

  it('Escape and the close button call onClose; a closed dialog shows nothing', async () => {
    const user = userEvent.setup();
    const p = props();
    const { rerender } = render(<CreateServer {...p} />);
    await user.keyboard('{Escape}');
    await user.click(screen.getByRole('button', { name: new RegExp(labels.close) }));
    expect(p.onClose).toHaveBeenCalledTimes(2);
    rerender(<CreateServer {...p} open={false} />);
    expect(screen.queryByText(labels.body)).toBeNull();
  });

  it.each(['signal', 'light', 'high-contrast'])('has no serious axe violations in %s, idle, refused and partial', async theme => {
    document.documentElement.dataset.theme = theme;
    for (const over of [{}, { error: 'Give the server a name.' }, { partial: PartialStory.args.partial }]) {
      const { container, unmount } = render(<div className="d-root"><CreateServer {...props(over)} /></div>);
      await expectNoAxeViolations(container);
      unmount();
    }
  });
});
