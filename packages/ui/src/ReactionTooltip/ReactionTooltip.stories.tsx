import type { Meta, StoryObj } from '@storybook/react-vite';
import { ReactionTooltip } from './ReactionTooltip.tsx';
import { Tooltip } from '../Tooltip/Tooltip.tsx';
const meta = {
  title: 'Conversation/ReactionTooltip', component: ReactionTooltip,
  args: { text: 'Ada, Björn and you reacted with 🛡️', avatars: [{ name: 'Ada' }, { name: 'Björn' }, { name: 'Mira' }] },
  render: args => <Tooltip label={args.text} content={<ReactionTooltip {...args} />}><button type="button" aria-label="shield, 3">🛡️ 3</button></Tooltip>,
  decorators: [Story => <div style={{ padding: 80 }}><Story /></div>],
} satisfies Meta<typeof ReactionTooltip>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Default: Story = {};
export const SixAvatars: Story = { args: { text: 'Ada, Björn and 4 others reacted with 🛡️', avatars: ['Ada', 'Björn', 'Mira', 'Sven', 'Liv', 'Kari'].map(name => ({ name })) } };
