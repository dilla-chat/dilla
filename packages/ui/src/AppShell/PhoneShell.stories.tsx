import type { Meta, StoryObj } from '@storybook/react-vite';
import { useState, type CSSProperties, type ReactNode } from 'react';
import { AppShell } from './AppShell.tsx';
import { CommunityRail } from '../CommunityRail/CommunityRail.tsx';
import { ChannelList } from '../ChannelList/ChannelList.tsx';
import { ChannelHeader } from '../ChannelHeader/ChannelHeader.tsx';
import { MessageLog } from '../MessageLog/MessageLog.tsx';
import { MessageRow } from '../MessageRow/MessageRow.tsx';
import { Composer } from '../Composer/Composer.tsx';
import { MemberList } from '../MemberList/MemberList.tsx';
import { Button } from '../Button/Button.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { Avatar } from '../Avatar/Avatar.tsx';
import { Pill } from '../Pill/Pill.tsx';
import { ActionMenu } from '../ActionMenu/ActionMenu.tsx';
import { StateDot } from '../StateDot/StateDot.tsx';
import { TabBar, type TabBarItem } from '../TabBar/TabBar.tsx';
import { CallBar } from '../CallBar/CallBar.tsx';
import { LinkBanner } from '../LinkBanner/LinkBanner.tsx';
import { RingBanner } from '../RingBanner/RingBanner.tsx';
import { ThreadPane } from '../ThreadPane/ThreadPane.tsx';
import { UserPanel } from '../UserPanel/UserPanel.tsx';
import { GeneralLog, GeneralThread, SignalComposer } from '../MessageRow/conversation.fixture.tsx';
import { Tooltip } from '../Tooltip/Tooltip.tsx';
import { IconButton } from '../IconButton/IconButton.tsx';
import { HarbourStage, harbourTiles } from '../CallStage/stage.fixture.tsx';
import { harbourStageBar } from '../NowPlaying/np.fixture.tsx';
import { harbourPeople } from './AppShell.fixture.tsx';
import { YOU } from '../internal/sample/midgard.ts';
import { SampleCore, SAMPLE_DUE } from './sampleCore.fixture.tsx';
import { Activity as WebActivity } from '../../../web/src/screens/Activity.tsx';
import { You as WebYou } from '../../../web/src/screens/You.tsx';

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

// A 390 × 844 phone (an iPhone 14). Switch the theme in the toolbar (signal, light, high-contrast). The decorator's 16px
// padding is taken back so the shell fills the frame exactly.
const meta = { title: 'Shell/Phone', parameters: { layout: 'fullscreen', webStyles: true } } satisfies Meta;
export default meta;
type Story = StoryObj<typeof meta>;

const noop = () => {};
// transform makes the frame the containing block of the fixed phone shell, so it stays inside its 390 px.
const frame: CSSProperties = { width: 390, margin: -16, transform: 'translateZ(0)', ['--d-shell-h' as string]: '844px' };
const Phone = ({ children }: { children: ReactNode }) => <div style={frame}>{children}</div>;

// The tabs the web passes (packages/web/src/screens/Shell.tsx `tabItem`): home (house), activity (bell), you (user), as in the prototype.
type TabId = 'servers' | 'dms' | 'activity' | 'you';
const tabs = (current: TabId): TabBarItem[] => [
  { id: 'home', label: 'Home', href: '#home', icon: 'house', current: current === 'servers' || current === 'dms', count: 3, countLabel: 'Home, 3 new' },
  { id: 'activity', label: 'Activity', href: '#activity', icon: 'bell', current: current === 'activity', count: 120, countLabel: 'Activity, 120 new' },
  { id: 'you', label: 'You', href: '#you', icon: 'user', current: current === 'you' },
];
const tabBar = (current: TabId) => <TabBar label="main" items={tabs(current)} onSelect={noop} />;
// Task 11 (Shell.tsx's homeCount / activityCount with the prototype's sample): no unread DM, so Home has no dot; Valhalla Kitchen's
// two mentions put the dot on Activity.
const signalTabBar = (current: TabId) => <TabBar label="main" onSelect={noop} items={[
  { id: 'home', label: 'Home', href: '#home', icon: 'house', current: current === 'servers' || current === 'dms' },
  { id: 'activity', label: 'Activity', href: '#activity', icon: 'bell', current: current === 'activity', count: 2, countLabel: 'Activity, 2 new' },
  { id: 'you', label: 'You', href: '#you', icon: 'user', current: current === 'you' },
]} />;

const rowLabel = (c: { name: string; unread: number; mentions: number; muted: boolean }) =>
  c.muted ? `${c.name}, muted, mentions ${c.mentions}` : `${c.name}, unread ${c.unread}, mentions ${c.mentions}`;
const itemLabel = (i: { name: string; unread: number; mentions: number }) => `${i.name}, unread ${i.unread}, mentions ${i.mentions}`;

const rail = (
  <CommunityRail label="servers" activeId="a1" onSelect={noop} addLabel="add a server" onAdd={noop} settingsLabel="settings" onSettings={noop}
    itemLabel={itemLabel}
    items={[{ id: 'a1', name: 'Midgard Crew' }, { id: 'b2', name: 'Night Owls', unread: 4 }, { id: 'c3', name: 'raid planning', unread: 2, mentions: 2 }]} />
);

function ServerMenu() {
  const [open, setOpen] = useState(false);
  return (
    <ActionMenu label="server menu" icon="ellipsis-vertical" open={open} onOpenChange={setOpen} onSelect={noop}
      items={[{ id: 'invite', label: 'invite people' }, { id: 'channel', label: 'new channel' }, { id: 'settings', label: 'server settings' }, { id: 'leave', label: 'leave server', danger: true }]} />
  );
}

const serverChannels = (
  <ChannelList label="channels" title="Midgard Crew" headerAction={<ServerMenu />} activeId="g" onSelect={noop} emptyLabel="No channels yet." rowLabel={rowLabel}
    channels={[
      { id: 'g', name: 'general', kind: 'text', readable: false }, { id: 'l', name: 'lfg', kind: 'text', readable: true, unread: 3 },
      { id: 't', name: 'loot', kind: 'text', readable: false, unread: 12, mentions: 2 }, { id: 'r', name: 'random', kind: 'text', readable: false, unread: 7, muted: true },
      { id: 'v', name: 'longhouse', kind: 'voice', readable: false },
    ]} />
);

const dmChannels = (
  <ChannelList label="direct messages" title="direct messages" activeId={null} onSelect={noop} emptyLabel="No conversations yet." rowLabel={rowLabel}
    footer={<div style={{ padding: '0.5rem 0.75rem' }}><Button variant="ghost">message someone</Button></div>}
    channels={[
      { id: 'd1', name: 'ada', kind: 'dm', readable: false, unread: 2 }, { id: 'd2', name: 'björn', kind: 'dm', readable: false, unread: 1 },
      { id: 'd3', name: 'mira', kind: 'dm', readable: false }, { id: 'd4', name: 'loot-bot', kind: 'dm', readable: false, muted: true },
    ]} />
);


const log = (
  <MessageLog label="messages in #general" emptyLabel="No messages yet.">
    <MessageRow author="ada" time="21:02" body="anyone up for a round tonight?" state="ok" />
    <MessageRow author="björn" time="21:03" body="after nine, still on the boat" state="ok" tag="web" />
    <MessageRow author="mira" time="21:04" body="count me in" state="ok" own />
    <MessageRow author="loot-bot" time="21:05" body="weekly reset in 2 h" state="ok" tag="bot" />
    <MessageRow author="ada" time="21:06" body="bring the tank gear, the second boss hits hard" state="ok" />
  </MessageLog>
);
const composer = (
  <Composer label="message #general" placeholder="message #general" maxLength={4000} value="" onChange={noop} onSend={noop} sendLabel="send" counterLabel={n => `${n} left`} />
);
const menu = <Button variant="ghost" aria-label="members"><Icon name="ellipsis-vertical" size={22} /></Button>;
const header = <ChannelHeader name="general" phone={{ backLabel: 'back to channels', onBack: noop, menu }} />;

const members = (
  <MemberList label="members" loading={false} loadingLabel="Loading members…" emptyLabel="No members yet."
    groups={[
      { id: 'mods', kind: 'role', title: 'moderators', members: [{ id: 'u1', name: 'Astrid', isOwner: true }] },
      { id: 'members', kind: 'online', title: 'online', members: [
        { id: 'u2', name: 'Bjorn' },
        { id: 'u3', name: 'Mira' },
      ] },
    ]} />
);

const callBar = <CallBar phase="connected" where="longhouse" muted={false} labels={{ open: 'open call in longhouse', mute: 'mute', leave: 'leave call', region: 'call' }} onOpen={noop} onMute={noop} onLeave={noop} />;

const common = { skipLabel: 'skip to content', header: null, composer: null } as const;

export const ServersList: Story = {
  render: () => <Phone><AppShell {...common} layout="phone" phonePane="list" rail={rail} sidebar={serverChannels} tabBar={tabBar('servers')}>{null}</AppShell></Phone>,
};
// Task 6b fix round 1: the servers list as the web composes it in Signal (Shell.tsx): the head is the name and the 26px ⌄ (Ruling AB),
// the stat line, the categories and rows; no search, no reminders. Task 11: harbour's people as the prototype's sample lists them.
const signalChannels = (
  <ChannelList label="channels" title="Midgard Crew" activeId="general" onSelect={noop} emptyLabel="No channels yet." rowLabel={rowLabel}
    headerAction={<ServerHeadMenu />}
    stats={[{ text: '3 online', live: true }, { text: '5 in voice' }]} newLabel={n => `${n} new`}
    categories={[{ id: 'raids', name: 'raids', collapsed: false }, { id: 'boats', name: 'boats', collapsed: false }]} onToggleCategory={noop}
    voice={{ labels: { open: 'voice {name}', count: '{n} in voice', playbackOnly: 'playback only', join: 'join voice', joined: 'connected',
      state: { speaking: 'speaking', muted: 'muted', deafened: 'deafened', notHeard: 'not heard yet', unverified: 'unverified stream', connecting: 'connecting',
        video: 'camera on', screen: 'sharing screen', web: 'web', bot: 'bot', noSound: 'no sound' } }, rooms: { hv: { joined: false, participants: [] }, harbour: { joined: false, participants: harbourPeople(false) }, afk: { joined: false, participants: [] } } }}
    channels={[
      { id: 'general', name: 'general', kind: 'text', readable: false }, { id: 'hv', name: 'general', kind: 'voice', readable: false },
      { id: 'loot', name: 'loot', kind: 'text', readable: false, parentId: 'raids', unread: 1 },
      { id: 'harbour', name: 'harbour', kind: 'voice', readable: false, parentId: 'boats' }, { id: 'afk', name: 'AFK', kind: 'voice', readable: false, afk: true },
    ]} />
);
// The rail as Shell.tsx passes it: the @ home tile first, no settings button (settings live under the You tab).
const signalRail = (
  <CommunityRail label="servers" activeId="a1" onSelect={noop} addLabel="add a server" onAdd={noop} itemLabel={itemLabel}
    home={{ label: 'direct messages', itemLabel: 'direct messages, unread 0, mentions 0', selected: false, unread: 0, mentions: 0, onSelect: noop }}
    items={[{ id: 'a1', name: 'Midgard Crew' }, { id: 'b2', name: 'Valhalla Kitchen', unread: 2, mentions: 2 }]} />
);
export const SignalServersList: Story = {
  globals: { theme: 'signal' },
  // Ruling AE: the list ends in the user panel above the tab bar, as the web's phone sidebar does (packages/web/src/screens/Shell.tsx).
  render: () => <Phone><AppShell {...common} layout="phone" phonePane="list" rail={signalRail} tabBar={signalTabBar('servers')}
    sidebar={<div style={{ display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 }}><div style={{ flex: '1 1 auto', minHeight: 0 }}>{signalChannels}</div>
      <UserPanel name={YOU.name} username={YOU.handle} line={YOU.status} muted={false} deafened={false} onMute={noop} onDeafen={noop}
        labels={{ region: 'your account', mute: 'mute', deafen: 'deafen' }} shortcuts={{ mute: 'Ctrl+Shift+M', deafen: 'Ctrl+Shift+D' }}
        status={{ current: 'online', label: 'your status: online', onChoose: noop,
          items: [{ id: 'online', label: 'online' }, { id: 'idle', label: 'idle' }, { id: 'dnd', label: 'do not disturb' }, { id: 'invisible', label: 'invisible' }] }} />
    </div>}>{null}</AppShell></Phone>,
};
export const ChannelView: Story = {
  render: () => (
    <Phone><AppShell {...common} layout="phone" phonePane="view" tabBar={tabBar('servers')} rail={rail} sidebar={serverChannels} header={header} composer={composer}>{log}</AppShell></Phone>
  ),
};
// web3.7.1: focusing the composer is typing, so the tab bar steps aside (the shell sets data-typing and hides it).
export const ChannelViewTyping: Story = {
  render: () => (
    <Phone><AppShell {...common} layout="phone" phonePane="view" tabBar={tabBar('servers')} rail={rail} sidebar={serverChannels} header={header} composer={composer}>{log}</AppShell></Phone>
  ),
  play: ({ canvasElement }) => { canvasElement.querySelector('textarea')?.focus(); },
};
export const DmsList: Story = {
  render: () => <Phone><AppShell {...common} layout="phone" phonePane="list" rail={null} sidebar={dmChannels} tabBar={tabBar('dms')}>{null}</AppShell></Phone>,
};
// Task 11 (Jonas, Q3; rule 3d): the web's own Activity and You (screens/Activity.tsx, screens/You.tsx) over the Midgard sample,
// in the list pane with no rail, as Shell.tsx mounts them for the Activity and You tabs.
export const Activity: Story = {
  globals: { theme: 'signal' },
  render: () => <Phone><SampleCore><AppShell {...common} layout="phone" phonePane="list" rail={null}
    sidebar={<WebActivity onOpen={noop} reminders={{ due: [...SAMPLE_DUE], onOpen: noop }} />} tabBar={signalTabBar('activity')}>{null}</AppShell></SampleCore></Phone>,
};
export const You: Story = {
  globals: { theme: 'signal' },
  render: () => <Phone><SampleCore><AppShell {...common} layout="phone" phonePane="list" rail={null}
    sidebar={<WebYou onOpenSettings={noop} />} tabBar={signalTabBar('you')}>{null}</AppShell></SampleCore></Phone>,
};
export const Offline: Story = {
  render: () => (
    <Phone>
      <AppShell {...common} layout="phone" phonePane="list" rail={rail} sidebar={serverChannels} tabBar={tabBar('servers')}
        banner={<LinkBanner status="offline" labels={{ offline: 'offline · reconnecting to dilla.thim.dev', connecting: 'connecting to dilla.thim.dev' }} />}>{null}</AppShell>
    </Phone>
  ),
};
export const CallBarOnList: Story = {
  name: 'CallBar',
  render: () => <Phone><AppShell {...common} layout="phone" phonePane="list" rail={rail} sidebar={serverChannels} tabBar={tabBar('servers')} callBar={callBar}>{null}</AppShell></Phone>,
};
// A channel the server can read: the small eye stays beside the title on a phone.
const readableHeader = <ChannelHeader name="lfg" readable readableLabel="readable by dilla.thim.dev" phone={{ backLabel: 'back to channels', onBack: noop, menu }} />;
export const ReadableChannelView: Story = {
  render: () => (
    <Phone><AppShell {...common} layout="phone" phonePane="view" tabBar={tabBar('servers')} rail={rail} sidebar={serverChannels} header={readableHeader} composer={composer}>{log}</AppShell></Phone>
  ),
};
// In a call on the view pane: the call bar is the bottom row and clears the home indicator.
export const ChannelViewInCall: Story = {
  render: () => (
    <Phone><AppShell {...common} layout="phone" phonePane="view" tabBar={tabBar('servers')} rail={rail} sidebar={serverChannels} header={header} composer={composer} callBar={callBar}>{log}</AppShell></Phone>
  ),
};
// A ring shows above the list pane, whose main is hidden.
const ring = (
  <RingBanner rings={[{ callId: 'c1', text: 'ada is calling', busy: false, name: 'ada' }]} labels={{ region: 'incoming calls', answer: 'answer', decline: 'decline' }} onAnswer={noop} onDecline={noop} />
);
export const RingOnList: Story = {
  render: () => <Phone><AppShell {...common} layout="phone" phonePane="list" rail={rail} sidebar={serverChannels} tabBar={tabBar('servers')} ring={ring}>{null}</AppShell></Phone>,
};
// Task 11 (Jonas, Q4): the members sheet covers the screen with the list's own head (Members ×), as on a desktop.
export const MembersSheet: Story = {
  render: () => (
    <Phone>
      <AppShell {...common} layout="phone" phonePane="view" tabBar={tabBar('servers')} rail={rail} sidebar={serverChannels} header={header} composer={composer}
        aside={<MemberList label="Members" closeLabel="close members" onClose={noop} loading={false} loadingLabel="Loading members…" emptyLabel="No members yet."
          groups={[{ id: 'online', kind: 'online', title: 'online', members: [{ id: 'u2', name: 'Björn', presence: 'online', status: 'bridge troll survivor' }] }]} />}
        asideLabel="members" asideOpen onAsideClose={noop}>{log}</AppShell>
    </Phone>
  ),
};

// web3.7 (flow 11): a thread on a phone is a full-screen sheet through the aside drawer. The sheet carries its own PhoneHeader,
// so the shell is given no close label (it would draw a second header).
const threadRoot = <MessageRow author="ada" time="21:02" body="anyone up for a round tonight? the second boss hits hard, so bring the tank gear" state="ok" thread={{ label: '3 replies', onOpen: noop }} />;
const threadReplies = (
  <MessageLog label="replies" emptyLabel="No replies yet.">
    <MessageRow author="björn" time="21:03" body="after nine, still on the boat" state="ok" tag="web" />
    <MessageRow author="mira" time="21:04" body="count me in" state="ok" own />
    <MessageRow author="ada" time="21:06" body="bring the tank gear, then" state="ok" />
  </MessageLog>
);
const threadComposer = (
  <Composer label="reply in thread" placeholder="reply in thread" maxLength={4000} value="" onChange={noop} onSend={noop} sendLabel="send" counterLabel={n => `${n} left`} />
);
export const ThreadSheet: Story = {
  render: () => (
    <Phone>
      <AppShell {...common} layout="phone" phonePane="view" tabBar={tabBar('servers')} rail={rail} sidebar={serverChannels} header={header} composer={composer}
        aside={<ThreadPane title="Thread" closeLabel="close thread" onClose={noop} root={threadRoot} composer={threadComposer} phone>{threadReplies}</ThreadPane>}
        asideLabel="thread" asideOpen onAsideClose={noop}>{log}</AppShell>
    </Phone>
  ),
};

// Final fix I3: a call arrives while a thread sheet is open. The ring stays above the sheet and can be answered.
export const RingOverThreadSheet: Story = {
  render: () => (
    <Phone>
      <AppShell {...common} layout="phone" phonePane="view" tabBar={tabBar('servers')} rail={rail} sidebar={serverChannels} header={header} composer={composer} ring={ring}
        aside={<ThreadPane title="Thread" closeLabel="close thread" onClose={noop} root={threadRoot} composer={threadComposer} phone>{threadReplies}</ThreadPane>}
        asideLabel="thread" asideOpen onAsideClose={noop}>{log}</AppShell>
    </Phone>
  ),
};

// Task 7: the phone conversation as the web composes it in Signal: the phone channel header (Threads, Members, search; Ruling U),
// the #general feed and the composer (packages/web/src/screens/Shell.tsx, MessageItem.tsx, ComposerArea.tsx).
const signalPhoneHeader = <ChannelHeader name="general" topic="raids, plans and the bridge troll" phone={{ backLabel: 'back to channels', onBack: noop, menu: <>
  <Button variant="ghost" mark={false} aria-label="Threads" pressed={false}><Icon name="messages-square" size={14} /></Button>
  <Tooltip label="Members"><Button variant="ghost" mark={false} aria-label="Members, 6"><Icon name="users" size={14} /><span aria-hidden="true">6</span></Button></Tooltip>
  <IconButton icon="search" size={28} label="search" />
</> }} />;
export const SignalConversation: Story = {
  globals: { theme: 'signal' },
  render: () => <Phone><AppShell {...common} layout="phone" phonePane="view" tabBar={signalTabBar('servers')} rail={signalRail} sidebar={signalChannels}
    header={signalPhoneHeader} composer={<SignalComposer gif={{ label: 'add a gif', open: false, onToggle: noop }} />}><GeneralLog /></AppShell></Phone>,
};

// Task 11: Björn's thread on a phone, as Shell.tsx mounts ThreadView in the aside (the full-screen sheet) over #general.
export const SignalThread: Story = {
  globals: { theme: 'signal' },
  render: () => <Phone><AppShell {...common} layout="phone" phonePane="view" tabBar={signalTabBar('servers')} rail={signalRail} sidebar={signalChannels}
    header={signalPhoneHeader} composer={<SignalComposer gif={{ label: 'add a gif', open: false, onToggle: noop }} />}
    aside={<GeneralThread phone />} asideLabel="Thread" asideOpen onAsideClose={noop}><GeneralLog /></AppShell></Phone>,
};

// Task 8 (rule 3e): the phone on the joined voice channel harbour as Shell.tsx composes it: the phone voice header (back, the
// name and "6 in voice"), VoiceView's stage without its own header (a focused share over the 150px filmstrip, the call bar's
// icons centred), the tab bar.
const voiceHeader = <ChannelHeader kind="voice" name="harbour" topic="6 in voice" phone={{ backLabel: 'back to channels', onBack: noop }} />;
export const SignalVoice: Story = {
  globals: { theme: 'signal' },
  render: () => <Phone><AppShell {...common} layout="phone" phonePane="view" tabBar={signalTabBar('servers')} rail={signalRail} sidebar={signalChannels}
    header={voiceHeader}><div className="dw-voice-channel"><div className="dw-voice-stage" data-call-phase="in">
      <HarbourStage header={false} tiles={harbourTiles('Björn')} focus="d-Ada:screen" nowPlaying={harbourStageBar} /></div></div></AppShell></Phone>,
};
export const SignalVoiceGrid: Story = {
  globals: { theme: 'signal' },
  render: () => <Phone><AppShell {...common} layout="phone" phonePane="view" tabBar={signalTabBar('servers')} rail={signalRail} sidebar={signalChannels}
    header={voiceHeader}><div className="dw-voice-channel"><div className="dw-voice-stage" data-call-phase="in">
      <HarbourStage header={false} tiles={harbourTiles()} focus={null} nowPlaying={harbourStageBar} /></div></div></AppShell></Phone>,
};
