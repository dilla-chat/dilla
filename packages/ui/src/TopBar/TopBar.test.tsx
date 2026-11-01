import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { TopBar, type TopBarProps } from './TopBar.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const labels = { reminders: 'Reminders', settings: 'Open settings', voice: 'open the call in harbour' };
const base = (over: Partial<TopBarProps> = {}): TopBarProps => ({
  searchLabel: 'Search or jump to…', searchShortcut: '⌘K', onSearch: vi.fn(), remindersDue: 0, onReminders: vi.fn(), onSettings: vi.fn(), labels, ...over,
});

describe('TopBar', () => {
  it('draws the D tile and DILLA, with the tile and the caret hidden from assistive tech', () => {
    const { container } = render(<TopBar {...base()} />);
    expect(screen.getByText('DILLA')).toBeInTheDocument();
    const tile = container.querySelector('.d-brand__tile');
    expect(tile).toHaveTextContent('D');
    expect(tile).toHaveAttribute('aria-hidden', 'true');
    expect(container.querySelector('.d-brand__caret')).toHaveAttribute('aria-hidden', 'true');
  });
  it('the search trigger is a button named by searchLabel that calls onSearch, with the shortcut drawn beside it', async () => {
    const onSearch = vi.fn();
    render(<TopBar {...base({ onSearch, searchShortcut: 'Ctrl K' })} />);
    const search = screen.getByRole('button', { name: 'Search or jump to…' });
    expect(search).toHaveTextContent('Ctrl K');
    expect(search).toHaveAttribute('aria-keyshortcuts', 'Control+K Meta+K');
    await userEvent.click(search);
    expect(onSearch).toHaveBeenCalledTimes(1);
  });
  it('has no voice chip without voice', () => {
    const { container } = render(<TopBar {...base()} />);
    expect(container.querySelector('.d-topbar__voice')).toBeNull();
    expect(screen.queryByText(/ ms$/)).toBeNull();
  });
  it('the voice chip reads "harbour · 24 ms", is described by its label and opens the stage', async () => {
    const onOpen = vi.fn();
    render(<TopBar {...base({ voice: { room: 'harbour', pingMs: 24, onOpen } })} />);
    const chip = screen.getByRole('button', { name: 'harbour · 24 ms' });
    expect(chip).toHaveAccessibleDescription('open the call in harbour');
    await userEvent.click(chip);
    expect(onOpen).toHaveBeenCalledTimes(1);
  });
  it('the voice chip leaves the ping out until there is a reading', () => {
    render(<TopBar {...base({ voice: { room: 'harbour', pingMs: null, onOpen: vi.fn() } })} />);
    expect(screen.getByRole('button', { name: 'harbour' })).toBeInTheDocument();
  });
  it('shows the due badge "3" only when remindersDue is 3, and the count is in the bell\'s description', () => {
    const { container, rerender } = render(<TopBar {...base({ remindersDue: 0 })} />);
    expect(container.querySelector('.d-topbar__badge')).toBeNull();
    rerender(<TopBar {...base({ remindersDue: 3, labels: { ...labels, remindersDue: n => `${String(n)} due` } })} />);
    const badge = container.querySelector('.d-topbar__badge');
    expect(badge).toHaveTextContent('3');
    expect(badge).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getByRole('button', { name: 'Reminders' })).toHaveAccessibleDescription('3 due');
  });
  it('the bell and the gear are 28px icon buttons with accessible names that call their handlers', async () => {
    const onReminders = vi.fn();
    const onSettings = vi.fn();
    render(<TopBar {...base({ onReminders, onSettings })} />);
    const bell = screen.getByRole('button', { name: 'Reminders' });
    const gear = screen.getByRole('button', { name: 'Open settings' });
    expect(bell).toHaveAttribute('data-size', '28');
    expect(gear).toHaveAttribute('data-size', '28');
    expect(bell).toHaveAttribute('aria-haspopup', 'dialog');
    await userEvent.click(bell);
    await userEvent.click(gear);
    expect(onReminders).toHaveBeenCalledTimes(1);
    expect(onSettings).toHaveBeenCalledTimes(1);
  });
  it('is the page banner landmark with no axe violations', async () => {
    const { container } = render(<TopBar {...base({ remindersDue: 2, voice: { room: 'harbour', pingMs: 24, onOpen: vi.fn() } })} />);
    expect(screen.getByRole('banner')).toBe(container.querySelector('.d-topbar'));
    await expectNoAxeViolations(container);
  });
});
