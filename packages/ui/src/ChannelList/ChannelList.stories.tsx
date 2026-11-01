import type { Meta, StoryObj } from '@storybook/react-vite';
import { ChannelList } from './ChannelList.tsx';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';
import { Button } from '../Button/Button.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { expectGlyphLine } from '../VoiceChannelRow/VoiceChannelRow.stories.tsx';
import type { VoiceParticipantView } from '../CallTile/CallTile.tsx';
import { ActionMenu } from '../ActionMenu/ActionMenu.tsx';
import { useState } from 'react';

/**
 * Signal task 10 fix round 2–3 (rule 3d): the server head's ⌄ as packages/web/src/screens/Shell.tsx composes it, a menu named
 * "server settings" with the server's settings, Mute server and its notify mode.
 */
function ServerHeadMenu() {
  const [open, setOpen] = useState(false);
  return <ActionMenu label="server settings" open={open} onOpenChange={setOpen} triggerClassName="d-channel-list__head-btn"
    triggerContent={<Icon name="chevron-down" size={15} />} onSelect={() => setOpen(false)}
    items={[{ id: 'settings', label: 'Server settings' }, { id: 'mute', label: 'Mute server', check: false },
      { id: 'mode:default', label: 'Notify: default', checked: true, group: 'Notifications' }, { id: 'mode:all', label: 'Notify: all messages', checked: false, group: 'Notifications' },
      { id: 'mode:mentions', label: 'Notify: mentions only', checked: false, group: 'Notifications' }, { id: 'mode:nothing', label: 'Notify: nothing', checked: false, group: 'Notifications' }]} />;
}

const meta = {
  title: 'Shell/ChannelList', component: ChannelList, excludeStories: ['expectOneGrid'],
  args: { label: 'channels', title: 'Midgard Crew', activeId: 'g', onSelect: () => {}, emptyLabel: 'No channels yet.',
    channels: [{ id: 'g', name: 'general', kind: 'text', readable: false }, { id: 't', name: 'loot', kind: 'text', readable: false }] },
  decorators: [Story => <div style={{ width: 240, height: 360 }}><Story /></div>],
} satisfies Meta<typeof ChannelList>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Default: Story = {};
export const WithVoiceAndReadable: Story = { args: { channels: [
  { id: 'g', name: 'general', kind: 'text', readable: false }, { id: 'l', name: 'lfg', kind: 'text', readable: true },
  { id: 't', name: 'loot', kind: 'text', readable: false }, { id: 'v', name: 'longhouse', kind: 'voice', readable: false },
] } };
export const Empty: Story = { args: { channels: [], activeId: null } };

// L-COPY-02 shell.channels.rowLabel and rowLabelMuted, filled in as en.ts's t() will; shell.dms.new.
const ROW_LABEL = (c: { name: string; unread: number; mentions: number; muted: boolean }) =>
  c.muted ? `${c.name}, muted, mentions ${c.mentions}` : `${c.name}, unread ${c.unread}, mentions ${c.mentions}`;
const BADGED = [
  { id: 'g', name: 'general', kind: 'text', readable: false },
  { id: 'l', name: 'lfg', kind: 'text', readable: true, unread: 3 },
  { id: 't', name: 'loot', kind: 'text', readable: false, unread: 150 },
  { id: 's', name: 'screenshots', kind: 'text', readable: false, unread: 5, mentions: 2 },
  { id: 'r', name: 'random', kind: 'text', readable: false, unread: 7, mentions: 3, muted: true },
  { id: 'v', name: 'longhouse', kind: 'voice', readable: false },
] as const;
export const Badges: Story = { args: { activeId: 'g', rowLabel: ROW_LABEL, channels: BADGED } };
export const DirectMessages: Story = { args: { label: 'direct messages', activeId: 'd2', rowLabel: ROW_LABEL, channels: [
  { id: 'd1', name: 'ada', kind: 'dm', readable: false, unread: 2 },
  { id: 'd2', name: 'björn', kind: 'dm', readable: false },
  { id: 'd3', name: 'mira, ada', kind: 'dm', readable: false, unread: 4, mentions: 2 },
], footer: <Button variant="ghost" size="sm">message someone</Button> } };
// Flow 07: the owner's server actions under the name (a member who does not own the server sees only the invite).
const ACTIONS = <>
  <Button aria-label="new channel"><span aria-hidden="true">+</span> channel</Button>
  <Button>invite link</Button>
</>;
// Flow 07 (whole-branch walk): in the 240 px sidebar both actions sit on one row, each at least 24 px tall and wide; a
// long channel name is cut inside its row and never widens the list.
export const WithActions: Story = { args: { activeId: 'g', rowLabel: ROW_LABEL, actions: ACTIONS, actionsLabel: 'server actions',
  channels: [...BADGED, { id: 'f', name: 'fika-på-fredag-och-veckans-planering', kind: 'text', readable: false },
    { id: 'm', name: 'midsommar-'.repeat(10), kind: 'voice', readable: false }] },
  play: ({ canvasElement }) => {
    const buttons = Array.from(canvasElement.querySelectorAll<HTMLElement>('.d-channel-list__actions .d-btn')).map(b => b.getBoundingClientRect());
    if (buttons.length !== 2 || buttons[0].top !== buttons[1].top) {
      throw new Error(`WithActions: the server actions wrap (tops ${buttons.map(b => String(b.top)).join(', ')})`);
    }
    for (const b of buttons) if (b.height < 24 || b.width < 24) throw new Error(`WithActions: an action is ${String(b.width)} × ${String(b.height)}`);
    const list = canvasElement.querySelector<HTMLElement>('.d-channel-list');
    if (list === null || list.scrollWidth > list.clientWidth + 1) throw new Error(`WithActions: the list is ${String(list?.scrollWidth)} wide`);
  } };
export const AnnouncementSlow: Story = { args: { activeId: 'g', channels: [
  { id: 'n', name: 'news', kind: 'announcement', readable: false, unread: 2 },
  { id: 'g', name: 'general', kind: 'text', readable: false, badge: 'slow mode' },
  { id: 's', name: 'announcements-and-everything-else', kind: 'announcement', readable: false, badge: 'slow mode' },
  { id: 'v', name: 'longhouse', kind: 'voice', readable: false },
] } };

// web3.8 (U3): one list, the voice channels in place with their people under them, a "+" in the list's header row, and
// the join button on hover or focus. The strings are the shell's, quoted.
const person = (name: string, more: Partial<VoiceParticipantView> = {}): VoiceParticipantView => ({
  deviceId: `d-${name}`, userId: `u-${name}`, name, state: 'ok', speaking: false, muted: false, deafened: false,
  video: false, screen: false, web: true, bot: false, ...more,
});
const VOICE = {
  labels: {
    open: 'voice {name}', count: '{n} in voice', playbackOnly: 'playback only', join: 'join voice', joined: 'connected',
    state: { speaking: 'speaking', muted: 'muted', deafened: 'deafened', notHeard: 'not heard yet', unverified: 'unverified stream',
      connecting: 'connecting', video: 'camera on', screen: 'sharing screen', web: 'web', bot: 'bot', noSound: 'no sound' },
  },
  rooms: {
    v1: { joined: false, participants: [person('ada', { speaking: true }), person('bo', { muted: true, video: true }), person('cy', { state: 'unverified' })] },
    v2: { joined: true, participants: [person('you', { deafened: true })] },
  },
  onJoin: () => {},
};
const ONE_LIST = [
  { id: 'g', name: 'general', kind: 'text', readable: false, unread: 3 },
  { id: 'v1', name: 'longhouse', kind: 'voice', readable: false },
  { id: 'n', name: 'news', kind: 'announcement', readable: false },
  { id: 'v2', name: 'workshop', kind: 'voice', readable: false },
  { id: 'l', name: 'lfg', kind: 'text', readable: true },
] as const;
const ONE_LIST_ARGS = {
  activeId: 'g', rowLabel: ROW_LABEL, channels: ONE_LIST, voice: VOICE,
};
const oneListPlay = ({ canvasElement }: { canvasElement: HTMLElement }) => {
  expectGlyphLine(canvasElement, 'OneList');
  const people = canvasElement.querySelectorAll('.d-voice-row__person');
  if (people.length !== 4) throw new Error(`OneList: ${String(people.length)} nested people, not 4`);
};
export const OneList: Story = { args: ONE_LIST_ARGS, globals: { theme: 'signal' }, play: oneListPlay };
export const OneListLight: Story = { args: ONE_LIST_ARGS, globals: { theme: 'light' }, play: oneListPlay };
export const OneListHighContrast: Story = { args: ONE_LIST_ARGS, globals: { theme: 'high-contrast' }, play: oneListPlay };

// web3.9 (UI-2): the row grammar. One name x and one trailing-slot right edge down the whole list.
const MIXED_VOICE = {
  ...VOICE,
  rooms: {
    v1: { joined: false, participants: [person('ada', { speaking: true }), person('bo', { muted: true, deafened: true }), person('cy')] },
    v2: { joined: true, participants: [person('you')] },
  },
};
const MIXED = [
  { id: 'g', name: 'general', kind: 'text', readable: false },
  { id: 'n', name: 'news', kind: 'announcement', readable: false, unread: 2 },
  { id: 'p', name: 'staff', kind: 'text', readable: false, mentions: 3, unread: 5 },
  { id: 'v1', name: 'harbour', kind: 'voice', readable: false, parentId: 'lounge' },
  { id: 'v2', name: 'workshop', kind: 'voice', readable: false, parentId: 'lounge', joined: true },
  { id: 'l', name: 'lfg', kind: 'text', readable: true, parentId: 'lounge' },
  { id: 'r1', name: 'raid-plans-with-a-rather-long-name-indeed', kind: 'text', readable: false, parentId: 'raids', unread: 120 },
  { id: 'afk', name: 'AFK', kind: 'voice', readable: false, afk: true, parentId: 'lounge' },
] as const;
const CATEGORIES = [{ id: 'lounge', name: 'lounge', collapsed: false }, { id: 'raids', name: 'raids', collapsed: false }];
const MIXED_ARGS = {
  activeId: 'g', rowLabel: ROW_LABEL, channels: MIXED, categories: CATEGORIES, voice: MIXED_VOICE,
  onToggleCategory: () => {}, onAddInCategory: () => {}, addInCategoryLabel: (n: string) => `new channel in ${n}`,
  headerAction: <Button variant="ghost" size="sm" aria-label="server menu">⋯</Button>,
};

/**
 * Every row name (channel, voice) starts at the same x (+-0.5px), a person's avatar too, and every trailing slot ends at the
 * same x. Signal §5.4: the category label sits after its 11px chevron (8 + 11 + 6), so it is not on the names' x.
 */
export function expectOneGrid(root: HTMLElement, story: string): void {
  const lefts = Array.from(root.querySelectorAll<HTMLElement>('.d-chrow__name, .d-voice-row__name, .d-voice-row__avatar'))
    .map(el => el.getBoundingClientRect().left);
  const rights = Array.from(root.querySelectorAll<HTMLElement>('.d-chrow .d-slot, .d-voice-row > .d-slot, .d-voice-row__person .d-slot'))
    .map(el => el.getBoundingClientRect().right);
  if (lefts.length < 8 || rights.length < 6) throw new Error(`${story}: too few rows measured (${String(lefts.length)}, ${String(rights.length)})`);
  const off = (xs: number[]) => Math.max(...xs) - Math.min(...xs);
  if (off(lefts) > 0.5) throw new Error(`${story}: the names start ${String(off(lefts))}px apart (${lefts.join(', ')})`);
  if (off(rights) > 0.5) throw new Error(`${story}: the slots end ${String(off(rights))}px apart (${rights.join(', ')})`);
  const list = root.querySelector<HTMLElement>('.d-channel-list__list');
  if (list !== null && list.scrollWidth > list.clientWidth + 1) throw new Error(`${story}: the list overflows sideways`);
}
const mixedPlay = ({ canvasElement }: { canvasElement: HTMLElement }) => { expectOneGrid(canvasElement, 'Mixed'); };
// The phone layout the way the app sets it: the phone context and the layout hook that gives the rows their 44px (base.css),
// filling the 390 x 844 viewport.
const tall = (width: number, phone = false) => (Story: () => React.JSX.Element) => phone ? (
  <PhoneLayoutProvider value><div data-layout="phone" data-phone style={{ width: '100%', height: 'calc(100vh - 32px)', maxWidth: 480 }}><Story /></div></PhoneLayoutProvider>
) : <div style={{ width, height: 640 }}><Story /></div>;
/** On a phone every channel row is at least 44px tall. */
const phonePlay = (story: string) => ({ canvasElement }: { canvasElement: HTMLElement }) => {
  const rows = Array.from(canvasElement.querySelectorAll<HTMLElement>('.d-chrow, .d-voice-row'));
  if (rows.length === 0) throw new Error(`${story}: no rows`);
  for (const r of rows) if (r.getBoundingClientRect().height < 44) throw new Error(`${story}: a row is ${String(r.getBoundingClientRect().height)} tall`);
  // Fix round 2 (rule 0): a category head keeps the prototype's 26px on a phone.
  for (const h of canvasElement.querySelectorAll<HTMLElement>('.d-channel-list__cat-head')) {
    if (Math.abs(h.getBoundingClientRect().height - 26) > 0.5) throw new Error(`${story}: a category head is ${String(h.getBoundingClientRect().height)} tall`);
  }
};

export const Mixed: Story = { args: MIXED_ARGS, globals: { theme: 'signal' }, decorators: [tall(240)], play: mixedPlay };
export const MixedLight: Story = { args: MIXED_ARGS, globals: { theme: 'light' }, decorators: [tall(240)], play: mixedPlay };
export const MixedHighContrast: Story = { args: MIXED_ARGS, globals: { theme: 'high-contrast' }, decorators: [tall(240)], play: mixedPlay };
export const MixedPhone: Story = { args: MIXED_ARGS, globals: { theme: 'signal' }, decorators: [tall(360, true)], play: ({ canvasElement }) => { mixedPlay({ canvasElement }); phonePlay('MixedPhone')({ canvasElement }); } };

const COLLAPSED_ARGS = { ...MIXED_ARGS, categories: [{ id: 'lounge', name: 'lounge', collapsed: true }, { id: 'raids', name: 'raids', collapsed: true }] };
/** Both collapsed: each keeps the rows that are current, unread, mentioned or joined (R23). */
export const Collapsed: Story = { args: COLLAPSED_ARGS, globals: { theme: 'signal' }, decorators: [tall(240)] };
export const CollapsedLight: Story = { args: COLLAPSED_ARGS, globals: { theme: 'light' }, decorators: [tall(240)] };
export const CollapsedHighContrast: Story = { args: COLLAPSED_ARGS, globals: { theme: 'high-contrast' }, decorators: [tall(240)] };

const UNREAD_ARGS = { activeId: 'm', rowLabel: ROW_LABEL, channels: [
  { id: 'q', name: 'quiet', kind: 'text', readable: false },
  { id: 'u', name: 'unread', kind: 'text', readable: false, unread: 4 },
  { id: 'm', name: 'selected', kind: 'text', readable: false, unread: 2 },
  { id: 'x', name: 'mentioned', kind: 'text', readable: false, unread: 9, mentions: 2 },
  { id: 'mu', name: 'muted-with-mention', kind: 'text', readable: false, unread: 9, mentions: 1, muted: true },
  { id: 'ms', name: 'muted', kind: 'text', readable: false, unread: 9, muted: true },
  { id: 'vv', name: 'private-voice', kind: 'voice', readable: false },
] as const };
/** The selected mark (fill and 2px bar), the unread mark (bright name and 4px tick), the mention pill over the unread pill. */
export const UnreadAndMention: Story = { args: UNREAD_ARGS, globals: { theme: 'signal' } };
export const UnreadAndMentionLight: Story = { args: UNREAD_ARGS, globals: { theme: 'light' } };
export const UnreadAndMentionHighContrast: Story = { args: UNREAD_ARGS, globals: { theme: 'high-contrast' } };
export const UnreadAndMentionPhone: Story = { args: UNREAD_ARGS, globals: { theme: 'signal' }, decorators: [tall(360, true)], play: phonePlay('UnreadAndMentionPhone') };

// Signal parity (README §5.4): the prototype's Midgard Crew sidebar at its first paint.
const SIGNAL_VOICE = {
  ...VOICE,
  rooms: {
    hv: { joined: false, participants: [] },
    harbour: { joined: false, participants: [person('Ada', { speaking: true, screen: true }), person('Björn', { video: true }), person('Kari', { video: true, screen: true }), person('Ole'), person('Glenn', { bot: true, web: false })] },
    afk: { joined: false, participants: [] },
  },
};
const SIGNAL_ARGS = {
  title: 'Midgard Crew', stats: [{ text: '3 online', live: true }, { text: '5 in voice' }], activeId: 'general', newLabel: (n: string) => `${n} new`, voice: SIGNAL_VOICE,
  categories: [{ id: 'raids', name: 'raids', collapsed: false }, { id: 'boats', name: 'boats', collapsed: false }], onToggleCategory: () => {},
  // The head the web passes (packages/web/src/screens/Shell.tsx headerAction, Ruling AB): the 26px ⌄, the server's menu.
  headerAction: <ServerHeadMenu />,
  channels: [
    { id: 'general', name: 'general', kind: 'text', readable: false },
    { id: 'hv', name: 'general', kind: 'voice', readable: false },
    { id: 'loot', name: 'loot', kind: 'text', readable: false, parentId: 'raids', unread: 1 },
    { id: 'harbour', name: 'harbour', kind: 'voice', readable: false, parentId: 'boats' },
    { id: 'afk', name: 'AFK', kind: 'voice', readable: false, afk: true },
  ],
} as const;
export const Signal: Story = { args: SIGNAL_ARGS, globals: { theme: 'signal' }, decorators: [tall(244)] };
