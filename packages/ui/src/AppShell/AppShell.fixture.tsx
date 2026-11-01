import { useState, type ReactNode } from 'react';
import { AppShell, type AppShellProps } from './AppShell.tsx';
import { CommunityRail } from '../CommunityRail/CommunityRail.tsx';
import { ChannelList } from '../ChannelList/ChannelList.tsx';
import { ChannelHeader } from '../ChannelHeader/ChannelHeader.tsx';
import { DateDivider, MessageLog } from '../MessageLog/MessageLog.tsx';
import { MessageRow } from '../MessageRow/MessageRow.tsx';
import { Composer } from '../Composer/Composer.tsx';
import { GeneralLog, Row, SignalComposer } from '../MessageRow/conversation.fixture.tsx';
import { IconButton } from '../IconButton/IconButton.tsx';
import { Banner } from '../Banner/Banner.tsx';
import { TopBar } from '../TopBar/TopBar.tsx';
import { Button } from '../Button/Button.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { Tooltip } from '../Tooltip/Tooltip.tsx';
import { UserPanel } from '../UserPanel/UserPanel.tsx';
import { MemberList } from '../MemberList/MemberList.tsx';
import type { VoiceParticipantView } from '../CallTile/CallTile.tsx';
import { ActionMenu } from '../ActionMenu/ActionMenu.tsx';
import { DMS, PEOPLE, YOU } from '../internal/sample/midgard.ts';

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

const noop = () => {};

/** Signal task 5: the desktop top bar; a story or a test that wants it passes this as `shell.topBar`. */
export const fixtureTopBar = (
  <TopBar searchLabel="Search or jump to…" searchShortcut="⌘K" onSearch={noop} remindersDue={0} onReminders={noop} onSettings={noop}
    labels={{ reminders: 'Reminders', settings: 'Open settings', voice: 'open the call in harbour' }} />
);

const rowLabel = (c: { name: string; unread: number; mentions: number; muted: boolean }) =>
  c.muted ? `${c.name}, muted, mentions ${c.mentions}` : `${c.name}, unread ${c.unread}, mentions ${c.mentions}`;
const itemLabel = (i: { name: string; unread: number; mentions: number }) => `${i.name}, unread ${i.unread}, mentions ${i.mentions}`;

// The composed shell for the stories and the focus-order test; every string is a story string.
// The Composer is controlled (L-UI-13): the fixture holds its text and clears it on send, as the
// shell (task 24) does once a send resolves.
export function ComposedShell({ banner = false, ring, shell }: { banner?: boolean; ring?: ReactNode; shell?: Partial<AppShellProps> }) {
  const [draft, setDraft] = useState('');
  const [tab, setTab] = useState('channels');
  return (
    <AppShell
      skipLabel="skip to messages"
      ring={ring}
      {...shell}
      banner={banner ? <Banner tone="warn" action={{ label: 'retry now', onAction: noop }}>offline · reconnecting to dilla.thim.dev</Banner> : shell?.banner}
      rail={<CommunityRail label="servers" activeId={tab === 'dms' ? null : 'a1'} onSelect={() => setTab('channels')}
        home={{ label: 'direct messages', itemLabel: 'direct messages, unread 2, mentions 0', selected: tab === 'dms', unread: 2, mentions: 0,
          onSelect: () => setTab('dms') }} addLabel="add a server" onAdd={noop}
        settingsLabel="settings" onSettings={noop} itemLabel={itemLabel}
        items={[{ id: 'a1', name: 'Midgard Crew' }, { id: 'b2', name: 'Night Owls', unread: 4 }, { id: 'c3', name: 'raid planning', unread: 2, mentions: 2 }]} />}
      sidebar={<ChannelList label={tab === 'channels' ? 'channels' : 'direct messages'} title="Midgard Crew"
        activeId={tab === 'channels' ? 'g' : null} onSelect={noop} emptyLabel="No channels yet." rowLabel={rowLabel}
        footer={tab === 'dms' ? <Button variant="ghost" size="sm">message someone</Button> : undefined}
        channels={tab === 'channels'
          ? [{ id: 'g', name: 'general', kind: 'text', readable: false }, { id: 'l', name: 'lfg', kind: 'text', readable: true, unread: 3 },
            { id: 't', name: 'loot', kind: 'text', readable: false, unread: 12, mentions: 2 }, { id: 'r', name: 'random', kind: 'text', readable: false, unread: 7, muted: true },
            { id: 'v', name: 'longhouse', kind: 'voice', readable: false }]
          : [{ id: 'd1', name: 'ada', kind: 'dm', readable: false, unread: 2 }, { id: 'd2', name: 'björn', kind: 'dm', readable: false }]} />}
      header={<ChannelHeader name="general" topic="evening plans, screenshots and the odd argument" />}
      composer={<Composer label="message #general" placeholder="message #general" maxLength={4000} value={draft} onChange={setDraft}
        onSend={() => setDraft('')} sendLabel="send" counterLabel={n => `${n} left`} />}
    >
      <MessageLog label="messages in #general" emptyLabel="No messages yet.">
        <MessageRow author="ada" time="21:02" body="anyone up for a round tonight?" state="ok" />
        <MessageRow author="björn" time="21:03" body="after nine, still on the boat" state="ok" tag="web" />
        <MessageRow author="mira" time="21:04" body="count me in" state="ok" own />
        <MessageRow author="loot-bot" time="21:05" body="weekly reset in 2 h" state="ok" tag="bot" />
      </MessageLog>
    </AppShell>
  );
}

/**
 * Task 6b fix round 2 (restyle rule 3d): the whole desktop frame composed the way packages/web/src/screens/Shell.tsx composes it
 * in Signal: the rail with the @ home tile, the server list (the 26px ⌄ head, the stat line, categories, the voice rows, AFK),
 * the channel header with Threads and Members (with the count, Ruling X), the user panel without a gear (its line under the
 * name), and the members aside. The messages, the composer and the member list are later tasks' (7, 9) and the people are
 * sample data (Task 11).
 */
/** The prototype's harbour occupants as the sidebar row lists them (VoiceView's participantView: web false, the bot's tag). */
export const harbourPeople = (joined: boolean): VoiceParticipantView[] => [
  // Björn speaks while you are in the call, as the stage shows (Shell.tsx passes the speaking devices of your call's room only).
  ...[['Ada', { screen: true }], ['Björn', { video: true, speaking: true }], ['Kari', { video: true, screen: true }], ['Ole', {}]] as const,
  ...(joined ? [['Mira', {}]] as const : []), ['Glenn', { bot: true }] as const,
].map(([name, more]) => ({ deviceId: `d-${name}`, userId: `u-${name}`, name, state: 'ok' as const, muted: false, deafened: false,
  video: false, screen: false, web: false, bot: false, ...more, speaking: joined && 'speaking' in more }));

/**
 * `voice` (task 8): the desktop frame on the voice channel harbour as Shell.tsx composes it there: the harbour row current with
 * its people (Mira among them once joined), no channel header (the stage carries the voice header), no composer, no aside,
 * the user panel; `main` is the stage.
 */
/**
 * `dm` (Task 11): the desktop frame on the @ home with Ada's DM open, as Shell.tsx composes it there: the rail's @ selected, the
 * DM list (its plain title "direct messages", the ✎ "message someone" and the ⌄ of the DM default, one row per DM), the DM header
 * (@ Ada, "@ada · planning the raid"), the log with its @ welcome and the DM composer.
 */
function DmHeadMenu() {
  const [open, setOpen] = useState(false);
  return <ActionMenu label="Direct messages menu" open={open} onOpenChange={setOpen} triggerClassName="d-channel-list__head-btn"
    triggerContent={<Icon name="chevron-down" size={15} />} onSelect={() => setOpen(false)}
    items={[{ id: 'dms-all', label: 'All messages', checked: true, group: 'Notifications' }, { id: 'dms-mentions', label: 'Mentions only', checked: false, group: 'Notifications' }]} />;
}
function DmLog() {
  const ada = DMS[0];
  return (
    <MessageLog label="messages with Ada" emptyLabel="No messages yet." welcome={{ icon: 'at-sign', title: ada.name, body: 'This is the start of your conversation.' }}>
      <DateDivider label="today" />
      {ada.messages.map(m => <Row key={m.time} author={m.author} time={m.time} body={m.body} nameColor={0xE9B6EC} />)}
    </MessageLog>
  );
}

export function SignalShell({ shell, voice, main, dm = false }: { shell?: Partial<AppShellProps>; voice?: { joined: boolean }; main?: ReactNode; dm?: boolean }) {
  const threadOpen = shell?.asideKind === 'thread' && shell.asideOpen === true;
  const membersOpen = shell?.asideOpen === true && !threadOpen;
  return (
    <AppShell
      skipLabel="skip to messages"
      {...shell}
      rail={<CommunityRail label="servers" activeId={dm ? null : 'a1'} onSelect={noop}
        home={{ label: 'direct messages', itemLabel: 'direct messages, unread 0, mentions 0', selected: dm, unread: 0, mentions: 0, onSelect: noop }}
        addLabel="add a server" onAdd={noop} itemLabel={itemLabel}
        items={[{ id: 'a1', name: 'Midgard Crew' }, { id: 'b2', name: 'Valhalla Kitchen', unread: 2, mentions: 2 }]} />}
      sidebar={dm ? (
        // Task 11 (Q2): Shell.tsx's DM list: the serif title, "2 online · 3 conversations", DIRECT MESSAGES, a row per DM with its
        // person (status for one person, the members for a group); Björn's DM is unread.
        <ChannelList label="direct messages" title="Direct messages" activeId="ada" onSelect={noop} emptyLabel="no direct messages yet" rowLabel={rowLabel}
          headerAction={<><IconButton icon="square-pen" size={28} label="message someone" onClick={noop} /><DmHeadMenu /></>}
          stats={[{ text: '2 online', live: true }, { text: '3 conversations' }]} sectionLabel="Direct messages"
          channels={DMS.map(d => ({ id: d.id, name: d.name, kind: 'dm' as const, readable: false, ...(d.id === 'bjorn' ? { unread: 1 } : {}),
            person: d.id === 'crew' ? { group: true, sub: 'Ada, Björn, you', initials: 'RC' }
              : { presence: 'online' as const, sub: d.id === 'ada' ? PEOPLE.Ada.status : PEOPLE['Björn'].status } }))} />
      ) : <ChannelList label="channels" title="Midgard Crew" activeId={voice !== undefined ? 'harbour' : 'general'} onSelect={noop} emptyLabel="No channels yet." rowLabel={rowLabel}
        headerAction={<ServerHeadMenu />}
        stats={[{ text: '3 online', live: true }, { text: voice?.joined === true ? '6 in voice' : '5 in voice' }]} newLabel={n => `${n} new`}
        categories={[{ id: 'raids', name: 'raids', collapsed: false }, { id: 'boats', name: 'boats', collapsed: false }]} onToggleCategory={noop}
        voice={{ labels: { open: 'voice {name}', count: '{n} in voice', playbackOnly: 'playback only', join: 'join voice', joined: 'connected',
          state: { speaking: 'speaking', muted: 'muted', deafened: 'deafened', notHeard: 'not heard yet', unverified: 'unverified stream', connecting: 'connecting',
            video: 'camera on', screen: 'sharing screen', web: 'web', bot: 'bot', noSound: 'no sound' } },
          rooms: { hv: { joined: false, participants: [] }, afk: { joined: false, participants: [] },
            // Task 11 (README §11 sample): harbour's people show on every screen, as the prototype's sidebar lists them.
            harbour: { joined: voice?.joined === true, participants: harbourPeople(voice?.joined === true) } } }}
        channels={[
          { id: 'general', name: 'general', kind: 'text', readable: false }, { id: 'hv', name: 'general', kind: 'voice', readable: false },
          { id: 'loot', name: 'loot', kind: 'text', readable: false, parentId: 'raids', unread: 1 },
          { id: 'harbour', name: 'harbour', kind: 'voice', readable: false, parentId: 'boats' }, { id: 'afk', name: 'AFK', kind: 'voice', readable: false, afk: true },
        ]} />}
      header={voice !== undefined ? null : dm ? <ChannelHeader kind="dm" name="Ada" topic="@ada · planning the raid" /> : <ChannelHeader name="general" topic="raids, plans and the bridge troll" actions={<>
        <Button variant="ghost" mark={false} data-thread-list-toggle="" pressed={threadOpen}><Icon name="messages-square" size={14} />Threads</Button>
        <Tooltip label="Members">
          <Button variant="ghost" mark={false} data-members-toggle="" aria-label="Members, 6" pressed={membersOpen}><Icon name="users" size={14} /><span aria-hidden="true">6</span></Button>
        </Tooltip>
      </>} />}
      bottom={<UserPanel name={YOU.name} username={YOU.handle} line={voice?.joined === true ? 'in harbour' : YOU.status} muted={false} deafened={false} onMute={noop} onDeafen={noop}
        labels={{ region: 'your account', mute: 'mute', deafen: 'deafen' }} shortcuts={{ mute: 'Ctrl+Shift+M', deafen: 'Ctrl+Shift+D' }}
        status={{ current: 'online', label: 'your status: online', onChoose: noop,
          items: [{ id: 'online', label: 'online' }, { id: 'idle', label: 'idle' }, { id: 'dnd', label: 'do not disturb' }, { id: 'invisible', label: 'invisible' }] }} />}
      composer={voice !== undefined ? null : dm ? <SignalComposer label="message @Ada" placeholder="Message Ada" gif={{ label: 'add a gif', open: false, onToggle: noop }} />
        : <SignalComposer gif={{ label: 'add a gif', open: false, onToggle: noop }} />}
      aside={shell?.aside === undefined ? undefined : shell.asideKind === 'thread' ? shell.aside : (
        // Task 9 (rule 3d): the members aside as MemberPane composes it on a desktop: "Members" with the close ×, role groups, the
        // presence group, the bots group, each row with its status line (a custom status, "in voice", or the presence word).
        <MemberList label="Members" closeLabel="close members" onClose={noop} loading={false} loadingLabel="Loading members…" emptyLabel="No members yet."
          ownerLabel="owner" botLabel="bot" onOpen={noop} onMenu={noop}
          groups={[
            { id: 'raid', kind: 'role', title: 'raid lead', color: 0xe9b6ec, members: [
              { id: 'u1', name: 'Ada', isOwner: true, color: 0xe9b6ec, presence: 'online', status: 'planning the raid' },
              { id: 'u5', name: 'Mira', color: 0xe9b6ec, presence: 'online', status: 'online' },
            ] },
            { id: 'members', kind: 'online', title: 'online', members: [
              { id: 'u2', name: 'Björn', presence: 'online', status: 'bridge troll survivor' },
              { id: 'u3', name: 'Kari', presence: 'online', status: 'streaming' },
              { id: 'u4', name: 'Ole', presence: 'online', status: 'afk-ish' },
            ] },
            { id: 'bots', kind: 'bots', title: 'bots', members: [{ id: 'g1', name: 'Glenn', isBot: true, presence: 'online', status: 'playing in harbour' }] },
          ]} />
      )}
    >
      {/* Task 7: the conversation as packages/web composes it (MessageItem, ComposerArea; conversation.fixture.tsx). */}
      {main ?? (dm ? <DmLog /> : <GeneralLog />)}
    </AppShell>
  );
}

