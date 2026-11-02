import type { Meta, StoryObj } from '@storybook/react-vite';
import { TabBar, type TabBarItem } from './TabBar.tsx';

// The theme toolbar uses the shared theme catalog. A real 390px phone width.
const meta = { title: 'Shell/TabBar', component: TabBar, parameters: { layout: 'fullscreen' } } satisfies Meta<typeof TabBar>;
export default meta;
type Story = StoryObj<typeof meta>;

const items = (current: string): TabBarItem[] => [
  { id: 'servers', label: 'servers', href: '#servers', icon: 'server', current: current === 'servers' },
  { id: 'dms', label: 'dms', href: '#dms', icon: 'message-square', current: current === 'dms', count: 3, countLabel: 'dms, 3 unread' },
  { id: 'activity', label: 'activity', href: '#activity', icon: 'activity', current: current === 'activity', count: 120, countLabel: 'activity, 120 unread' },
  { id: 'you', label: 'you', href: '#you', icon: 'user', current: current === 'you' },
];

export const Counts: Story = {
  args: { label: 'main', items: items('servers'), onSelect: () => {} },
  render: args => <div style={{ maxWidth: 390 }}><TabBar {...args} /></div>,
};
export const CurrentYou: Story = {
  args: { label: 'main', items: items('you'), onSelect: () => {} },
  render: args => <div style={{ maxWidth: 390 }}><TabBar {...args} /></div>,
};

/** Signal §5.10 (the prototype's phone tabs): Home, Activity (a count) and You, Home current. */
const SIGNAL_ITEMS = (current: string): TabBarItem[] => [
  { id: 'home', label: 'Home', href: '#home', icon: 'home', current: current === 'home' },
  { id: 'activity', label: 'Activity', href: '#activity', icon: 'bell', current: current === 'activity', count: 2, countLabel: 'Activity, 2 unread' },
  { id: 'you', label: 'You', href: '#you', icon: 'user', current: current === 'you' },
];
const checkHomeGlyph = (canvasElement: HTMLElement, current: boolean) => {
  const home = canvasElement.querySelector<HTMLAnchorElement>('a[href="#home"]')!;
  const glyph = home.querySelector<SVGSVGElement>('svg')!;
  const size = glyph.getBoundingClientRect();
  const color = getComputedStyle(glyph).color;
  const expectedColor = current ? 'rgb(228, 232, 238)' : 'rgb(118, 125, 137)';
  if (glyph.querySelectorAll('path').length !== 2 || size.width !== 19 || size.height !== 19
    || glyph.getAttribute('stroke-width') !== '2' || color !== expectedColor) {
    throw new Error(`Home must draw the pinned Lucide Home/House glyph: ${JSON.stringify({ width: size.width, height: size.height, color, expectedColor })}`);
  }
};
export const Signal: Story = {
  args: { label: 'main', items: SIGNAL_ITEMS('home'), onSelect: () => {} }, globals: { theme: 'signal' },
  render: args => <div style={{ maxWidth: 390 }}><TabBar {...args} /></div>,
  play: ({ canvasElement }) => checkHomeGlyph(canvasElement, true),
};
export const SignalInactiveHome: Story = {
  args: { label: 'main', items: SIGNAL_ITEMS('activity'), onSelect: () => {} }, globals: { theme: 'signal' },
  render: args => <div style={{ maxWidth: 390 }}><TabBar {...args} /></div>,
  play: ({ canvasElement }) => checkHomeGlyph(canvasElement, false),
};
