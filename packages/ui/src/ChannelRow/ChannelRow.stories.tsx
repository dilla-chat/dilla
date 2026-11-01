import type { Meta, StoryObj } from '@storybook/react-vite';
import { ChannelRow } from './ChannelRow.tsx';
import { ChannelList } from '../ChannelList/ChannelList.tsx';
import { VoiceChannelRow } from '../VoiceChannelRow/VoiceChannelRow.tsx';
import type { VoiceParticipantView } from '../CallTile/CallTile.tsx';

// The sidebar's scroller: 244px wide on bg-2, padded 4px 8px (Signal §5.4).
const meta = { title: 'Primitives/ChannelRow', component: ChannelRow, args: { onSelect: () => {} },
  decorators: [Story => <div style={{ width: 244, boxSizing: 'border-box', background: 'var(--bg-2)', padding: '4px 8px' }}><Story /></div>] } satisfies Meta<typeof ChannelRow>;
export default meta;
type Story = StoryObj<typeof meta>;
const NEW = (n: string) => `${n} new`;
export const Text: Story = { args: { name: 'general', kind: 'text' } };
export const Active: Story = { args: { name: 'general', kind: 'text', active: true } };
/** Unread: a bright 600 name and the "1 new" tag. */
export const Unread: Story = { args: { name: 'loot', kind: 'text', unread: 1, newLabel: NEW } };
/** A mention: the "@2" tag in --mention (Ruling O). */
export const Mention: Story = { args: { name: 'raids', kind: 'text', unread: 4, mentions: 2, newLabel: NEW } };
/** The built-in AFK channel: the moon glyph. */
/** Active with a mention (the prototype's VK #general). */
export const MentionActive: Story = { args: { name: 'general', kind: 'text', active: true, unread: 4, mentions: 2, newLabel: NEW } };
export const Afk: Story = { args: { name: 'AFK', kind: 'afk' } };
export const Mentions: Story = { args: { name: 'screenshots', kind: 'text', unread: 3, mentions: 12 } };
export const ReadableByServer: Story = { args: { name: 'lfg', kind: 'text', readable: true } };
export const Voice: Story = { args: { name: 'longhouse', kind: 'voice' } };
export const PrivateVoice: Story = { args: { name: 'sauna', kind: 'voice', private: true } };
export const Muted: Story = { args: { name: 'random', kind: 'text', muted: true } };
export const DirectMessage: Story = { args: { name: 'ada', kind: 'dm', unread: 2 } };
export const MutedWithMention: Story = { args: { name: 'random', kind: 'text', unread: 7, mentions: 2, muted: true } };
// web3.7 (flow 11): the announcement kind takes the megaphone in place of #; the badge is a note after the name.
export const AnnouncementSlow: Story = { args: { name: 'news', kind: 'announcement', badge: 'slow mode' } };
export const Announcement: Story = { args: { name: 'news', kind: 'announcement', unread: 2 } };
export const SlowModeText: Story = { args: { name: 'general', kind: 'text', badge: 'slow mode', active: true } };

/** A collapsed category still lists its active and unread rows (Signal §5.4, R23): here #general (active) and #loot (1 new). */
export const CollapsedButUnread: Story = {
  args: { name: 'general', kind: 'text' },
  decorators: [Story => <div style={{ margin: '-4px -8px' }}><Story /></div>],
  render: () => (
    <div style={{ height: 260 }}>
      <ChannelList label="channels" title="Midgard Crew" activeId="general" onSelect={() => {}} emptyLabel="no channels" newLabel={NEW}
        categories={[{ id: 'raids', name: 'raids', collapsed: true }]} onToggleCategory={() => {}}
        channels={[
          { id: 'general', name: 'general', kind: 'text', readable: false, parentId: 'raids' },
          { id: 'loot', name: 'loot', kind: 'text', readable: false, parentId: 'raids', unread: 1 },
          { id: 'quiet', name: 'quiet', kind: 'text', readable: false, parentId: 'raids' },
        ]} />
    </div>
  ),
};

const person = (name: string, more: Partial<VoiceParticipantView> = {}): VoiceParticipantView => ({
  deviceId: `d-${name}`, userId: `u-${name}`, name, state: 'ok', speaking: false, muted: false, deafened: false, video: false, screen: false, web: true, bot: false, ...more,
});
const VOICE_LABELS = {
  open: 'voice {name}', count: '{n} in voice', playbackOnly: 'playback only', join: 'join voice', joined: 'connected',
  state: { speaking: 'speaking', muted: 'muted', deafened: 'deafened', notHeard: 'not heard yet', unverified: 'unverified stream', connecting: 'connecting', video: 'camera on', screen: 'sharing screen', web: 'web', bot: 'bot', noSound: 'no sound' },
};
/** The voice channel you are in: a --fg 600 name, the --live speaker, the count at the right (the sidebar draws it as a voice row). */
export const VoiceJoined: Story = {
  args: { name: 'harbour', kind: 'voice' },
  render: () => (
    <ul role="list" style={{ listStyle: 'none', margin: 0, padding: 0 }}>
      <li className="d-voice-item">
        <VoiceChannelRow name="harbour" current={false} joined participants={[person('Ada'), person('Björn'), person('Mira')]} labels={VOICE_LABELS} onOpen={() => {}} />
      </li>
    </ul>
  ),
};
