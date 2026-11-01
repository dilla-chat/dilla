import { render, screen, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { ReactionBar, type ReactionBarProps } from './ReactionBar.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const ITEMS: ReactionBarProps['items'] = [
  { emoji: '👍', count: 3, mine: true, name: 'thumbs up, 3' },
  { emoji: '🦀', count: 1, mine: false, name: 'crab, 1' },
];
function bar(over: Partial<ReactionBarProps> = {}) {
  const onToggle = vi.fn();
  const onAdd = vi.fn();
  const r = render(<ReactionBar label="reactions" tabbable items={ITEMS} onToggle={onToggle} addLabel="add a reaction" onAdd={onAdd} {...over} />);
  return { ...r, onToggle, onAdd };
}

describe('ReactionBar', () => {
  it('is a named group of pressed-state chips named by their names, then the add button', () => {
    const { container } = bar();
    expect(screen.getByRole('group', { name: 'reactions' })).toHaveClass('d-reaction-bar');
    const thumbs = screen.getByRole('button', { name: 'thumbs up, 3' });
    const crab = screen.getByRole('button', { name: 'crab, 1' });
    expect(thumbs).toHaveAttribute('aria-pressed', 'true');
    expect(crab).toHaveAttribute('aria-pressed', 'false');
    expect(thumbs).toHaveAttribute('data-emoji', '👍');
    expect(thumbs.textContent).toBe('👍3');
    expect(screen.getAllByRole('button').map((b) => b.getAttribute('aria-label'))).toEqual(['thumbs up, 3', 'crab, 1', 'add a reaction']);
    expect(container.querySelector('.d-reaction-bar__add svg')).toHaveAttribute('data-icon', 'smile-plus');
  });

  it('toggles a chip by its emoji and opens the grid from the add button, passing the button', async () => {
    const user = userEvent.setup();
    const { onToggle, onAdd } = bar();
    await user.click(screen.getByRole('button', { name: 'crab, 1' }));
    expect(onToggle).toHaveBeenCalledWith('🦀');
    const add = screen.getByRole('button', { name: 'add a reaction' });
    await user.click(add);
    expect(onAdd).toHaveBeenCalledWith(add);
  });

  it('shows safe received text while toggling the original reaction identity', async () => {
    const raw = '\u202egpj.exe';
    const { onToggle } = bar({ items: [{ emoji: raw, display: 'gpj.exe', count: 1, mine: false, name: 'gpj.exe, 1' }] });
    const chip = screen.getByRole('button', { name: 'gpj.exe, 1' });
    expect(chip.textContent).toBe('gpj.exe1');
    await userEvent.setup().click(chip);
    expect(onToggle).toHaveBeenCalledWith(raw);
  });

  it('is one roving tab stop across the chips and the add button, and none when not tabbable', () => {
    const { rerender, onToggle, onAdd } = bar();
    const b = screen.getAllByRole('button');
    expect(b.map((x) => x.tabIndex)).toEqual([0, -1, -1]);
    b[0]!.focus();
    fireEvent.keyDown(b[0]!, { key: 'ArrowRight' });
    fireEvent.keyDown(b[1]!, { key: 'ArrowRight' });
    expect(b[2]).toHaveFocus();
    fireEvent.keyDown(b[2]!, { key: 'ArrowRight' });
    expect(b[0]).toHaveFocus();
    rerender(<ReactionBar label="reactions" tabbable={false} items={ITEMS} onToggle={onToggle} addLabel="add a reaction" onAdd={onAdd} />);
    expect(screen.getAllByRole('button').map((x) => x.tabIndex)).toEqual([-1, -1, -1]);
  });

  it('renders nothing without reactions', () => {
    const { container } = bar({ items: [] });
    expect(container.firstChild).toBeNull();
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><ReactionBar label="reactions" tabbable items={ITEMS} onToggle={() => {}} addLabel="add a reaction" onAdd={() => {}} /></div>);
    await expectNoAxeViolations(container);
  });
});
