import { useEffect, useRef, useState, type ReactNode } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { MessageRow } from './MessageRow.tsx';
import { MessageToolbar } from '../MessageToolbar/MessageToolbar.tsx';
import { ReactionBar } from '../ReactionBar/ReactionBar.tsx';
import { AttachmentCard } from '../AttachmentCard/AttachmentCard.tsx';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';
import { useDrawnImage } from '../internal/drawn-image.ts';
import { GeneralLog, RAID_JSON, Row, rowReactions, yamlBody } from './conversation.fixture.tsx';

// The state strings are task 24's to own in en.ts; these are the drafts it starts from.
const meta = {
  title: 'Conversation/MessageRow', component: MessageRow,
  args: { author: 'ada', time: '21:02', body: 'anyone up for a round tonight?', state: 'ok' },
  decorators: [Story => <div style={{ maxWidth: 720 }}><Story /></div>],
} satisfies Meta<typeof MessageRow>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Ok: Story = {};
export const Own: Story = { args: { author: 'mira', body: 'count me in', own: true } };
export const Web: Story = { args: { author: 'björn', body: 'after nine, still on the boat', tag: 'web' } };
export const Bot: Story = { args: { author: 'loot-bot', body: 'weekly reset in 2 h', tag: 'bot' } };
export const Pending: Story = { args: { author: 'mira', body: 'count me in', own: true, state: 'pending', stateLabel: 'sending' } };
export const Failed: Story = { args: { author: 'mira', body: 'count me in', own: true, state: 'failed', stateLabel: 'not sent', detail: 'E_TOO_LARGE',
  actions: [{ label: 'retry', onAction: () => {} }, { label: 'discard', onAction: () => {} }] } };
export const CannotRead: Story = { args: { state: 'cannot-read', stateLabel: 'This message could not be read on this device.', detail: 'E_SENDER_MISMATCH' } };
export const Deleted: Story = { args: { state: 'deleted', stateLabel: 'message deleted' } };
export const Multiline: Story = { args: { body: 'route for tonight:\n1. harbour\n2. the old mill\n3. back by midnight' } };

// ---- web-2b (L-UI-40): every string is L-COPY-03's, quoted (en.ts gains them in task 9). ----

/** An image card whose thumbnail is drawn on a canvas at story time and shown through a revoked-on-unmount blob: URL. */
function ImageCardStory({ tabbable = true }: { tabbable?: boolean }) {
  const thumbUrl = useDrawnImage(320, 240);
  return <AttachmentCard kind="image" name="drawn.png" size="48 KB" thumbUrl={thumbUrl} w={320} h={240} openLabel="open drawn.png"
    onOpen={() => {}} saveLabel="save drawn.png" state="idle" tabbable={tabbable} />;
}

/** The toolbar shows while the row holds focus: this focuses the row on mount, so the screenshots show it. */
function FocusRow({ children }: { children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => { ref.current?.querySelector('article')?.focus(); }, []);
  return <div ref={ref} style={{ paddingTop: 24 }}>{children}</div>;
}

const LONG = `${'meet at the harbour after nine, bring the rope and the good lantern. '.repeat(13).trimEnd()} Ok.`;

export const Everything: Story = {
  args: {
    author: 'ada', time: '21:04', state: 'ok', own: true, active: true, seq: '4', msgId: 'ab'.repeat(16), editedLabel: 'edited', pinnedLabel: 'pinned',
    mention: true,
    body: [{ kind: 'text', text: '' }, { kind: 'mention', label: '@mira', me: false, broadcast: false }, { kind: 'text', text: ' bring the map, ' },
      { kind: 'mention', label: '@everyone', me: true, broadcast: true }, { kind: 'text', text: ' meet at the harbour' }],
    reply: { label: 'reply to björn', author: 'björn', excerpt: 'after nine, still on the boat', state: 'ok', jumpLabel: 'go to the original message', onJump: () => {} },
    toolbar: <MessageToolbar moreLabel="more actions" label="actions for this message" tabbable items={[
      { id: 'react', label: 'react', onAction: () => {} }, { id: 'reply', label: 'reply', onAction: () => {} },
      { id: 'edit', label: 'edit', onAction: () => {} }, { id: 'pin', label: 'unpin', onAction: () => {} },
      { id: 'delete', label: 'delete', danger: true, onAction: () => {} },
    ]} />,
    attachments: <><ImageCardStory /><AttachmentCard kind="file" name="route.gpx" size="12.4 KB" thumbUrl={null} w={null} h={null}
      openLabel="open route.gpx" saveLabel="save route.gpx" onSave={() => {}} state="idle" tabbable /></>,
    reactions: <ReactionBar label="reactions" tabbable items={[{ emoji: '👍', count: 3, mine: true, name: 'thumbs up, 3' },
      { emoji: '🦀', count: 1, mine: false, name: 'crab, 1' }, { emoji: '🎉', count: 2, mine: false, name: 'party popper, 2' }]}
      onToggle={() => {}} addLabel="add a reaction" onAdd={() => {}} />,
    pendingAction: { label: 'saving…', tone: 'pending' },
  },
  decorators: [Story => <FocusRow><Story /></FocusRow>],
};
export const LongBody: Story = { args: { body: LONG } };
export const MentionsMe: Story = {
  args: { author: 'mira', time: '21:05', mention: true,
    body: [{ kind: 'mention', label: '@ada', me: true, broadcast: false }, { kind: 'text', text: ' are you bringing the map?' }] },
};
/** MP7: a user mention and the author name are buttons that open a profile card; role and broadcast pills stay plain. */
export const OpensProfile: Story = {
  args: { author: 'mira', authorId: 'u-mira', time: '21:05', onOpenProfile: () => undefined,
    body: [{ kind: 'mention', label: '@ada', me: false, broadcast: false, userId: 'u-ada' }, { kind: 'text', text: ' and ' },
      { kind: 'mention', label: '@everyone', me: false, broadcast: true }, { kind: 'text', text: ' take a look' }] },
};
/** A role mention carries its role colour as a dot before the name; the text keeps its token colour. */
export const RoleMention: Story = {
  args: { author: 'mira', time: '21:05',
    body: [{ kind: 'mention', label: '@moderators', me: false, broadcast: false, color: 0x3498db }, { kind: 'text', text: ' please look at this' }] },
};
export const ReplyMissing: Story = {
  args: { author: 'mira', time: '21:05', body: 'count me in',
    reply: { label: 'reply to björn', author: 'björn', excerpt: '', state: 'missing', stateText: 'the original message cannot be shown here', jumpLabel: 'go to the original message' } },
};
export const ReplyDeleted: Story = {
  args: { author: 'mira', time: '21:05', body: 'count me in',
    reply: { label: 'reply to björn', author: 'björn', excerpt: '', state: 'deleted', stateText: 'the original message was deleted', jumpLabel: 'go to the original message' } },
};
export const PendingEdit: Story = {
  args: { author: 'ada', time: '21:04', own: true, body: 'meet at the harbour at nine', editedLabel: 'edited', pendingAction: { label: 'saving…', tone: 'pending' } },
};
export const FailedReaction: Story = {
  args: { author: 'mira', time: '21:05', body: 'count me in', active: true,
    pendingAction: { label: 'not saved', tone: 'failed', actions: [{ label: 'retry', onAction: () => {} }, { label: 'discard', onAction: () => {} }] } },
};
export const AttachmentOnly: Story = { args: { author: 'björn', time: '21:06', tag: 'web', body: [], attachments: <ImageCardStory tabbable={false} /> } };
export const Flash: Story = { args: { author: 'björn', time: '21:03', tag: 'web', body: 'after nine, still on the boat', flash: true } };

// web3.7 (flow 11): a thread's summary chip under the body, and the thread item in the row's toolbar.
export const ThreadChip: Story = {
  args: { author: 'ada', time: '21:02', body: 'anyone up for a round tonight?', thread: { label: '3 replies', onOpen: () => {} } },
};
export const ThreadChipActive: Story = {
  args: { author: 'ada', time: '21:02', body: 'anyone up for a round tonight?', active: true, thread: { label: '1 reply', onOpen: () => {} },
    toolbar: <MessageToolbar moreLabel="more actions" label="actions for this message" tabbable items={[
      { id: 'react', label: 'react', onAction: () => {} }, { id: 'reply', label: 'reply', onAction: () => {} },
      { id: 'thread', label: 'reply in thread', onAction: () => {} }, { id: 'pin', label: 'pin', onAction: () => {} },
    ]} /> },
};

// web3.9 (R29, G2, R30): a grouped row, an always-shown device tag, and a phone row that reveals its toolbar on a tap.
export const Grouped: Story = { args: { grouped: true, time: '10:06 PM', body: 'i have the map' } };
export const DeviceTagAlways: Story = { args: { author: 'björn', body: 'after nine, still on the boat', tag: 'web' } };
const phoneToolbar = (
  <MessageToolbar moreLabel="more actions" label="actions for this message" tabbable items={[
    { id: 'react', label: 'react', onAction: () => {} }, { id: 'reply', label: 'reply', onAction: () => {} }, { id: 'pin', label: 'pin', onAction: () => {} },
  ]} />
);
function PhoneRow({ startRevealed }: { startRevealed: boolean }) {
  const [revealed, setRevealed] = useState(startRevealed);
  return (
    <PhoneLayoutProvider value>
      <div data-layout="phone" style={{ maxWidth: 390 }}>
        <MessageRow author="ada" time="21:02" body="anyone up for a round tonight?" state="ok" toolbar={phoneToolbar} revealed={revealed} onReveal={setRevealed} />
      </div>
    </PhoneLayoutProvider>
  );
}
export const PhoneHidden: Story = { render: () => <PhoneRow startRevealed={false} /> };
export const PhoneRevealed: Story = { render: () => <PhoneRow startRevealed /> };

// ---- Task 7 (Signal §8): the prototype's conversation states, with the props packages/web passes (conversation.fixture.tsx).
const wide = (Story: () => ReactNode) => <div style={{ maxWidth: 900, paddingTop: 16, background: 'var(--bg)' }}><Story /></div>;
const signal = { globals: { theme: 'signal' }, decorators: [wide] };
export const SignalFirst: Story = { ...signal, render: () => <Row author="Ada" time="20:27" body="raid tonight at 20:00? bring the good shields" nameColor={0xE9B6EC} /> };
export const SignalFollowUp: Story = { ...signal, render: () => <>
  <Row author="Ada" time="20:29" body="the map from saturday is in #loot, the one with the coffee stain" nameColor={0xE9B6EC} />
  <Row author="Ada" time="20:29" grouped body="see you at the bridge" nameColor={0xE9B6EC} />
</> };
/** Hover forced (`forceHover`): the fill, the accent bar, the gutter time and the floating toolbar. */
export const SignalHover: Story = { ...signal, render: () => <>
  <Row author="Ada" time="20:29" body="the map from saturday is in #loot, the one with the coffee stain" nameColor={0xE9B6EC} />
  <Row author="Ada" time="20:29" grouped forceHover body="see you at the bridge" nameColor={0xE9B6EC} />
</> };
export const SignalMentionOfYou: Story = { ...signal, render: () => <Row author="Liv" time="18:12" mention
  body={[{ kind: 'text', text: 'me. ' }, { kind: 'mention', label: '@Mira', me: true, broadcast: false }, { kind: 'text', text: ' can you bring the cardamom?' }]} /> };
const REPLY = { label: 'reply to Ada', author: 'Ada', excerpt: 'raid tonight at 20:00? bring the good shields', state: 'ok' as const, jumpLabel: 'go to the original message', onJump: () => {} };
export const SignalReply: Story = { ...signal, render: () => <Row author="Mira" time="20:31" own reply={REPLY} body="two ropes. final answer." /> };
export const SignalReplyFlash: Story = { ...signal, render: () => <Row author="Ada" time="20:27" flash body="raid tonight at 20:00? bring the good shields" nameColor={0xE9B6EC} /> };
export const SignalReactions: Story = { ...signal, render: () => <Row author="Björn" time="20:27" body="in. last time the bridge troll took everything we had" forceHover
  reactions={rowReactions([{ emoji: '🛡️', count: 3, mine: true, name: 'shield' }, { emoji: '🔥', count: 1, mine: false, name: 'fire' }])} /> };
export const SignalThread: Story = { ...signal, render: () => <Row author="Björn" time="20:27" body="in. last time the bridge troll took everything we had"
  thread={{ label: '2 replies', last: 'last 20:33', onOpen: () => {} }} /> };
export const SignalCodeBlockYaml: Story = { ...signal, render: () => <Row author="Björn" time="20:29" body={yamlBody} /> };
export const SignalCodeBlockJson: Story = { ...signal, render: () => <Row author="Björn" time="20:29"
  body={[{ kind: 'text', text: 'same thing for the bot:' }, { kind: 'codeblock', lang: 'json', text: RAID_JSON }]} /> };
function SignalFiles() {
  const thumbUrl = useDrawnImage(560, 420);
  return <Row author="Björn" time="19:02" body="uploaded the map. coffee stain is a feature" attachments={<>
    <AttachmentCard kind="image" name="map.png" size="1.2 MB" thumbUrl={thumbUrl} w={560} h={420} openLabel="open map.png" onOpen={() => {}} saveLabel="save map.png" state="idle" tabbable={false} />
    <AttachmentCard kind="file" name="route.gpx" size="12.4 KB" thumbUrl={null} w={null} h={null} openLabel="open route.gpx" saveLabel="save route.gpx" onSave={() => {}} state="idle" tabbable={false} />
  </>} />;
}
export const SignalAttachments: Story = { ...signal, render: () => <SignalFiles /> };
function SignalGif() {
  const thumbUrl = useDrawnImage(480, 360);
  return <Row author="Mira" time="20:32" own body={[]} attachments={<AttachmentCard kind="image" name="victory.gif" size="2.1 MB" thumbUrl={thumbUrl} w={480} h={360}
    openLabel="open victory.gif" onOpen={() => {}} saveLabel="save victory.gif" state="idle" tabbable={false} badge="GIF" />} />;
}
export const SignalGifAttachment: Story = { ...signal, render: () => <SignalGif /> };
export const SignalReplyingTarget: Story = { ...signal, render: () => <Row author="Ada" time="20:27" replying body="raid tonight at 20:00? bring the good shields" nameColor={0xE9B6EC} /> };
/** The prototype's #general feed, top to bottom (the now-playing card is Task 8b's). */
export const SignalFeed: Story = { globals: { theme: 'signal' }, decorators: [Story => <div style={{ height: 720, maxWidth: 1000, display: 'flex', flexDirection: 'column' }}><Story /></div>], render: () => <GeneralLog /> };
