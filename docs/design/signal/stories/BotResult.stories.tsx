// Worked example against the proposed API in README §9. Place at packages/ui/src/BotResult/BotResult.stories.tsx.
import type { Meta, StoryObj } from '@storybook/react-vite';
import { BotResult } from './BotResult.tsx';

const meta = {
  title: 'Feed/BotResult',
  component: BotResult,
  args: { onDismiss: () => {} },
  decorators: [Story => <div style={{ width: 640, background: 'var(--bg)' }}><Story /></div>],
} satisfies Meta<typeof BotResult>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Pending: Story = { args: { status: 'pending', body: 'Checking your voice channel…' } };
export const Success: Story = { args: { status: 'success', body: 'Reminder set for Fri 30 Oct, 20:00 CET in #general.' } };
export const FailedNotInVoice: Story = { args: { status: 'failed', body: 'Join a voice channel first. Glenn plays where you are.' } };
export const FailedNoDj: Story = { args: { status: 'failed', body: 'Playback needs a role with Control music. An owner sets it in Server settings → Roles.' } };
export const ConfirmKick: Story = {
  args: { status: 'pending', body: 'Kick Björn? They can rejoin with a new invite.', confirm: { label: 'Kick', danger: true, onConfirm: () => {}, onCancel: () => {} } },
};
export const NotConfirmed: Story = { args: { status: 'cancelled', body: 'Kick cancelled. Nothing changed.' } };
