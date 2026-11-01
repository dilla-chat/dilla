import type { Meta, StoryObj } from '@storybook/react-vite';
import { MemberList } from './MemberList.tsx';

const noop = () => {};
const meta = {
  title: 'Shell/MemberList', component: MemberList,
  args: {
    label: 'Members', closeLabel: 'close members', onClose: noop, loading: false, loadingLabel: 'Loading members…', emptyLabel: 'No members yet.',
    onOpen: noop, onMenu: noop,
    groups: [
      { id: 'mods', kind: 'role', title: 'moderators', color: 0xe74c3c, members: [{ id: 'u1', name: 'Astrid', isOwner: true, color: 0xe74c3c, presence: 'online', status: 'writing the release notes' }] },
      { id: 'online', kind: 'online', title: 'online', members: [
        { id: 'u2', name: 'Bjorn', color: 0x3498db, presence: 'online', status: 'online' },
        { id: 'u3', name: 'Sigrid', presence: 'idle', status: 'idle' },
        { id: 'u4', name: 'Maximilian-Alexander von Hohenzollern-Sigmaringen the Third', color: 0xf1c40f, presence: 'dnd', status: 'do not disturb' },
        { id: 'u7', name: 'Dag', status: 'in a meeting', presence: 'dnd' },
      ] },
      { id: 'bots', kind: 'bots', title: 'bots', members: [{ id: 'u8', name: 'Botty', isBot: true, presence: 'online', status: 'in voice' }] },
      { id: 'offline', kind: 'offline', title: 'offline', members: [
        { id: 'u5', name: 'Cleo', color: 0x9b59b6, presence: 'offline', status: 'offline' },
        { id: 'u6', name: 'Erik', presence: 'offline', status: 'offline' },
      ] },
    ],
  },
  decorators: [Story => <div style={{ width: '232px', minHeight: '14rem', background: 'var(--bg-2)' }}><Story /></div>],
} satisfies Meta<typeof MemberList>;
export default meta;
type Story = StoryObj<typeof meta>;

/**
 * Task 9 (Signal §5.7), the prototype's members aside: the 44px head with the close ×, group headings (10px uppercase, a 3×10 bar
 * on a coloured role), 44px rows with 6px corners, a 28px avatar (a bot's rounded square), the 10.5px status line.
 */
function expectMembers(root: HTMLElement, story: string): void {
  const bar = root.querySelector<HTMLElement>('.d-member-list__bar');
  if (bar === null) throw new Error(`${story}: no head`);
  if (Math.abs(bar.getBoundingClientRect().height - 44) > 0.5) throw new Error(`${story}: the head is ${String(bar.getBoundingClientRect().height)} high, not 44`);
  const close = bar.querySelector<HTMLElement>('.d-member-list__close');
  if (close === null || Math.abs(close.getBoundingClientRect().width - 32) > 0.5) throw new Error(`${story}: no 32px close ×`);
  const rows = Array.from(root.querySelectorAll<HTMLElement>('[data-row]'));
  if (rows.length === 0) throw new Error(`${story}: no rows`);
  for (const row of rows) {
    const h = row.getBoundingClientRect().height;
    if (Math.abs(h - 44) > 0.5) throw new Error(`${story}: a row is ${String(h)} high, not 44`);
    if (getComputedStyle(row).borderTopLeftRadius !== '6px') throw new Error(`${story}: a row's corners are ${getComputedStyle(row).borderTopLeftRadius}, not 6px`);
    const a = row.querySelector<HTMLElement>('.d-avatar')!.getBoundingClientRect();
    if (Math.abs(a.width - 28) > 0.5) throw new Error(`${story}: the avatar is ${String(a.width)} wide, not 28`);
    const line = row.querySelector<HTMLElement>('.d-member-list__line2');
    if (line === null || getComputedStyle(line).fontSize !== '10.5px') throw new Error(`${story}: a row has no 10.5px status line`);
  }
  const hs = getComputedStyle(root.querySelector<HTMLElement>('.d-member-list__title')!);
  if (hs.fontSize !== '10px' || hs.textTransform !== 'uppercase') throw new Error(`${story}: the group heading is ${hs.fontSize} ${hs.textTransform}`);
  for (const b of Array.from(root.querySelectorAll<HTMLElement>('.d-member-list__title-bar'))) {
    const r = b.getBoundingClientRect();
    if (Math.abs(r.width - 3) > 0.5 || Math.abs(r.height - 10) > 0.5) throw new Error(`${story}: a colour bar is ${String(r.width)}×${String(r.height)}, not 3×10`);
  }
  const bot = root.querySelector<HTMLElement>('[data-kind="bots"] .d-avatar');
  if (bot !== null && bot.dataset.shape !== 'bot') throw new Error(`${story}: a bot's avatar is not the rounded square`);
  if (root.querySelectorAll('[data-icon="crown"]').length !== 1) throw new Error(`${story}: expected exactly one crown`);
  const xs = new Set(rows.map(r => Math.round(r.querySelector<HTMLElement>('.d-member-list__name')!.getBoundingClientRect().left)));
  if (xs.size > 1) throw new Error(`${story}: the names start at ${[...xs].join(', ')}, not at one x`);
  const off = root.querySelector<HTMLElement>('[data-kind="offline"] [data-row] .d-member-list__line1');
  if (off !== null && getComputedStyle(off).opacity !== '0.8') throw new Error(`${story}: an offline row is not dimmed (${getComputedStyle(off).opacity})`);
  // I2: the focused row button draws its 2px ring inside its own box, so the aside's edge cannot clip it.
  const btn = rows[0]!.querySelector<HTMLElement>('.d-member-list__open')!;
  btn.focus({ focusVisible: true } as FocusOptions);
  if (getComputedStyle(btn).outlineOffset !== '-2px') throw new Error(`${story}: the row's focus ring is offset ${getComputedStyle(btn).outlineOffset}, not -2px (inside)`);
  btn.blur();
}
export const Default: Story = { play: ({ canvasElement }) => { expectMembers(canvasElement, 'MemberList'); } };
export const ThreeSections: Story = { ...Default, globals: { theme: 'light' } };
export const ThreeSectionsHighContrast: Story = { ...Default, globals: { theme: 'high-contrast' } };
/** The prototype's members aside (Midgard Crew): a coloured role group, the presence group and the bots group. */
export const Prototype: Story = {
  ...Default,
  args: {
    groups: [
      { id: 'raid', kind: 'role', title: 'raid lead', color: 0xe9b6ec, members: [
        { id: 'u1', name: 'Ada', isOwner: true, color: 0xe9b6ec, presence: 'online', status: 'planning the raid' },
        { id: 'u5', name: 'Mira', color: 0xe9b6ec, presence: 'online', status: 'online' },
      ] },
      { id: 'members', kind: 'online', title: 'online', members: [
        { id: 'u2', name: 'Björn', presence: 'online', status: 'bridge troll survivor' },
        { id: 'u3', name: 'Kari', presence: 'online', status: 'streaming' },
        { id: 'u4', name: 'Ole', presence: 'online', status: 'afk-ish' },
      ] },
      { id: 'bots', kind: 'bots', title: 'bots', members: [{ id: 'g1', name: 'Glenn', isBot: true, presence: 'online', status: 'in voice' }] },
    ],
  },
};
export const Loading: Story = { args: { loading: true } };
/** Inside a landmark that names the list itself (a phone's sheet): no head. */
export const NoBar: Story = { args: { label: undefined } };
export const Empty: Story = { args: { groups: [] } };
