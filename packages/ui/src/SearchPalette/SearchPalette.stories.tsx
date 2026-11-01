import type { Meta, StoryObj } from '@storybook/react-vite';
import { SearchPalette, type SearchPaletteGroup } from './SearchPalette.tsx';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';
const noop = () => {};
// As packages/web/src/screens/Search.tsx and palette.ts pass them (task 9 fix round 1).
const labels = { search: 'Search channels, people and messages', placeholder: 'Search channels, people and messages', empty: '', close: 'Close',
  hints: { move: 'move', open: 'open' }, note: 'Searches messages held in this browser' };
const channels: SearchPaletteGroup = { id: 'channels', label: 'Channels', items: [
  { id: 'ch:general', label: 'general', sub: 'Midgard Crew', icon: 'hash' },
  { id: 'ch:hv', label: 'general', sub: 'Midgard Crew · voice', icon: 'volume-2' },
  { id: 'ch:loot', label: 'loot', sub: 'Midgard Crew', icon: 'hash' },
  { id: 'ch:harbour', label: 'harbour', sub: 'Midgard Crew · voice', icon: 'volume-2' },
  { id: 'ch:vk', label: 'general', sub: 'Valhalla Kitchen', icon: 'hash' },
  { id: 'ch:recipes', label: 'recipes', sub: 'Valhalla Kitchen', icon: 'hash' },
  { id: 'ch:stove', label: 'stove', sub: 'Valhalla Kitchen · voice', icon: 'volume-2' },
] };
const people: SearchPaletteGroup = { id: 'people', label: 'People', items: [
  { id: 'dm:ada', label: 'Ada', sub: '@ada · planning the raid', avatar: { name: 'Ada' } },
  { id: 'dm:bjorn', label: 'Björn', sub: '@bjorn · bridge troll survivor', avatar: { name: 'Björn' } },
  { id: 'dm:crew', label: 'raid crew', sub: 'group DM · 3 people', avatar: { name: 'raid crew' } },
] };
const actions: SearchPaletteGroup = { id: 'actions', label: 'Actions', items: [
  { id: 'act:settings', label: 'Open settings', sub: 'Profile, devices, voice…', icon: 'settings' },
  { id: 'act:mute', label: 'Mute', sub: '⌘⇧M', icon: 'mic-off' },
  { id: 'act:dm', label: 'Message someone', sub: 'Start a direct message', icon: 'message-square' },
] };
const messages: SearchPaletteGroup = { id: 'messages', label: 'Messages', items: [
  { id: 'msg:1', label: 'bring a rope this time', sub: '#general · Ada · 20:31', icon: 'message-square' },
  { id: 'msg:2', label: 'two ropes', sub: '#general · Mira · 20:32', icon: 'message-square' },
  { id: 'msg:3', label: 'the rope is in the loot pile', sub: '#loot · Björn · Oct 19 09:41', icon: 'message-square' },
] };
const meta: Meta<typeof SearchPalette> = { title: 'Search/SearchPalette', component: SearchPalette };
export default meta;
type Story = StoryObj<typeof SearchPalette>;
const args = { open: true, query: 'rope', groups: [messages], labels, onQuery: noop, onPick: noop, onClose: noop, modal: true };
// The desktop palette over the app (Signal §5.8): the app stays visible behind the scrim.
const overShell = [(Story: () => React.JSX.Element) => (
  <div style={{ minHeight: '100vh', padding: '1rem', background: 'var(--bg)', color: 'var(--fg-2)', fontFamily: 'var(--font-mono)' }}>
    <p>general</p><p>loot</p><p>harbour</p>
    <Story />
  </div>
)];
/** Opened, nothing typed yet: the channels, the people and the actions, as the prototype. */
export const Empty: Story = { args: { ...args, query: '', groups: [channels, people, actions] }, parameters: { layout: 'fullscreen' }, decorators: overShell };
/** A query typed, the message search still answering: what matches by name already shows. */
export const Query: Story = { args: { ...args, query: 'ro', groups: [], labels: { ...labels, empty: 'Searching your messages' } }, parameters: { layout: 'fullscreen' }, decorators: overShell };
/** Message results: the group heading, the selected row with its accent bar and ↵, each with #place · author · time. */
export const MessagesResults: Story = { args, parameters: { layout: 'fullscreen' }, decorators: overShell };
/** Every group at once ("ra": a channel, a person, messages). */
export const AllGroups: Story = {
  args: { ...args, query: 'ra', groups: [{ ...channels, items: channels.items.slice(0, 1) }, { ...people, items: people.items.slice(2) }, messages, { ...actions, items: actions.items.slice(0, 1) }] },
  parameters: { layout: 'fullscreen' }, decorators: overShell,
};
/** Nothing matches. */
export const NoResults: Story = { args: { ...args, query: 'lantern', groups: [], labels: { ...labels, empty: 'Nothing in this browser matches “lantern”.' } }, parameters: { layout: 'fullscreen' }, decorators: overShell };
/** A phone: the inline palette (the web app's dialog around it puts it 12px from the top over the scrim). */
export const Phone: Story = {
  args: { ...args, modal: false },
  decorators: [Story => <PhoneLayoutProvider value><div style={{ width: '366px', padding: '12px', background: 'var(--scrim)' }}><Story /></div></PhoneLayoutProvider>],
};
