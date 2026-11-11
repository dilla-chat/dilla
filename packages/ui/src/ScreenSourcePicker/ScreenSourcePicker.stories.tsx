import type { Meta, StoryObj } from '@storybook/react-vite';
import { ScreenSourcePicker, type ScreenSourceTile } from './ScreenSourcePicker.tsx';

// Fake sources: the desktop picker window gets data: URLs of real thumbnails; the stories draw inline SVG so they need no capture.
const shot = (hue: number, label: string): string =>
  `data:image/svg+xml;utf8,${encodeURIComponent(`<svg xmlns="http://www.w3.org/2000/svg" width="320" height="180"><rect width="320" height="180" fill="hsl(${hue} 25% 22%)"/><rect x="16" y="16" width="120" height="10" rx="2" fill="hsl(${hue} 30% 45%)"/><rect x="16" y="36" width="288" height="128" rx="4" fill="hsl(${hue} 20% 14%)"/><text x="160" y="110" font-family="monospace" font-size="14" fill="hsl(${hue} 30% 70%)" text-anchor="middle">${label}</text></svg>`)}`;
const icon = (hue: number): string =>
  `data:image/svg+xml;utf8,${encodeURIComponent(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16"><rect width="16" height="16" rx="4" fill="hsl(${hue} 60% 50%)"/></svg>`)}`;
const SOURCES: ScreenSourceTile[] = [
  { id: 's1', name: 'Built-in Retina Display', kind: 'screen', thumbnail: shot(210, 'Built-in'), icon: null },
  { id: 's2', name: 'Studio Display', kind: 'screen', thumbnail: shot(150, 'Studio Display'), icon: null },
  { id: 'w1', name: 'Terminal — ssh homelab', kind: 'window', thumbnail: shot(100, 'Terminal'), icon: icon(100) },
  { id: 'w2', name: 'Notes', kind: 'window', thumbnail: shot(45, 'Notes'), icon: icon(45) },
  { id: 'w3', name: 'Figma — dilla signal handoff with a very long window title', kind: 'window', thumbnail: shot(280, 'Figma'), icon: icon(280) },
  { id: 'w4', name: 'Minimised window', kind: 'window', thumbnail: null, icon: null },
];

const meta = {
  title: 'Chrome/ScreenSourcePicker', component: ScreenSourcePicker,
  args: {
    open: true, sources: SOURCES, onShare: () => {}, onCancel: () => {},
    labels: { title: 'Share your screen', screens: 'Screens', windows: 'Windows', share: 'Share', cancel: 'Cancel', empty: 'Nothing to share here' },
  },
} satisfies Meta<typeof ScreenSourcePicker>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Screens: Story = {};
/** The Windows tab: the same grid, each window's app icon beside its title. */
export const Windows: Story = { args: { defaultTab: 'window' } };
export const Empty: Story = { args: { sources: [] } };
