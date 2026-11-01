import type { Meta, StoryObj } from '@storybook/react-vite';
import { IconButton } from './IconButton.tsx';

const meta = { title: 'Primitives/IconButton', component: IconButton, args: { icon: 'settings', label: 'open settings' } } satisfies Meta<typeof IconButton>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {};
export const WithChord: Story = { args: { icon: 'search', label: 'search', keys: ['Ctrl', 'K'] } };
export const Pressed: Story = { args: { icon: 'headphones', label: 'deafen', pressed: true, keys: ['Ctrl', 'Shift', 'D'] } };
export const Unpressed: Story = { args: { icon: 'headphones', label: 'deafen', pressed: false } };
export const Compact: Story = { args: { size: 28 } };
export const Disabled: Story = { args: { disabled: true } };
// The four sizes side by side. On a desktop the box is the size; on a phone layout every one is 44 x 44 (base.css).
export const Sizes: Story = {
  render: () => (
    <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
      <IconButton icon="plus" label="add, 28" size={28} />
      <IconButton icon="plus" label="add, 32" size={32} />
      <IconButton icon="plus" label="add, 40" size={40} />
      <IconButton icon="plus" label="add, 44" size={44} />
    </div>
  ),
};
