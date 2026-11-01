// Worked example against the proposed API in README §9. Place at packages/ui/src/SensitivityMeter/SensitivityMeter.stories.tsx.
// Levels are fixed (no timers) so screenshot tests stay deterministic.
import type { Meta, StoryObj } from '@storybook/react-vite';
import { SensitivityMeter } from './SensitivityMeter.tsx';

const meta = {
  title: 'Settings/SensitivityMeter',
  component: SensitivityMeter,
  args: { onThresholdChange: () => {}, levelDb: -100, thresholdDb: -50, auto: true, testing: false },
  decorators: [Story => <div style={{ width: 520, padding: 16, background: 'var(--bg)' }}><Story /></div>],
} satisfies Meta<typeof SensitivityMeter>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Idle: Story = {};
export const TestingHeard: Story = { args: { testing: true, levelDb: -28, thresholdDb: -48 } };
export const TestingTooQuiet: Story = { args: { testing: true, levelDb: -62, thresholdDb: -48 } };
export const Clipping: Story = { args: { testing: true, levelDb: -6, thresholdDb: -48 } };
export const ManualThreshold: Story = { args: { auto: false, testing: true, levelDb: -40, thresholdDb: -35 } };
