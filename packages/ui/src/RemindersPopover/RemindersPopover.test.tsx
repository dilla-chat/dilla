import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { RemindersPopover, type ReminderItem } from './RemindersPopover.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const LABELS = { title: 'Reminders', tabs: 'Reminder lists', due: 'Due', upcoming: 'Upcoming', dismiss: 'Dismiss', cancel: 'Cancel',
  emptyDue: 'Nothing due.', emptyUpcoming: 'No upcoming reminders. Try /remind.', footer: 'Private to you, across your devices. Add one with /remind.' };
const ITEMS: ReminderItem[] = [
  { id: '1', text: 'bring two ropes', when: 'Today, 20:00', where: '#general', due: true },
  { id: '2', text: 'raid at the bridge', when: 'Fri 30 Oct, 20:00', where: '#general', due: false },
];

function Harness({ items = ITEMS, onAction = vi.fn(), onClose = vi.fn() }: { items?: ReminderItem[]; onAction?: (id: string, a: 'dismiss' | 'cancel') => void; onClose?: () => void }) {
  const [tab, setTab] = useState<'due' | 'upcoming'>('due');
  return (
    <div className="d-root">
      <button type="button" id="bell">bell</button>
      <button type="button">elsewhere</button>
      <RemindersPopover items={items} tab={tab} onTab={setTab} onAction={onAction} onClose={onClose} labels={LABELS} botInitials="GL"
        returnFocus={() => document.getElementById('bell')} anchor={() => document.getElementById('bell')} />
    </div>
  );
}

describe('RemindersPopover', () => {
  it('says only that it is loading, with no empty line under it, until the list is known', () => {
    render(<RemindersPopover items={[]} tab="due" onTab={vi.fn()} onAction={vi.fn()} onClose={vi.fn()} labels={LABELS} status="Loading reminders…" loading />);
    expect(screen.getByText('Loading reminders…')).toBeInTheDocument();
    expect(screen.queryByText('Nothing due.')).toBeNull();
  });
  it('is a non-modal dialog named Reminders; focus starts on the picked tab; the due tab lists the due ones with Dismiss', () => {
    render(<Harness />);
    const dialog = screen.getByRole('dialog', { name: 'Reminders' });
    expect(dialog).toHaveAttribute('aria-modal', 'false');
    expect(screen.getByRole('tab', { name: 'Due 1' })).toHaveFocus();
    expect(screen.getByRole('tab', { name: 'Upcoming 1' })).toHaveAttribute('aria-selected', 'false');
    const rows = within(dialog).getAllByRole('listitem');
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent('bring two ropes');
    expect(rows[0]).toHaveTextContent('Today, 20:00 · #general');
    expect(within(rows[0]).getByRole('button', { name: 'Dismiss' })).toHaveAccessibleDescription('bring two ropes');
    expect(dialog).toHaveTextContent(LABELS.footer);
  });
  it('the arrow keys move between the tabs; Upcoming lists the rest with Cancel', async () => {
    const user = userEvent.setup();
    const onAction = vi.fn();
    render(<Harness onAction={onAction} />);
    await user.keyboard('{ArrowRight}');
    expect(screen.getByRole('tab', { name: 'Upcoming 1' })).toHaveFocus();
    expect(screen.getByRole('tab', { name: 'Upcoming 1' })).toHaveAttribute('aria-selected', 'true');
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onAction).toHaveBeenCalledWith('2', 'cancel');
  });
  it('Dismiss reports the due one', async () => {
    const user = userEvent.setup();
    const onAction = vi.fn();
    render(<Harness onAction={onAction} />);
    await user.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(onAction).toHaveBeenCalledWith('1', 'dismiss');
  });
  it('says the tab is empty', async () => {
    const user = userEvent.setup();
    render(<Harness items={[]} />);
    expect(screen.getByText('Nothing due.')).toBeInTheDocument();
    await user.click(screen.getByRole('tab', { name: 'Upcoming 0' }));
    expect(screen.getByText('No upcoming reminders. Try /remind.')).toBeInTheDocument();
  });
  it('Escape closes it and gives focus back to the bell', async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    render(<Harness onClose={onClose} />);
    await user.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(screen.getByRole('button', { name: 'bell' })).toHaveFocus();
  });
  it('a press outside closes it; a press on the bell (its anchor) does not count as outside', async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    render(<Harness onClose={onClose} />);
    await user.click(screen.getByRole('button', { name: 'bell' }));
    expect(onClose).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'elsewhere' }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
  it('a busy row cannot be changed twice', () => {
    render(<Harness items={[{ ...ITEMS[0], busy: true }]} />);
    expect(screen.getByRole('button', { name: 'Dismiss' })).toBeDisabled();
  });
  it('has no serious axe violations', async () => {
    const { container } = render(<Harness />);
    await expectNoAxeViolations(container);
  });
});
