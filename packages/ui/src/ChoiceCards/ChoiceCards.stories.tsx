import type { Meta, StoryObj } from '@storybook/react-vite';
import { useState } from 'react';
import { ChoiceCards, type ChoiceCardsProps } from './ChoiceCards.tsx';

const OPTIONS: ChoiceCardsProps['options'] = [
  { id: 'app', title: 'Authenticator app', hint: 'A six-digit code that changes every 30 s', icon: 'smartphone' },
  { id: 'passkey', title: 'Passkey', hint: 'Touch ID, Windows Hello or a security key', icon: 'fingerprint' },
];

function Controlled(args: ChoiceCardsProps) {
  const [value, setValue] = useState(args.value);
  return <ChoiceCards {...args} value={value} onChange={setValue} />;
}

const meta = {
  title: 'Ceremony/ChoiceCards', component: ChoiceCards,
  args: { name: 'factor', label: 'Second factor', options: OPTIONS, value: 'app', onChange: () => {} },
  render: args => <Controlled {...args} />,
  decorators: [Story => <div style={{ maxWidth: 560 }}><Story /></div>],
} satisfies Meta<typeof ChoiceCards>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Default: Story = {};
export const NothingChosen: Story = { args: { value: '' } };
export const PasskeyUnavailable: Story = {
  args: { options: [OPTIONS[0]!, { ...OPTIONS[1]!, disabled: true, reason: 'This browser has no passkey support' }] },
};
export const Narrow: Story = { decorators: [Story => <div style={{ width: 312 }}><Story /></div>] };
