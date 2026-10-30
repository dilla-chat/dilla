import { render, screen, within, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { RecoveryKey, type RecoveryKeyProps } from './RecoveryKey.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const GROUPS = ['7K3M', 'QW9D', 'X2RT', '0PNA', 'HV5C', 'J8ZE', 'M4TB', 'S6YF', '1GKD', 'R3WP', 'ZN7H', 'C9QX', '5TVA'] as const;
// L-COPY-01: onboarding.keys.acknowledge, onboarding.keys.label, onboarding.keys.print.
const ACK = 'I have written down or printed my recovery key';
const LABEL = 'Recovery key';
const PRINT = 'Print';

function renderKey(over: Partial<RecoveryKeyProps> = {}) {
  const props: RecoveryKeyProps = {
    groups: GROUPS, label: LABEL, acknowledgeLabel: ACK, acknowledged: false,
    onAcknowledge: vi.fn(), printLabel: PRINT, onPrint: vi.fn(), ...over,
  };
  return { props, ...render(<RecoveryKey {...props} />) };
}

describe('RecoveryKey', () => {
  it('lists the 13 groups in order as a list named by the label', () => {
    const { container } = renderKey();
    expect(container.firstElementChild).toHaveClass('d-recovery-key');
    const list = screen.getByRole('list', { name: LABEL });
    const items = within(list).getAllByRole('listitem');
    expect(items).toHaveLength(13);
    expect(items.map(i => i.textContent)).toEqual([...GROUPS]);
  });

  it('holds nothing but the groups as text, so selecting the grid copies the key and nothing else', () => {
    renderKey();
    expect(screen.getByRole('list', { name: LABEL }).textContent).toBe(GROUPS.join(''));
  });

  it('offers no copy control: print is the only button', () => {
    renderKey();
    expect(screen.getAllByRole('button')).toHaveLength(1);
    expect(screen.getByRole('button', { name: PRINT })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /copy/i })).toBeNull();
    expect(screen.queryByText(/copy/i)).toBeNull();
  });

  it('does not cancel copying the key text', () => {
    renderKey();
    expect(fireEvent.copy(screen.getByRole('list', { name: LABEL }))).toBe(true);
  });

  it('reports the acknowledgement both ways', async () => {
    const user = userEvent.setup();
    const onAcknowledge = vi.fn();
    const { rerender, props } = renderKey({ onAcknowledge });
    const box = screen.getByRole('checkbox', { name: ACK });
    expect(box).not.toBeChecked();
    await user.click(box);
    expect(onAcknowledge).toHaveBeenLastCalledWith(true);
    rerender(<RecoveryKey {...props} acknowledged />);
    expect(screen.getByRole('checkbox', { name: ACK })).toBeChecked();
    await user.click(screen.getByRole('checkbox', { name: ACK }));
    expect(onAcknowledge).toHaveBeenLastCalledWith(false);
  });

  // Wave B task 3b: the prototype's order, the actions row (Print) under the grid and the check box under it.
  it('is operable by keyboard: Tab reaches print first, then the checkbox, which Space ticks', async () => {
    const user = userEvent.setup();
    const onAcknowledge = vi.fn();
    const onPrint = vi.fn();
    renderKey({ onAcknowledge, onPrint });
    await user.tab();
    expect(screen.getByRole('button', { name: PRINT })).toHaveFocus();
    await user.keyboard('{Enter}');
    expect(onPrint).toHaveBeenCalledTimes(1);
    await user.tab();
    expect(screen.getByRole('checkbox', { name: ACK })).toHaveFocus();
    await user.keyboard(' ');
    expect(onAcknowledge).toHaveBeenCalledWith(true);
  });
  it('names the grid for assistive technology only, as the prototype draws no label', () => {
    renderKey();
    expect(screen.getByText(LABEL)).toHaveClass('d-sr-only');
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><RecoveryKey groups={GROUPS} label={LABEL} acknowledgeLabel={ACK}
      acknowledged={false} onAcknowledge={() => {}} printLabel={PRINT} onPrint={() => {}} /></div>);
    await expectNoAxeViolations(container);
  });
});
