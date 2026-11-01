import { render, screen, within } from '@testing-library/react';
import { it, expect } from 'vitest';
import { Keyboard } from './Keyboard.tsx';

// Task 10 (README §5.9): a chord is drawn one keycap per key, so a row is found by its keycaps joined with '+'.
// A row may carry a second chord (Control+/ beside Control/⌘+K), each its own keycap run (.d-keyhint).
const chordsOf = (row: Element) => Array.from(row.querySelectorAll('.d-keyhint')).map(h => Array.from(h.querySelectorAll('kbd')).map(k => k.textContent).join('+'));

// Fix round 2 (Jonas): the prototype's five rows first, with its labels and keycaps, then dilla's other shortcuts.
it('lists the prototype\'s rows as it draws them, then dilla\'s other shortcuts', () => {
  render(<Keyboard />);
  const table = screen.getByRole('table', { name: 'Keyboard' });
  expect(within(table).getAllByRole('rowheader').map(h => h.textContent)).toEqual(
    ['Search', 'Mute', 'Deafen', 'Close a window', 'Jump to present', 'Open search', 'Call the open direct message']);
  expect(Array.from(table.querySelectorAll('tbody tr')).map(r => chordsOf(r))).toEqual(
    [['⌘+K'], ['⌘+⇧+M'], ['⌘+⇧+D'], ['Esc'], ['Ctrl+End'], ['Ctrl+/'], ['⌘+⇧+Enter']]);
});

// Decision 1 (WCAG 2.1.4): no single character key; Esc is the one unmodified key, and it is not a character.
it('binds no single character key', () => {
  render(<Keyboard />);
  for (const row of Array.from(document.querySelectorAll('tbody tr'))) {
    const keys = Array.from(row.querySelectorAll('kbd')).map(k => k.textContent ?? '');
    if (keys.length === 1) expect(keys).toEqual(['Esc']);
  }
});

it('has a sentence-case heading', () => {
  render(<Keyboard />);
  expect(screen.getByRole('heading', { name: 'Keyboard', level: 2 })).toBeInTheDocument();
});
