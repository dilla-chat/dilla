import type { Meta, StoryObj } from '@storybook/react-vite';
import { CallBar, type CallBarProps } from './CallBar.tsx';

// Switch the theme in the toolbar (signal, light, high-contrast). A real 390px phone width.
const meta = { title: 'Voice/CallBar', component: CallBar, parameters: { layout: 'fullscreen' } } satisfies Meta<typeof CallBar>;
export default meta;
type Story = StoryObj<typeof meta>;

const base: CallBarProps = {
  phase: 'connected', where: 'longhouse', muted: false,
  labels: { open: 'open call in longhouse', mute: 'mute', leave: 'leave call', region: 'call' },
  onOpen: () => {}, onMute: () => {}, onLeave: () => {},
};
const frame = (el: React.ReactNode) => <div style={{ maxWidth: 390 }}>{el}</div>;

export const Connected: Story = { args: base, render: args => frame(<CallBar {...args} />) };
export const Muted: Story = { args: { ...base, muted: true }, render: args => frame(<CallBar {...args} />) };
export const Connecting: Story = { args: { ...base, phase: 'connecting' }, render: args => frame(<CallBar {...args} />) };
export const Reconnecting: Story = { args: { ...base, phase: 'reconnecting' }, render: args => frame(<CallBar {...args} />) };
export const Paused: Story = { args: { ...base, phase: 'paused' }, render: args => frame(<CallBar {...args} />) };
export const AllPhases: Story = {
  args: base,
  render: args => (
    <div style={{ maxWidth: 390, display: 'grid', gap: 16 }}>
      {(['connecting', 'connected', 'reconnecting', 'paused'] as const).map(phase => (
        <CallBar key={phase} {...args} phase={phase} labels={{ ...args.labels, region: `call, ${phase}` }} />
      ))}
    </div>
  ),
};
