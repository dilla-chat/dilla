import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { CommandBar, CommandChips, CommandField, CommandList, CommandSuggestions, commandOptionId } from './CommandPicker.tsx';
import { Composer } from '../Composer/Composer.tsx';

const LABELS = { title: 'Commands', hint: '↑↓ move · tab complete · esc close', noMatch: 'No command matches /xyz', sendAsText: 'Send as text' };
const ITEMS = [
  { id: 'remind', icon: 'alarm-clock' as const, name: 'remind', args: '<when> <text>', description: 'A private one-time reminder', source: 'Glenn' },
  { id: 'reminders list', icon: 'bell' as const, name: 'reminders list', args: '', description: 'Open your reminders inbox', source: 'Glenn' },
];

describe('CommandList', () => {
  it('is a listbox pointing at its active row, rows named "/name, description"', () => {
    render(<CommandList id="cmds" items={ITEMS} activeId="reminders list" showHint labels={LABELS} query="/re" onActive={() => {}} onPick={() => {}} onSendAsText={() => {}} />);
    const list = screen.getByRole('listbox', { name: 'Commands' });
    expect(list).toHaveAttribute('aria-activedescendant', commandOptionId('cmds', 'reminders list'));
    expect(screen.getByRole('option', { name: '/remind, A private one-time reminder' })).toHaveAttribute('aria-selected', 'false');
    expect(screen.getByRole('option', { name: '/reminders list, Open your reminders inbox' })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByText('↑↓ move · tab complete · esc close')).toBeInTheDocument();
  });

  it('picks on pointer down without taking focus, and follows the pointer', () => {
    const onPick = vi.fn(); const onActive = vi.fn();
    render(<CommandList id="cmds" items={ITEMS} activeId="remind" showHint={false} labels={LABELS} query="/re" onActive={onActive} onPick={onPick} onSendAsText={() => {}} />);
    const row = screen.getByRole('option', { name: /reminders list/ });
    fireEvent.mouseEnter(row);
    expect(onActive).toHaveBeenCalledWith('reminders list');
    expect(fireEvent.mouseDown(row)).toBe(false);
    expect(onPick).toHaveBeenCalledWith('reminders list');
    expect(screen.queryByText('↑↓ move · tab complete · esc close')).toBeNull();
  });

  it('offers "Send as text" when nothing matches', () => {
    const onSendAsText = vi.fn();
    render(<CommandList id="cmds" items={[]} activeId={null} showHint labels={LABELS} query="/xyz" onActive={() => {}} onPick={() => {}} onSendAsText={onSendAsText} />);
    expect(screen.getByText('No command matches /xyz')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Send as text' }));
    expect(onSendAsText).toHaveBeenCalledTimes(1);
  });
});

describe('CommandField', () => {
  it('marks the active chip and an invalid one, and names the input by its label', () => {
    const { container } = render(<CommandChips icon="alarm-clock" name="remind">
      <CommandField label="when" optional={false} value="" placeholder="in 1 hour" active invalid onChange={() => {}} onFocus={() => {}} onKeyDown={() => {}} />
      <CommandField label="note" optional value="" placeholder="optional" active={false} invalid={false} onChange={() => {}} onFocus={() => {}} onKeyDown={() => {}} />
    </CommandChips>);
    const when = screen.getByRole('combobox', { name: 'when' });
    expect(when).toHaveAttribute('aria-invalid', 'true');
    expect(when.closest('.d-cmd-field')).toHaveAttribute('data-active', 'true');
    expect(screen.getByRole('combobox', { name: 'note' })).not.toHaveAttribute('aria-invalid');
    expect(container.querySelector('.d-cmd-chips__name')).toHaveTextContent('/remind');
    expect(screen.getByText('note?')).toBeInTheDocument();
  });

  it('sizes the input to max(value, placeholder, 4) characters plus 16px', () => {
    render(<CommandField label="when" optional={false} value="in 2h" placeholder="in 1 hour" active={false} invalid={false} onChange={() => {}} onFocus={() => {}} onKeyDown={() => {}} />);
    expect(screen.getByRole('combobox', { name: 'when' }).style.width).toBe('calc(9ch + 1rem)');
  });

  it('a ghost chip takes no text and says how to run', () => {
    const onChange = vi.fn();
    render(<CommandField label="" optional={false} value="" placeholder="press ↵ to run" active invalid={false} ghost onChange={onChange} onFocus={() => {}} onKeyDown={() => {}} />);
    const ghost = screen.getByRole('textbox', { name: 'press ↵ to run' });
    expect(ghost).toHaveAttribute('readonly');
    fireEvent.change(ghost, { target: { value: 'x' } });
    expect(onChange).not.toHaveBeenCalled();
  });

  it('a free-text chip is a one-row textarea that grows a row per line and takes its width from the longest line', () => {
    const onChange = vi.fn();
    const { rerender, container } = render(<CommandField label="text" optional={false} value="one" placeholder="packs the rope" active invalid={false} multiline onChange={onChange} onFocus={() => {}} onKeyDown={() => {}} />);
    const text = screen.getByRole('textbox', { name: 'text' });
    expect(text.tagName).toBe('TEXTAREA');
    expect(text).toHaveAttribute('rows', '1');
    expect(container.querySelector('.d-cmd-field')).not.toHaveAttribute('data-multiline');
    fireEvent.change(text, { target: { value: 'one\ntwo' } });
    expect(onChange).toHaveBeenCalledWith('one\ntwo');
    rerender(<CommandField label="text" optional={false} value={'one\na much longer second line'} placeholder="packs the rope" active invalid={false} multiline onChange={onChange} onFocus={() => {}} onKeyDown={() => {}} />);
    expect(text).toHaveAttribute('rows', '2');
    expect(text.style.width).toBe('calc(25ch + 1rem)');
    expect(container.querySelector('.d-cmd-field')).toHaveAttribute('data-multiline', 'true');
  });

  it('points at its suggestions while they show', () => {
    render(<CommandField label="user" optional={false} value="" placeholder="" active invalid={false} listId="sugs" activeOptionId="sugs-ada" onChange={() => {}} onFocus={() => {}} onKeyDown={() => {}} />);
    const user = screen.getByRole('combobox', { name: 'user' });
    expect(user).toHaveAttribute('aria-expanded', 'true');
    expect(user).toHaveAttribute('aria-controls', 'sugs');
    expect(user).toHaveAttribute('aria-activedescendant', 'sugs-ada');
  });
});

describe('CommandSuggestions', () => {
  it('picks on pointer down and keeps the chip focused', () => {
    const onPick = vi.fn();
    render(<><input aria-label="user" />
      <CommandSuggestions id="sugs" title="Members" activeId="ada" onActive={() => {}} onPick={onPick}
        items={[{ id: 'ada', label: 'Ada', sub: '@ada', avatar: { name: 'Ada' } }, { id: 'in 1 hour', label: 'in 1 hour', sub: 'Fri 30 Oct, 19:00 CET', icon: 'alarm-clock' }]} /></>);
    const input = screen.getByRole('textbox', { name: 'user' });
    input.focus();
    expect(screen.getByRole('listbox', { name: 'Members' })).toBeInTheDocument();
    const ada = screen.getByRole('option', { name: /Ada/ });
    expect(ada).toHaveAttribute('aria-selected', 'true');
    expect(fireEvent.mouseDown(ada)).toBe(false);
    expect(onPick).toHaveBeenCalledWith('ada');
    expect(document.activeElement).toBe(input);
  });
});

describe('CommandBar', () => {
  it('announces an error and keeps the hint out of the reading order', () => {
    const { rerender } = render(<CommandBar icon="alarm-clock" text="A private one-time reminder" tone="info" hint="tab next field · ↵ run · esc leave" />);
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.getByText('tab next field · ↵ run · esc leave')).toHaveAttribute('aria-hidden', 'true');
    rerender(<CommandBar icon="triangle-alert" text="when is required" tone="danger" />);
    expect(screen.getByRole('alert')).toHaveTextContent('when is required');
  });
});

describe('Composer input slot', () => {
  it('draws the chips in place of the textarea and hands send to the owner', () => {
    const onSend = vi.fn();
    render(<Composer label="message #general" placeholder="Message #general" maxLength={4000} value="" onChange={() => {}} onSend={onSend} sendLabel="send" counterLabel={n => String(n)}
      input={<CommandChips icon="pause" name="pause"><CommandField label="" optional={false} value="" placeholder="press ↵ to run" active invalid={false} ghost onChange={() => {}} onFocus={() => {}} onKeyDown={() => {}} /></CommandChips>} />);
    expect(screen.queryByRole('textbox', { name: /message/ })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: /send/ }));
    expect(onSend).toHaveBeenCalledWith('');
  });
});
