import type { Meta, StoryObj } from '@storybook/react-vite';
import { TopBar } from './TopBar.tsx';

// README §8: TopBar Default, InVoice, RemindersDue. Drawn 1280 wide (the decorator's 16px padding taken back), as the
// prototype's bar at 1280×800, so the centred search lands where it does there.
const meta = {
  title: 'Shell/TopBar',
  component: TopBar,
  parameters: { layout: 'fullscreen' },
  decorators: [Story => <div style={{ width: 1280, margin: -16 }}><Story /></div>],
  args: {
    searchLabel: 'Search or jump to…', searchShortcut: '⌘K', onSearch: () => {}, remindersDue: 0, onReminders: () => {}, onSettings: () => {},
    labels: { reminders: 'Reminders', settings: 'Open settings', voice: 'open the call in harbour', remindersDue: (n: number) => `${String(n)} due` },
  },
} satisfies Meta<typeof TopBar>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {};
export const InVoice: Story = { args: { voice: { room: 'harbour', pingMs: 24, onOpen: () => {} } } };
export const RemindersDue: Story = { args: { remindersDue: 3 } };
/** Off Apple platforms the chord reads "Ctrl K". */
export const CtrlShortcut: Story = { args: { searchShortcut: 'Ctrl K' } };
