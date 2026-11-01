import type { Meta, StoryObj } from '@storybook/react-vite';
import { ConfirmDialog } from './ConfirmDialog.tsx';
import { TextField } from '../TextField/TextField.tsx';

const meta = {
  title: 'Shell/ConfirmDialog', component: ConfirmDialog,
  args: {
    open: true, title: 'Ban Astrid?', body: 'Astrid cannot join again until you unban them.', confirmLabel: 'ban', busyLabel: 'Banning…',
    cancelLabel: 'cancel', tone: 'danger', busy: false, error: null, onConfirm: () => {}, onClose: () => {},
  },
} satisfies Meta<typeof ConfirmDialog>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Danger: Story = {};
export const Accent: Story = { args: { tone: 'accent', title: 'Hand the server to Astrid?', confirmLabel: 'hand over', busyLabel: 'Handing over…' } };
export const Busy: Story = { args: { busy: true } };
export const Refused: Story = { args: { error: 'You cannot ban this member.', attempt: 1 } };
export const TypedName: Story = {
  args: {
    title: 'Delete Midgard Crew?', confirmLabel: 'delete', busyLabel: 'Deleting…', canConfirm: false,
    children: <TextField id="confirm-name" label="Type the server name" value="" onChange={() => {}} autoComplete="off" />,
  },
};
/** Task 9: the prototype's kick confirmation: the 40px avatar beside the 15px title and the body, Cancel and the filled danger action. */
export const Kick: Story = { args: { person: 'Björn', title: 'Kick Björn from Midgard Crew?', body: 'Björn leaves now and can come back only with a new invite.', confirmLabel: 'Kick', busyLabel: 'Kicking…', cancelLabel: 'Cancel' } };
