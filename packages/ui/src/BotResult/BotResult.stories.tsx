import type { Meta, StoryObj } from '@storybook/react-vite';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';
import { CONVERSATION_CONTRAST_NOT } from '../../scripts/prototype-contrast.ts';
import { privateResultCopy } from '../../../web/src/screens/commands/copy.ts';
import { BotResult } from './BotResult.tsx';
const meta = {
  title: 'Conversation/BotResult', component: BotResult,
  parameters: { a11y: { config: { rules: [{ id: 'color-contrast', selector: `*${CONVERSATION_CONTRAST_NOT}` }] } } },
  args: { author: { name: 'Glenn', avatarName: 'Glenn', shape: 'bot' }, time: '10:24', onDismiss: () => undefined,
    labels: { ...privateResultCopy.labels, dismissName: privateResultCopy.dismissName('Result') } },
} satisfies Meta<typeof BotResult>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Pending: Story = { args: { status: 'pending', body: 'Saving your reminder…', labels: { ...privateResultCopy.labels, dismissName: privateResultCopy.dismissName('Saving your reminder…') } } };
export const Success: Story = { args: { status: 'success', body: 'Saved. I’ll remind you in 1 hour: bring two ropes', labels: { ...privateResultCopy.labels, dismissName: privateResultCopy.dismissName('Saved. I’ll remind you in 1 hour: bring two ropes') } } };
export const Failed: Story = { args: { status: 'failed', body: 'Join a voice channel first. Glenn plays where you are.', labels: { ...privateResultCopy.labels, dismissName: privateResultCopy.dismissName('Join a voice channel first. Glenn plays where you are.') } } };
export const NotConfirmed: Story = { args: { author: { name: 'dilla', avatarName: 'dilla', shape: 'circle' }, status: 'cancelled', body: 'Kick cancelled. Nothing changed.', labels: { ...privateResultCopy.labels, dismissName: privateResultCopy.dismissName('Kick cancelled. Nothing changed.') } } };

export const Phone: Story = { args: { status: 'success', body: 'Saved. I’ll remind you in 1 hour: bring two ropes', labels: { ...privateResultCopy.labels, dismissName: privateResultCopy.dismissName('Saved. I’ll remind you in 1 hour: bring two ropes') } },
  decorators: [Story => <PhoneLayoutProvider value><div data-layout="phone"><Story /></div></PhoneLayoutProvider>], parameters: { phoneViewport: true } };
