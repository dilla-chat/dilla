import { useEffect, useState } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { MemberList } from '../MemberList/MemberList.tsx';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';
import { ProfileCard, type ProfileCardAction, type ProfileCardProps } from './ProfileCard.tsx';

const noop = () => {};
// As MemberCardHost passes them (task 9): Message (primary), the safety-number shield, and Edit roles with manageRoles.
const message: ProfileCardAction = { id: 'message', label: 'Message', icon: 'message-square', primary: true };
const compare: ProfileCardAction = { id: 'compare', label: 'compare safety numbers', icon: 'shield' };
const roles: ProfileCardAction = { id: 'roles', label: 'edit roles', icon: 'square-pen' };
const groups = [
  { id: 'raid', kind: 'role' as const, title: 'raid lead', color: 0xe9b6ec, members: [
    { id: 'u1', name: 'Ada', isOwner: true, color: 0xe9b6ec, presence: 'online' as const, status: 'planning the raid' },
    { id: 'u5', name: 'Mira', color: 0xe9b6ec, presence: 'online' as const, status: 'online' },
  ] },
  { id: 'members', kind: 'online' as const, title: 'online', members: [
    { id: 'u2', name: 'Björn', presence: 'online' as const, status: 'bridge troll survivor' },
    { id: 'u3', name: 'Kari', presence: 'online' as const, status: 'streaming' },
  ] },
  { id: 'bots', kind: 'bots' as const, title: 'bots', members: [{ id: 'g1', name: 'Glenn', isBot: true, presence: 'online' as const, status: 'in voice' }] },
];

/** The member list on the right of the screen with the card open beside a row, as the shell mounts it. */
function Beside(props: Omit<ProfileCardProps, 'anchor'> & { row: string }) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  useEffect(() => { setAnchor(document.querySelector<HTMLElement>(`[data-focus-key="member:${props.row}"]`)); }, [props.row]);
  return (
    <div style={{ display: 'flex', justifyContent: 'flex-end', minHeight: '480px' }}>
      <div style={{ width: '232px', background: 'var(--bg-2)' }}><MemberList label="Members" closeLabel="close members" onClose={noop} groups={groups} loading={false}
        loadingLabel="Loading members…" emptyLabel="No members yet." onOpen={noop} openId={props.row} /></div>
      {anchor !== null ? <ProfileCard {...props} anchor={anchor} /> : null}
    </div>
  );
}

const meta = {
  title: 'Shell/ProfileCard', component: ProfileCard,
  args: {
    name: 'Björn', username: 'bjorn', presence: 'online', client: 'WEB', status: 'bridge troll survivor', bio: 'Carries shields, loses them.',
    roles: [{ name: 'DJ', color: 0xffd16a }, { name: '@everyone', color: null }], actions: [message, compare, roles], onAction: noop, onClose: noop,
  },
} satisfies Meta<typeof ProfileCard>;
export default meta;
type Story = StoryObj<typeof meta>;

/** Waits (up to 3s) for the card to be mounted and placed, never a fixed delay. */
async function settled(root: HTMLElement): Promise<void> {
  for (let i = 0; i < 60; i++) {
    const card = root.querySelector<HTMLElement>('.d-profile-card');
    if (card !== null && (card.dataset.layout === 'sheet' || card.style.left !== '')) {
      // The sheet rises in (d-sheet-up): measure where it comes to rest.
      await Promise.all(document.getAnimations().filter(a => a.effect?.getTiming().iterations !== Infinity).map(a => a.finished.catch(() => undefined)));
      await new Promise(r => setTimeout(r, 30));
      return;
    }
    await new Promise(r => setTimeout(r, 50));
  }
}
/** Task 9 (Signal §5.7): a 300px popover beside the row, the 56px band and avatar, 34px actions, focus inside. */
async function expectPopover(root: HTMLElement, story: string, row: string): Promise<void> {
  await settled(root);
  const card = root.querySelector<HTMLElement>('.d-profile-card');
  const anchor = root.querySelector<HTMLElement>(`[data-focus-key="member:${row}"]`);
  if (card === null || anchor === null) throw new Error(`${story}: no card or no row`);
  const c = card.getBoundingClientRect(); const r = anchor.getBoundingClientRect();
  // Layout sizes (offset*), not the box: the d-modal entrance may still be scaling it.
  if (Math.abs(card.offsetWidth - 302) > 0.5) throw new Error(`${story}: the card is ${String(card.offsetWidth)} wide, not 300 + its 1px edges`);
  if (c.right > r.left + 0.5) throw new Error(`${story}: the card (right ${String(c.right)}) overlaps the row (left ${String(r.left)})`);
  if (c.left < 0 || c.top < 0 || c.bottom > window.innerHeight + 0.5) throw new Error(`${story}: the card is outside the viewport`);
  const band = card.querySelector<HTMLElement>('.d-profile-card__band')!.offsetHeight;
  if (Math.abs(band - 56) > 0.5) throw new Error(`${story}: the band is ${String(band)} high, not 56`);
  const avatar = card.querySelector<HTMLElement>('.d-avatar')!.offsetWidth;
  if (Math.abs(avatar - 56) > 0.5) throw new Error(`${story}: the avatar is ${String(avatar)} wide, not 56`);
  for (const b of Array.from(card.querySelectorAll<HTMLElement>('.d-profile-card__action, .d-profile-card__icon-action'))) {
    if (Math.abs(b.offsetHeight - 34) > 0.5) throw new Error(`${story}: an action is ${String(b.offsetHeight)} high, not 34`);
  }
  if (!card.contains(document.activeElement)) throw new Error(`${story}: focus is not inside the card`);
  if (card.getAttribute('role') !== 'dialog') throw new Error(`${story}: not a dialog`);
}
/** The phone sheet: pinned to the bottom of the screen, full width, 16px top corners, focus inside. */
async function expectSheet(root: HTMLElement, story: string): Promise<void> {
  await settled(root);
  const card = root.querySelector<HTMLElement>('.d-profile-card');
  if (card === null) throw new Error(`${story}: no card`);
  const c = card.getBoundingClientRect();
  if (Math.abs(c.bottom - window.innerHeight) > 0.5) throw new Error(`${story}: the sheet's bottom is ${String(c.bottom)}, not the screen's ${String(window.innerHeight)}`);
  if (Math.abs(c.width - window.innerWidth) > 0.5) throw new Error(`${story}: the sheet is ${String(c.width)} wide, not full width`);
  if (getComputedStyle(card).borderTopLeftRadius !== '16px' || getComputedStyle(card).borderBottomLeftRadius !== '0px') throw new Error(`${story}: the sheet's corners are not 16px 16px 0 0`);
  if (!card.contains(document.activeElement)) throw new Error(`${story}: focus is not inside the card`);
}

const beside = (row: string): Story => ({
  render: args => <Beside {...args} row={row} />, parameters: { layout: 'fullscreen' }, globals: { theme: 'signal' },
  play: ({ canvasElement }) => expectPopover(canvasElement, `ProfileCard ${row}`, row),
});
/** A member, as a viewer with Manage roles sees them (the prototype's Mira): Message, the shield and Edit roles. */
export const Person: Story = { ...beside('u2') };
/** The owner, with Manage roles: the crown after the name, Message, the shield and Edit roles. */
export const Owner: Story = {
  ...beside('u1'),
  args: { name: 'Ada', username: 'ada', status: 'planning the raid', isOwner: true, bio: 'Raid lead. Has the map. Will not explain the stain.',
    roles: [{ name: 'raid lead', color: 0xe9b6ec }, { name: '@everyone', color: null }], actions: [message, compare, roles] },
};
/** A bot: the rounded-square avatar. */
export const Bot: Story = {
  ...beside('g1'),
  args: { name: 'Glenn', username: 'glenn', status: undefined, presence: 'idle', isBot: true, client: undefined, bio: 'Managed bot. Music, polls, reminders and moderation for this server.',
    roles: [{ name: '@everyone', color: null }], actions: [message, compare, roles] },
};
/** Your own card: no actions (the prototype's). */
export const You: Story = {
  ...beside('u5'),
  args: { name: 'Mira', username: 'mira', status: undefined, presence: 'online', isYou: true, bio: '', roles: [{ name: 'raid lead', color: 0xe9b6ec }, { name: '@everyone', color: null }], actions: [] },
};
export const PersonLight: Story = { ...Person, globals: { theme: 'light' } };
export const PersonHighContrast: Story = { ...Person, globals: { theme: 'high-contrast' } };

export const PhoneSheet: Story = {
  render: args => <PhoneLayoutProvider value><ProfileCard {...args} /></PhoneLayoutProvider>,
  parameters: { layout: 'fullscreen' }, globals: { theme: 'signal' },
  play: ({ canvasElement }) => expectSheet(canvasElement, 'ProfileCard sheet'),
};
export const PhoneSheetLight: Story = { ...PhoneSheet, globals: { theme: 'light' } };
export const PhoneSheetHighContrast: Story = { ...PhoneSheet, globals: { theme: 'high-contrast' } };
