import type { Meta, StoryObj } from '@storybook/react-vite';
import { GifPicker } from './GifPicker.tsx';

// Previews are page-minted URLs in the app; the stories use inline SVG data URLs so they need no network.
const swatch = (hue: number, w: number, h: number): string =>
  `data:image/svg+xml;utf8,${encodeURIComponent(`<svg xmlns="http://www.w3.org/2000/svg" width="${w}" height="${h}"><rect width="${w}" height="${h}" fill="hsl(${hue} 40% 40%)"/></svg>`)}`;
// Giphy's fixed-width previews are 200 px wide: wide, tall and square ones side by side, none cropped.
const SIZES: readonly (readonly [number, number])[] = [[200, 113], [200, 356], [200, 200], [200, 80], [200, 150], [200, 267], [200, 113]];
const ITEMS = ['cat typing', 'dog', 'fish', '', 'bird', 'otter', 'owl'].map((title, i) => ({
  id: `g${i}`, title, width: SIZES[i][0], height: SIZES[i][1], src: i === 3 ? null : swatch(i * 55, SIZES[i][0], SIZES[i][1]),
}));

const meta = {
  title: 'Conversation/GifPicker', component: GifPicker,
  args: {
    label: 'gifs', searchLabel: 'search gifs', searchPlaceholder: 'Search Giphy', query: '', onQuery: () => {}, items: ITEMS,
    status: null, poweredBy: 'VIA GIPHY', untitled: 'gif', moreLabel: 'more', hasMore: true, onMore: () => {},
    pickingId: null, onPick: () => {}, onClose: () => {},
  },
  decorators: [Story => <div style={{ maxWidth: 420 }}><Story /></div>],
} satisfies Meta<typeof GifPicker>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Results: Story = {};
export const Empty: Story = { args: { items: [], status: { kind: 'empty', text: 'no gifs for that' }, hasMore: false, query: 'zzzz' } };
export const Failed: Story = { args: { items: [], status: { kind: 'error', text: 'could not reach the gif service' }, hasMore: false } };
export const Picking: Story = { args: { pickingId: 'g1', status: { kind: 'busy', text: 'adding the gif' } } };
// A phone (390 px): two columns, the same proportions, each result at least 44 px tall.
export const Phone: Story = {
  decorators: [Story => <div data-layout="phone" style={{ width: 358 }}><Story /></div>],
};
