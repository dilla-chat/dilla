import type { Meta, StoryObj } from '@storybook/react-vite';
import { Segmented } from './Segmented.tsx';

// Every string is L-COPY-02's (notify.default.*, notify.channel.*, appearance.theme.*).
const NOTIFY = [{ value: 'dms-mentions', label: 'direct messages and mentions' }, { value: 'everything', label: 'every message' }, { value: 'nothing', label: 'nothing' }];
const THEMES = [{ value: 'system', label: 'system' }, { value: 'signal', label: 'signal' }, { value: 'light', label: 'light' }, { value: 'high-contrast', label: 'high contrast' }];
const MODES = [{ value: 'default', label: 'default' }, { value: 'all', label: 'all' }, { value: 'mentions', label: 'mentions' }, { value: 'nothing', label: 'nothing' }];
const meta = {
  title: 'Form/Segmented', component: Segmented,
  args: { id: 'notify-default', label: 'notify me about', options: NOTIFY, value: 'dms-mentions', onChange: () => {} },
} satisfies Meta<typeof Segmented>;
export default meta;
type Story = StoryObj<typeof meta>;
export const NotifyDefault: Story = {};
export const Theme: Story = { args: { id: 'theme', label: 'theme', options: THEMES, value: 'system' } };
export const ChannelMode: Story = { args: { id: 'notify-general', label: '#general · Midgard Crew', options: MODES, value: 'mentions' } };
