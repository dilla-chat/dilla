import { useState } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { PollDialog, type PollDialogProps } from './PollDialog.tsx';
function Live(args: PollDialogProps) { const [value, onChange] = useState(args.value); return <PollDialog {...args} value={value} onChange={onChange} canPost={!!value.question.trim() && value.answers.filter(a => a.trim()).length >= 2} />; }
const meta = { title: 'Form/PollDialog', component: PollDialog, globals: { theme: 'signal' }, render: args => <Live {...args} />, args: {
  open: true, channelName: 'general', bot: { name: 'Glenn' }, value: { question: '', answers: ['', ''], duration: '24h', multi: false }, error: null, canPost: false, onChange: () => {}, onPost: () => {}, onCancel: () => {},
} } satisfies Meta<typeof PollDialog>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Empty: Story = {};
export const Filled: Story = { args: { value: { question: 'What should we raid on Friday?', answers: ['the bridge', 'the harbour'], duration: '24h', multi: false }, canPost: true } };
export const Error: Story = { args: { error: 'Write a question' } };
