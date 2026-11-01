import type { Meta, StoryObj } from '@storybook/react-vite';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';
import { CommunityRail } from './CommunityRail.tsx';

const ITEMS = [{ id: 'a1', name: 'Midgard Crew' }, { id: 'b2', name: 'Night Owls', unread: 4 }, { id: 'c3', name: 'raid planning', unread: 120, mentions: 2 }, { id: 'd4', name: 'Quiet Corner' }];
const HOME = { label: 'direct messages', selected: false, unread: 0, mentions: 0, itemLabel: 'direct messages, unread', onSelect: () => {} };

/** The rail is --rail-w (56px) wide; the story gives it the shell's column. */
const meta = {
  title: 'Shell/CommunityRail', component: CommunityRail,
  args: { label: 'servers', items: [{ id: 'a1', name: 'Midgard Crew' }, { id: 'b2', name: 'Night Owls' }, { id: 'c3', name: 'raid planning' }],
    activeId: 'a1', onSelect: () => {}, addLabel: 'add a server', onAdd: () => {} },
  decorators: [Story => <div style={{ width: 56, height: 420 }}><Story /></div>],
} satisfies Meta<typeof CommunityRail>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Default: Story = {};
export const NoneActive: Story = { args: { activeId: null } };
export const Empty: Story = { args: { items: [], activeId: null } };
export const Badges: Story = { args: { items: ITEMS } };
export const WithSettings: Story = { args: { settingsLabel: 'settings', onSettings: () => {} } };
export const FreshInstance: Story = { args: { items: [], activeId: null, settingsLabel: 'settings', onSettings: () => {} } };

// Signal (§5.3): the `@` tile first, a hairline, the servers, one dashed "+" circle; a mention count at a tile's bottom right.
const FULL = { items: ITEMS, activeId: 'a1', home: HOME, addLabel: 'add a server', onAdd: () => {}, settingsLabel: 'settings', onSettings: () => {} };
const rgb = (c: string): [number, number, number] => { const m = /rgba?\(([\d.]+)[, ]+([\d.]+)[, ]+([\d.]+)/.exec(c); if (m === null) throw new Error(`cannot read colour ${c}`); return [Number(m[1]), Number(m[2]), Number(m[3])]; };
const lum = ([r, g, b]: [number, number, number]) => { const f = (v: number) => { const x = v / 255; return x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b); };
/** Unread shows no visible count; a mention is a count at the tile's bottom right, readable at 3:1 or better. */
const badgePlay = (story: string) => ({ canvasElement }: { canvasElement: HTMLElement }) => {
  for (const sr of Array.from(canvasElement.querySelectorAll<HTMLElement>('.d-sr-only'))) {
    const r = sr.getBoundingClientRect(); if (r.width > 1 || r.height > 1) throw new Error(`${story}: an unread count is visible`);
  }
  const mention = canvasElement.querySelector<HTMLElement>('.d-community-rail__badge .d-pill');
  if (mention === null) return;
  const tile = mention.closest('.d-community-rail__item') as HTMLElement;
  const t = tile.getBoundingClientRect(); const m = mention.getBoundingClientRect();
  if (m.left + m.width / 2 < t.left + t.width / 2 || m.top + m.height / 2 < t.top + t.height / 2) throw new Error(`${story}: the mention badge is not at the bottom right`);
  const st = getComputedStyle(mention);
  const a = lum(rgb(st.color)); const b = lum(rgb(st.backgroundColor));
  const ratio = (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
  if (ratio < 3) throw new Error(`${story}: the mention count is ${ratio.toFixed(2)}:1`);
};
/** A server selected (the accent square with its ring), unread servers, and the `@` tile quiet. */
export const WithHome: Story = { args: FULL, globals: { theme: 'signal' }, play: badgePlay('WithHome') };
export const WithHomeLight: Story = { args: FULL, globals: { theme: 'light' }, play: badgePlay('WithHomeLight') };
export const WithHomeHighContrast: Story = { args: FULL, globals: { theme: 'high-contrast' }, play: badgePlay('WithHomeHighContrast') };
/** Home selected (DMs open) with unread messages and a mention (the count at the bottom right). */
export const HomeSelected: Story = { args: { ...FULL, activeId: null, home: { ...HOME, selected: true, unread: 3, mentions: 1, itemLabel: 'direct messages, unread 3, mentions 1' } }, globals: { theme: 'signal' }, play: badgePlay('HomeSelected') };
export const HomeUnread: Story = { args: { ...FULL, home: { ...HOME, unread: 5, itemLabel: 'direct messages, unread 5' } }, globals: { theme: 'signal' }, play: badgePlay('HomeUnread') };
/** On a phone the settings tile is gone (the You tab is the way) and the tiles are the prototype's 38 (Jonas 2026-10-27, superseding Ruling N). */
export const OnPhone: Story = { args: FULL, globals: { theme: 'signal' },
  decorators: [Story => <PhoneLayoutProvider value><div data-layout="phone" data-phone style={{ width: 56, height: 'calc(100vh - 32px)' }}><Story /></div></PhoneLayoutProvider>],
  play: ({ canvasElement }) => {
    const tiles = Array.from(canvasElement.querySelectorAll<HTMLElement>('.d-community-rail__item'));
    for (const t of tiles) { const b = t.getBoundingClientRect(); if (b.width !== 38 || b.height !== 38) throw new Error(`OnPhone: a tile is ${String(b.width)} x ${String(b.height)}`); }
    if (tiles.length === 0 || canvasElement.querySelector('.d-community-rail__settings') !== null) throw new Error('OnPhone: no tiles, or the settings tile shows');
  } };

/** Signal parity (README §5.3, the prototype's rail): `@` at rest, a server selected, one with a mention, the "+" circle. */
export const Signal: Story = {
  args: { items: [{ id: 'mc', name: 'Midgard Crew' }, { id: 'vk', name: 'Viking Kin', unread: 3, mentions: 2 }], activeId: 'mc', home: HOME, addLabel: 'add a server', onAdd: () => {} },
  globals: { theme: 'signal' }, play: badgePlay('Signal'),
};
/** The `@` tile selected: the selected fill and the accent glyph. */
export const SignalHomeSelected: Story = {
  args: { items: [{ id: 'mc', name: 'Midgard Crew' }, { id: 'vk', name: 'Viking Kin', unread: 3, mentions: 2 }], activeId: null, home: { ...HOME, selected: true }, addLabel: 'add a server', onAdd: () => {} },
  globals: { theme: 'signal' },
};
