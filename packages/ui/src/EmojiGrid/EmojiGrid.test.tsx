import { render, screen, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { EmojiGrid } from './EmojiGrid.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

// REACTION_EMOJI of task 9 (Mesh chat-app.jsx:389-393) with the L-COPY-03 names, quoted.
const EMOJI = ['👍', '👎', '❤️', '😄', '😂', '😢', '😡', '😍', '🎉', '🔥', '💯', '✨', '🙏', '👀', '🤔', '😴',
  '🛠', '🚀', '✅', '❌', '💡', '📌', '🐛', '📦', '☕', '🍕', '🌮', '🎨', '🎵', '🌙', '☀️', '🦀'];
const NAMES = ['thumbs up', 'thumbs down', 'red heart', 'grinning face', 'tears of joy', 'crying face', 'angry face', 'heart eyes',
  'party popper', 'fire', 'hundred points', 'sparkles', 'folded hands', 'eyes', 'thinking face', 'sleeping face',
  'hammer and wrench', 'rocket', 'check mark', 'cross mark', 'light bulb', 'pushpin', 'bug', 'package',
  'hot beverage', 'pizza', 'taco', 'artist palette', 'musical note', 'crescent moon', 'sun', 'crab'];
const ITEMS = EMOJI.map((emoji, i) => ({ emoji, name: NAMES[i]! }));

function grid() {
  const onPick = vi.fn();
  const onClose = vi.fn();
  const r = render(<EmojiGrid label="pick a reaction" items={ITEMS} onPick={onPick} onClose={onClose} />);
  return { ...r, onPick, onClose, b: screen.getAllByRole('button') };
}

describe('EmojiGrid', () => {
  const previewItems = [{ emoji: '👍', name: 'thumbs up' }, { emoji: '👎', name: 'thumbs down' }, { emoji: '🛡️', name: 'shield' }];
  function searchable(items = previewItems) {
    return render(<EmojiGrid label="Emoji" items={items} search={{ label: 'Search emoji', placeholder: 'Search emoji' }}
      onPick={() => {}} onClose={() => {}} />);
  }
  it('opens the searchable picker with search focused and the prototype shield preview', () => {
    const { container } = searchable();
    expect(screen.getByRole('searchbox', { name: 'Search emoji' })).toHaveFocus();
    expect(container.querySelector('.d-emoji-grid__foot')).toHaveTextContent('🛡️:shield:');
    expect(screen.getAllByRole('button').map(button => button.getAttribute('data-emoji'))).toEqual(['👍', '👎', '🛡️']);
    expect(screen.getAllByRole('button').filter(button => button.tabIndex === 0)).toEqual([screen.getByRole('button', { name: 'thumbs up' })]);
  });
  it('updates the preview from actual keyboard grid focus', async () => {
    const user = userEvent.setup();
    const { container } = searchable();
    const footer = container.querySelector('.d-emoji-grid__foot');
    await user.keyboard('{ArrowDown}');
    expect(screen.getByRole('button', { name: 'thumbs up' })).toHaveFocus();
    expect(footer).toHaveTextContent('👍:thumbs_up:');
    await user.keyboard('{ArrowRight}');
    expect(screen.getByRole('button', { name: 'thumbs down' })).toHaveFocus();
    expect(footer).toHaveTextContent('👎:thumbs_down:');
  });
  it('retains the last pointer preview when the pointer leaves an emoji', async () => {
    const user = userEvent.setup();
    const { container } = searchable();
    await user.keyboard('{ArrowDown}{End}');
    expect(screen.getByRole('button', { name: 'shield' })).toHaveFocus();
    const thumbs = screen.getByRole('button', { name: 'thumbs up' });
    await user.hover(thumbs);
    expect(container.querySelector('.d-emoji-grid__foot')).toHaveTextContent('👍:thumbs_up:');
    await user.unhover(thumbs);
    expect(container.querySelector('.d-emoji-grid__foot')).toHaveTextContent('👍:thumbs_up:');
  });
  it('uses an inventory item when a searchable picker has no shield', () => {
    const { container } = searchable([{ emoji: '🔥', name: 'fire' }]);
    expect(container.querySelector('.d-emoji-grid__foot')).toHaveTextContent('🔥:fire:');
    expect(screen.getAllByRole('button')).toHaveLength(1);
  });
  it('is a non-modal dialog of 32 named buttons, the first focused and the only tab stop', () => {
    const { b } = grid();
    const dialog = screen.getByRole('dialog', { name: 'pick a reaction' });
    expect(dialog).toHaveClass('d-emoji-grid');
    expect(dialog).not.toHaveAttribute('aria-modal');
    expect(b.map((x) => x.getAttribute('aria-label'))).toEqual(NAMES);
    expect(b.map((x) => x.getAttribute('data-emoji'))).toEqual(EMOJI);
    expect(b[0]).toHaveFocus();
    expect(b.filter((x) => x.tabIndex === 0)).toEqual([b[0]]);
  });

  it('moves in two dimensions without wrapping, with Home and End', () => {
    const { b } = grid();
    const key = (from: number, k: string) => { fireEvent.keyDown(b[from]!, { key: k }); return b.indexOf(document.activeElement as HTMLElement); };
    expect(key(0, 'ArrowRight')).toBe(1);
    expect(key(1, 'ArrowDown')).toBe(9);
    expect(key(9, 'ArrowDown')).toBe(17);
    expect(key(17, 'ArrowLeft')).toBe(16);
    expect(key(16, 'ArrowUp')).toBe(8);
    expect(key(8, 'ArrowUp')).toBe(0);
    expect(key(0, 'ArrowUp')).toBe(0);
    expect(key(0, 'ArrowLeft')).toBe(0);
    expect(key(0, 'End')).toBe(31);
    expect(key(31, 'ArrowDown')).toBe(31);
    expect(key(31, 'ArrowRight')).toBe(31);
    expect(key(31, 'Home')).toBe(0);
    expect(b.filter((x) => x.tabIndex === 0)).toEqual([b[0]]);
  });

  it('picks with a click, Enter and Space, once each', async () => {
    const user = userEvent.setup();
    const { b, onPick } = grid();
    await user.click(b[9]!);
    b[31]!.focus();
    await user.keyboard('{Enter}');
    b[2]!.focus();
    await user.keyboard(' ');
    expect(onPick.mock.calls.map((c) => c[0])).toEqual(['🔥', '🦀', '❤️']);
  });

  it('closes on Escape without letting it reach the row, and on a pointer down outside', () => {
    const outer = vi.fn();
    const onClose = vi.fn();
    render(<div onKeyDown={() => outer()}><button type="button">outside</button>
      <EmojiGrid label="pick a reaction" items={ITEMS} onPick={() => {}} onClose={onClose} /></div>);
    fireEvent.keyDown(screen.getAllByRole('button')[3]!, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(outer).not.toHaveBeenCalled();
    fireEvent.pointerDown(screen.getByRole('button', { name: 'outside' }));
    expect(onClose).toHaveBeenCalledTimes(2);
    fireEvent.pointerDown(screen.getByRole('button', { name: 'fire' }));
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><EmojiGrid label="pick a reaction" items={ITEMS} onPick={() => {}} onClose={() => {}} /></div>);
    await expectNoAxeViolations(container);
  });
});
