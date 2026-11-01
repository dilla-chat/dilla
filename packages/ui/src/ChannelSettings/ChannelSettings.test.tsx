import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ChannelSettings, type ChannelSettingsProps } from './ChannelSettings.tsx';
import meta from './ChannelSettings.stories.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const labels: ChannelSettingsProps['labels'] = meta.args.labels;

function props(over: Partial<ChannelSettingsProps> = {}): ChannelSettingsProps {
  return {
    open: true, kind: 'text', name: 'ship-talk', onName: vi.fn(), topic: '', onTopic: vi.fn(), parent: '', parents: meta.args.parents,
    onParent: vi.fn(), error: null, busy: false, labels, onSubmit: vi.fn(), onClose: vi.fn(), ...over,
  };
}

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('ChannelSettings', () => {
  it('is a dialog for a text channel: name, topic and category, with focus in the name', () => {
    render(<ChannelSettings {...props()} />);
    expect(screen.getByRole('dialog', { name: labels.title })).toBeInTheDocument();
    const name = screen.getByRole('textbox', { name: labels.name });
    expect(name).toHaveFocus();
    expect(name).toHaveAccessibleDescription(labels.nameHint);
    expect(screen.getByRole('textbox', { name: labels.topic })).toHaveAccessibleDescription(labels.topicHint);
    expect(screen.getByRole('combobox', { name: labels.category })).toBeInTheDocument();
  });

  it('a voice channel has no topic, but a category', () => {
    render(<ChannelSettings {...props({ kind: 'voice' })} />);
    expect(screen.queryByRole('textbox', { name: labels.topic })).toBeNull();
    expect(screen.getByRole('combobox', { name: labels.category })).toBeInTheDocument();
  });

  it('a category has neither a topic nor a category select', () => {
    render(<ChannelSettings {...props({ kind: 'category' })} />);
    expect(screen.queryByRole('textbox', { name: labels.topic })).toBeNull();
    expect(screen.queryByRole('combobox', { name: labels.category })).toBeNull();
    expect(screen.getByRole('textbox', { name: labels.name })).toBeInTheDocument();
  });

  it('typing, choosing a category and Enter report to the caller', async () => {
    const user = userEvent.setup();
    const p = props();
    render(<ChannelSettings {...p} />);
    await user.type(screen.getByRole('textbox', { name: labels.topic }), 'x');
    expect(p.onTopic).toHaveBeenCalledWith('x');
    await user.selectOptions(screen.getByRole('combobox', { name: labels.category }), 'c1');
    expect(p.onParent).toHaveBeenCalledWith('c1');
    await user.type(screen.getByRole('textbox', { name: labels.name }), '{Enter}');
    expect(p.onSubmit).toHaveBeenCalledTimes(1);
  });

  it('shows a refusal as an alert and moves focus back to the name; busy submits nothing', async () => {
    const user = userEvent.setup();
    const p = props({ error: 'You cannot edit this channel.' });
    const { rerender } = render(<ChannelSettings {...p} />);
    expect(screen.getByRole('alert')).toHaveTextContent('You cannot edit this channel.');
    expect(screen.getByRole('textbox', { name: labels.name })).toHaveFocus();
    const busy = { ...p, error: null, busy: true };
    rerender(<ChannelSettings {...busy} />);
    expect(screen.getByRole('status')).toHaveTextContent(labels.busy);
    const submit = screen.getByRole('button', { name: labels.busy });
    expect(submit).toHaveAttribute('aria-disabled', 'true');
    await user.click(submit);
    expect(p.onSubmit).not.toHaveBeenCalled();
  });

  it('Escape calls onClose', async () => {
    const user = userEvent.setup();
    const p = props();
    render(<ChannelSettings {...p} />);
    await user.keyboard('{Escape}');
    expect(p.onClose).toHaveBeenCalledTimes(1);
  });

  it.each(['signal', 'light', 'high-contrast'])('has no serious axe violations in %s, for each kind', async theme => {
    document.documentElement.dataset.theme = theme;
    for (const over of [{}, { kind: 'voice' as const, error: 'Give the channel a name.' }, { kind: 'category' as const }]) {
      const { container, unmount } = render(<div className="d-root"><ChannelSettings {...props(over)} /></div>);
      await expectNoAxeViolations(container);
      unmount();
    }
  });
});

// useId values differ between two renders; the rest of the markup must not.
const norm = (html: string) => html.replace(/\b(id|for|form|aria-describedby|aria-labelledby)="[^"]*"/g, '$1="_"');
describe('ChannelSettings children slot', () => {
  it('renders children inside the form, after the topic and category fields, and adds nothing without them', () => {
    const { unmount } = render(<ChannelSettings {...props()} />);
    const bare = norm(document.querySelector('form')!.innerHTML);
    unmount();
    render(<ChannelSettings {...props()}><label>private <input type="checkbox" /></label></ChannelSettings>);
    const form = document.querySelector('form')!;
    const slot = screen.getByRole('checkbox', { name: 'private' });
    expect(form.contains(slot)).toBe(true);
    for (const before of [screen.getByRole('textbox', { name: labels.topic }), screen.getByRole('combobox', { name: labels.category })]) {
      expect(before.compareDocumentPosition(slot) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
    expect(norm(form.innerHTML.replace(/<label>private <input type="checkbox"><\/label>/, ''))).toBe(bare);
  });
});
