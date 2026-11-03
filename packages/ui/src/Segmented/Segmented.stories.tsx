import { themes } from '@dilla/design-tokens';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { Segmented } from './Segmented.tsx';

// Every string is L-COPY-02's (notify.default.*, notify.channel.*, appearance.theme.*).
const NOTIFY = [{ value: 'dms-mentions', label: 'direct messages and mentions' }, { value: 'everything', label: 'every message' }, { value: 'nothing', label: 'nothing' }];
const THEMES = Object.keys(themes).map(value => ({ value, label: value }));
const MODES = [{ value: 'default', label: 'default' }, { value: 'all', label: 'all' }, { value: 'mentions', label: 'mentions' }, { value: 'nothing', label: 'nothing' }];
const meta = {
  title: 'Form/Segmented', component: Segmented,
  args: { id: 'notify-default', label: 'notify me about', options: NOTIFY, value: 'dms-mentions', onChange: () => {} },
} satisfies Meta<typeof Segmented>;
export default meta;
type Story = StoryObj<typeof meta>;
export const NotifyDefault: Story = {};
export const Theme: Story = { args: { id: 'theme', label: 'theme', options: THEMES, value: 'signal' } };
export const ChannelMode: Story = { args: { id: 'notify-general', label: '#general · Midgard Crew', options: MODES, value: 'mentions' } };
