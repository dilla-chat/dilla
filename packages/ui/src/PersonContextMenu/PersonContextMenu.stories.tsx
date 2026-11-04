import type { Meta, StoryObj } from '@storybook/react-vite';
import { PersonContextMenu } from './PersonContextMenu.tsx';
import { tileMenu } from '../CallStage/stage.fixture.tsx';

// Fix round 1 (Jonas): the person menu of a voice occupant and a stage tile, as VoiceView and Shell.tsx fill it (tileMenu: the
// items VoiceView passes: wave B task 4 added Profile, Message, Mention, Edit roles, and on your own menu Mute, Deafen, Move to and
// Leave voice; Server mute and Disconnect are wave C). Opened at the prototype's spot, 40px in.
const meta = {
  title: 'Voice/PersonContextMenu', component: PersonContextMenu,
  args: { ...tileMenu('Ole'), open: true, at: { x: 40, y: 40 }, onOpenChange: () => {}, returnFocus: () => null },
  parameters: { layout: 'fullscreen' },
  decorators: [Story => <div style={{ height: 420 }}><Story /></div>],
} satisfies Meta<typeof PersonContextMenu>;
export default meta;
type Story = StoryObj<typeof meta>;

/** Another person in voice, as the voice view opens it: the header, "User volume", Profile, Message, Mention, Mute for me, Copy username, Edit roles. */
export const Person: Story = {};
/** The same with every item the prototype draws (Mention, and Edit roles for a viewer with Manage roles). */
export const PersonWithMentionAndRoles: Story = { args: { ...tileMenu('Ole') } };
/** Muted for me: the item's box is checked (the menu stays open on it). */
export const MutedForMe: Story = { args: { ...tileMenu('Ole', { muted: true, volume: 70 }) } };
/** Yourself in a call: Mute and Deafen with their keys, Profile, Move to the other voice channels (with the head count), Leave voice. */
export const You: Story = {
  render: () => <PersonContextMenu {...tileMenu('Mira', { own: true, title: 'Mira (you)' })} open at={{ x: 40, y: 40 }} onOpenChange={() => {}} returnFocus={() => null} />,
};
/** Yourself with a long room name: it ends in an ellipsis inside the 236px menu, and the keycaps and head counts stay in view. */
export const YouLongRoomName: Story = {
  render: () => {
    const menu = tileMenu('Mira', { own: true, title: 'Mira (you)' });
    const long = { ...menu, move: { ...menu.move!, rooms: [{ id: 'general', name: 'general', count: 0 }, { id: 'late', name: 'Late night lounge for people', count: 3 }] } };
    return <PersonContextMenu {...long} open at={{ x: 40, y: 40 }} onOpenChange={() => {}} returnFocus={() => null} />;
  },
};
/** A bot (Glenn): the rounded-square avatar; the volume, Mute for me and Copy username only. */
export const Bot: Story = { args: { ...tileMenu('Glenn', { bot: true }) } };
/** A shared screen: its own stream's volume. */
export const Screen: Story = { args: { ...tileMenu('Kari', { screen: true }) } };
/** Task 9: a member row's menu, as MemberCardHost fills it (no voice): Profile, Message, Copy username, Edit roles, then Kick after a rule. */
export const Member: Story = {
  args: { person: { name: 'Björn', where: '@bjorn' }, label: 'actions for Björn', volume: undefined, items: [
    { id: 'profile', label: 'Profile', icon: 'user' }, { id: 'message', label: 'Message', icon: 'message-square' },
    { id: 'mention', label: 'Mention', icon: 'at-sign' }, { id: 'copy', label: 'Copy username', icon: 'copy' }, { id: 'roles', label: 'Edit roles', icon: 'shield' },
    { id: 'kick', label: 'Kick Björn', icon: 'log-out', danger: true, sep: true },
  ] },
};
