import { render, screen, within } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { KeyboardTable } from './KeyboardTable.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const ROWS = [
  { action: 'search', keys: ['ctrl', 'k'] },
  { action: 'previous channel', keys: ['alt', '↑'] },
  { action: 'close', keys: ['esc'] },
];

describe('KeyboardTable', () => {
  it('is a table named by its caption', () => {
    render(<KeyboardTable caption="keyboard shortcuts" rows={ROWS} />);
    const table = screen.getByRole('table', { name: 'keyboard shortcuts' });
    expect(table.querySelector('caption')).toHaveTextContent('keyboard shortcuts');
  });

  it('has one row per action, the action on the left and the chord on the right as kbds', () => {
    render(<KeyboardTable caption="keyboard shortcuts" rows={ROWS} />);
    const rows = screen.getAllByRole('row');
    expect(rows).toHaveLength(3);
    const first = within(rows[0]);
    expect(first.getByRole('rowheader')).toHaveTextContent('search');
    const cell = first.getByRole('cell');
    expect([...cell.querySelectorAll('kbd')].map(k => k.textContent)).toEqual(['ctrl', 'k']);
    expect(rows[0].firstElementChild).toBe(first.getByRole('rowheader'));
    expect(rows[0].lastElementChild).toBe(cell);
  });

  it('draws a second chord after the word between them', () => {
    render(<KeyboardTable caption="keyboard shortcuts" orLabel="or" rows={[{ action: 'search', keys: ['ctrl', 'k'], alt: ['ctrl', '/'] }]} />);
    const cell = screen.getByRole('cell');
    expect([...cell.querySelectorAll('.d-keyhint')].map(h => [...h.querySelectorAll('kbd')].map(k => k.textContent))).toEqual([['ctrl', 'k'], ['ctrl', '/']]);
    expect(cell).toHaveTextContent('ctrlkorctrl/');
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><KeyboardTable caption="keyboard shortcuts" rows={ROWS} /></div>);
    await expectNoAxeViolations(container);
  });
  // Task 11 (merge 9+10 minor): an alt chord without orLabel shows both chords and no empty word element.
  it('a row with alt and no orLabel draws both chords and no empty or element', () => {
    const { container } = render(<KeyboardTable caption="keyboard shortcuts" rows={[{ action: 'search', keys: ['ctrl', 'k'], alt: ['ctrl', '/'] }]} />);
    expect(container.querySelector('.d-keyboard-table__or')).toBeNull();
    expect(container.querySelectorAll('.d-keyboard-table__keys kbd').length).toBeGreaterThanOrEqual(4);
  });
});
