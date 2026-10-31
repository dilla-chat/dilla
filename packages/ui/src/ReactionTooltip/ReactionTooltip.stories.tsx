import { expect, userEvent, within } from 'storybook/test';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { ReactionTooltip } from './ReactionTooltip.tsx';
import { Tooltip } from '../Tooltip/Tooltip.tsx';
const meta = {
  title: 'Conversation/ReactionTooltip', component: ReactionTooltip,
  args: { emoji: '🛡️', text: 'Ada, Björn and you reacted with :shield:', avatars: [{ name: 'Ada' }, { name: 'Björn' }, { name: 'Mira' }] },
  render: args => <Tooltip label={args.text} content={<ReactionTooltip {...args} />}><button type="button" aria-label="shield, 3">🛡️ 3</button></Tooltip>,
  decorators: [Story => <div style={{ padding: 80 }}><Story /></div>],
} satisfies Meta<typeof ReactionTooltip>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Default: Story = {
  play: async ({ canvasElement }) => {
    await userEvent.tab();
    const tooltip = await within(document.body).findByRole('tooltip');
    await expect(tooltip).toHaveTextContent('reacted with :shield:');
    await expect(tooltip.querySelector('.d-reaction-tooltip__emoji')).toHaveTextContent('🛡️');
    await userEvent.keyboard('{Escape}');
    canvasElement.querySelector<HTMLButtonElement>('button')!.blur();
  },
};
export const SixAvatars: Story = { args: { text: 'Ada, Björn and 4 others reacted with :shield:', avatars: ['Ada', 'Björn', 'Mira', 'Sven', 'Liv', 'Kari'].map(name => ({ name })) } };
