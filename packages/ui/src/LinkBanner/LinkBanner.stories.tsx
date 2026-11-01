import type { Meta, StoryObj } from '@storybook/react-vite';
import { LinkBanner } from './LinkBanner.tsx';

// Switch the theme in the toolbar (signal, light, high-contrast). A real 390px phone width.
const meta = { title: 'Shell/LinkBanner', component: LinkBanner, parameters: { layout: 'fullscreen' } } satisfies Meta<typeof LinkBanner>;
export default meta;
type Story = StoryObj<typeof meta>;

const labels = { offline: 'offline · reconnecting to dilla.thim.dev', connecting: 'connecting to dilla.thim.dev' };
const frame = (el: React.ReactNode) => <div style={{ maxWidth: 390 }}>{el}</div>;

export const Offline: Story = { args: { status: 'offline', labels }, render: args => frame(<LinkBanner {...args} />) };
export const Connecting: Story = { args: { status: 'connecting', labels }, render: args => frame(<LinkBanner {...args} />) };
