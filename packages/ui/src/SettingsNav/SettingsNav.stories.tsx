import type { Meta, StoryObj } from '@storybook/react-vite';
import { SettingsNav } from './SettingsNav.tsx';

// Every string is L-COPY-02's (settings.nav.*).
const meta = {
  title: 'Settings/SettingsNav', component: SettingsNav,
  args: { label: 'settings sections', activeId: 'devices', onSelect: () => {},
    items: [{ id: 'devices', label: 'devices', group: 'Account' }, { id: 'notifications', label: 'notifications', group: 'Preferences' }, { id: 'appearance', label: 'appearance', group: 'Preferences' }] },
  decorators: [Story => <div style={{ width: '13.75rem', padding: '0.875rem 0.625rem', background: 'var(--bg-2)' }}><Story /></div>],
} satisfies Meta<typeof SettingsNav>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Devices: Story = {};
export const Notifications: Story = { args: { activeId: 'notifications' } };
export const GroupedIndex: Story = {};
// 16px icons in the glyph column, the names aligned (the settings sections of web3.9).
const WITH_ICONS = [
  { id: 'general', label: 'general', icon: 'settings' }, { id: 'privacy', label: 'privacy', icon: 'shield' }, { id: 'keyboard', label: 'keyboard', icon: 'keyboard' },
  { id: 'devices', label: 'devices', icon: 'monitor-smartphone' }, { id: 'voice', label: 'voice', icon: 'headphones' }, { id: 'sign-out', label: 'sign out', icon: 'log-out' },
] as const;
export const WithIcons: Story = { args: { activeId: 'privacy', items: WITH_ICONS } };
// One item without an icon keeps the empty glyph column, so its name still starts at the same x.
export const MixedIcons: Story = { args: { activeId: 'general', items: [WITH_ICONS[0], { id: 'about', label: 'about' }, WITH_ICONS[1]] } };
