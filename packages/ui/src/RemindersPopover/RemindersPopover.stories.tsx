import type { Meta, StoryObj } from '@storybook/react-vite';
import { useState } from 'react';
import { RemindersPopover, type ReminderItem } from './RemindersPopover.tsx';

// The prototype's copy and its two sample reminders (README §6.7).
const LABELS = { title: 'Reminders', tabs: 'Reminder lists', due: 'Due', upcoming: 'Upcoming', dismiss: 'Dismiss', cancel: 'Cancel',
  emptyDue: 'Nothing due.', emptyUpcoming: 'No upcoming reminders. Try /remind.', footer: 'Private to you, across your devices. Add one with /remind.' };
const ITEMS: ReminderItem[] = [
  { id: '1', text: 'bring two ropes', when: 'Today, 20:00', where: '#general', due: true },
  { id: '2', text: 'raid at the bridge', when: 'Fri 30 Oct, 20:00 CET', where: '#general', due: false },
];

function Live({ items, start }: { items: ReminderItem[]; start: 'due' | 'upcoming' }) {
  const [tab, setTab] = useState(start);
  // The bell's box: the popover hangs 36px under it, 6px past its right edge (the prototype's data-rem wrapper).
  return (
    <div style={{ display: 'flex', justifyContent: 'flex-end', paddingRight: 24, minHeight: 420 }}>
      <span style={{ position: 'relative', display: 'inline-flex', width: 28, height: 28 }}>
        <RemindersPopover items={items} tab={tab} onTab={setTab} onAction={() => {}} onClose={() => {}} botInitials="GL" labels={LABELS} />
      </span>
    </div>
  );
}

const meta = { title: 'Shell/RemindersPopover', component: RemindersPopover, globals: { theme: 'signal' } } satisfies Meta<typeof RemindersPopover>;
export default meta;
type Story = StoryObj<typeof meta>;
const base = { items: ITEMS, tab: 'due' as const, onTab: () => {}, onAction: () => {}, onClose: () => {}, labels: LABELS };

export const Due: Story = { args: base, render: () => <Live items={ITEMS} start="due" /> };
export const Upcoming: Story = { args: base, render: () => <Live items={ITEMS} start="upcoming" /> };
export const Empty: Story = { args: base, render: () => <Live items={[]} start="due" /> };
