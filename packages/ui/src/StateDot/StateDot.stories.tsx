import type { Meta, StoryObj } from '@storybook/react-vite';
import { StateDot, type StateDotTone } from './StateDot.tsx';

const TONES: StateDotTone[] = ['ok', 'off', 'pending', 'idle', 'dnd', 'warn', 'danger'];

// Switch the theme in the toolbar (signal, light, high-contrast); every dot sits beside its word.
const meta = { title: 'Primitives/StateDot', component: StateDot, args: { tone: 'ok' } } satisfies Meta<typeof StateDot>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Ok: Story = {};
export const AllTones: Story = {
  render: () => (
    <ul style={{ listStyle: 'none', margin: 0, padding: 0, display: 'grid', gap: 8 }}>
      {TONES.map(t => (
        <li key={t} style={{ display: 'flex', alignItems: 'center', gap: 16 }}>
          <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, minWidth: 96 }}><StateDot tone={t} /> {t}</span>
          <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}><StateDot tone={t} size="md" /> {t} md</span>
        </li>
      ))}
    </ul>
  ),
};
