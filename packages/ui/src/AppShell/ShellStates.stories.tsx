import type { Meta, StoryObj } from '@storybook/react-vite';
import { useState, type ReactNode } from 'react';
import { AppShell, type AppShellProps } from './AppShell.tsx';
import { CommunityRail } from '../CommunityRail/CommunityRail.tsx';
import { ChannelList } from '../ChannelList/ChannelList.tsx';
import { ChannelHeader } from '../ChannelHeader/ChannelHeader.tsx';
import { MessageLog } from '../MessageLog/MessageLog.tsx';
import { MessageRow, type MessageRowProps } from '../MessageRow/MessageRow.tsx';
import { Composer } from '../Composer/Composer.tsx';
import { EmptyState } from '../EmptyState/EmptyState.tsx';
import { Banner } from '../Banner/Banner.tsx';
import { Dialog } from '../Dialog/Dialog.tsx';
import { TextField } from '../TextField/TextField.tsx';
import { Button } from '../Button/Button.tsx';
import { TopBar } from '../TopBar/TopBar.tsx';
// The shipped copy of @dilla/web, so the F16 screenshots show exactly what the app says.
import { t, formatTime } from '../../../web/src/strings/index.ts';

const meta = { title: 'Screens/Shell', parameters: { layout: 'fullscreen' } } satisfies Meta;
export default meta;
type Story = StoryObj<typeof meta>;

const noop = () => {};
const time = formatTime(1_790_000_000);
const SERVERS = [{ id: 'a', name: 'Midgard' }, { id: 'b', name: 'Valhalla' }];
const CHANNELS = [
  { id: 'lounge', name: 'lounge', kind: 'voice' as const, readable: false },
  { id: 'general', name: 'general', kind: 'text' as const, readable: false },
  { id: 'random', name: 'random', kind: 'text' as const, readable: false },
  { id: 'lobby', name: 'lobby', kind: 'text' as const, readable: true },
];
const ROWS: MessageRowProps[] = [
  { author: 'bob', time, body: 'anyone up for a round tonight?', state: 'ok' },
  { author: 'Helper', time, body: 'Reminder: the server restarts at 22:00.', tag: 'bot', state: 'ok' },
  { author: 'Ada L', time, body: 'in ten, setting up', own: true, tag: 'web', state: 'ok' },
  { author: t('shell.message.unknownAuthor', { id: '29292929' }), time, body: '', state: 'cannot-read', stateLabel: t('shell.message.cannotRead'), detail: 'E_SENDER_MISMATCH' },
  { author: 'Ada L', time, body: 'bringing snacks', own: true, tag: 'web', state: 'pending', stateLabel: t('shell.message.pending') },
  { author: 'Ada L', time, body: 'and the spare headset', own: true, tag: 'web', state: 'failed', stateLabel: t('shell.message.failed'), detail: 'E_NETWORK',
    actions: [{ label: t('shell.message.retry'), onAction: noop }, { label: t('shell.message.discard'), onAction: noop }] },
];

const BADGES: Record<string, { unread: number; mentions: number; muted?: boolean }> = {
  random: { unread: 3, mentions: 0 }, lobby: { unread: 0, mentions: 1, muted: true },
};
const DMS = [
  { id: 'bob', name: 'bob', kind: 'dm' as const, readable: false, unread: 2, mentions: 0 },
  { id: 'eve', name: 'eve', kind: 'dm' as const, readable: false },
];
const rowLabel = (c: { name: string; unread: number; mentions: number; muted: boolean }) =>
  c.muted ? t('shell.channels.rowLabelMuted', { name: c.name, mentions: c.mentions })
    : c.unread === 0 && c.mentions === 0 ? c.name : t('shell.channels.rowLabel', { name: c.name, unread: c.unread, mentions: c.mentions });
const itemLabel = (i: { name: string; unread: number; mentions: number }) =>
  i.unread === 0 && i.mentions === 0 ? i.name : t('shell.rail.itemLabel', { name: i.name, unread: i.unread, mentions: i.mentions });

function Screen(p: { resize?: Pick<AppShellProps, 'paneWidths' | 'onPaneResize' | 'resizeLabels'>; active?: string | null; servers?: { id: string; name: string }[]; main: ReactNode; reason?: string | null;
  banner?: ReactNode; status?: 'online' | 'connecting' | 'offline'; after?: ReactNode; tab?: 'channels' | 'dms'; badges?: boolean; dm?: boolean }) {
  const servers = p.servers ?? SERVERS;
  const active = p.active === undefined ? 'general' : p.active;
  const tab = p.tab ?? 'channels';
  const channel = p.dm ? { id: 'bob', name: 'bob', kind: 'text' as const, readable: false } : CHANNELS.find(c => c.id === active) ?? null;
  // 800px less the preview decorator's 16px padding on each side, as the Shell/AppShell stories do. The link state is the
  // banner's to say (`status` is kept for the stories that name it); the Signal top bar has no link chunk.
  return (
    <div style={{ ['--d-shell-h' as string]: '768px' }}>
      <AppShell
        skipLabel={t('shell.skip')}
        rail={<CommunityRail label={t('shell.rail.label')} items={servers.map(s => (p.badges && s.id === 'b' ? { ...s, unread: 2, mentions: 1 } : s))}
          activeId={servers[0]?.id ?? null} onSelect={noop} addLabel={t('layout.rail.add')} onAdd={noop}
          settingsLabel={t('shell.rail.settings')} onSettings={noop} itemLabel={itemLabel} />}
        sidebar={servers.length ? (
          <ChannelList title="Midgard"
            label={t(tab === 'dms' ? 'shell.dms.label' : 'shell.channels.label')}
            channels={tab === 'dms' ? DMS : CHANNELS.map(c => ({ ...c, ...(p.badges ? BADGES[c.id] : {}) }))}
            activeId={tab === 'dms' ? (p.dm ? 'bob' : null) : active} onSelect={noop}
            emptyLabel={t(tab === 'dms' ? 'shell.dms.empty' : 'shell.channels.empty')} rowLabel={rowLabel}
            footer={tab === 'dms' ? <Button variant="ghost">{t('shell.dms.new')}</Button> : undefined} />
        ) : null}
        header={channel ? <ChannelHeader kind={p.dm ? 'dm' : 'channel'} name={channel.name} topic={channel.id === 'general' ? 'say hi' : undefined} readable={channel.readable} readableLabel={t('shell.readable')} /> : null}
        // The Composer is controlled (task 21); a story holds no text, so it passes an empty value.
        composer={channel ? <Composer label={t(p.dm ? 'shell.dm.composer' : 'shell.composer.label', p.dm ? { name: 'bob' } : { channel: channel.name })}
          placeholder={t(p.dm ? 'shell.dm.composer' : 'shell.composer.placeholder', p.dm ? { name: 'bob' } : { channel: channel.name })}
          maxLength={4000} value="" onChange={noop} disabled={Boolean(p.reason)} disabledReason={p.reason ?? undefined} onSend={noop} sendLabel={t('shell.composer.send')}
          counterLabel={n => (n < 0 ? t('shell.composer.over', { n: -n }) : t('shell.composer.remaining', { n }))} /> : null}
        topBar={<TopBar searchLabel={t('topbar.search')} searchShortcut="⌘K" onSearch={noop} remindersDue={0} onReminders={noop} onSettings={noop}
        labels={{ reminders: t('reminder.inbox'), settings: t('workspace.open'), voice: t('nav.call.open', { where: 'harbour' }) }} />}
        banner={p.banner}
        {...p.resize}
      >
        {p.main}
      </AppShell>
      {p.after}
    </div>
  );
}
const conversation = (
  <MessageLog label={t('shell.log.label', { channel: 'general' })} emptyLabel={t('shell.log.empty')} earlier={{ label: t('shell.log.earlier'), onLoad: noop }}>
    {ROWS.map((r, i) => <MessageRow key={i} {...r} />)}
  </MessageLog>
);
const empty = <MessageLog label={t('shell.log.label', { channel: 'general' })} emptyLabel={t('shell.log.empty')}>{null}</MessageLog>;

export const Conversation: Story = { render: () => <Screen main={conversation} /> };
export const Joining: Story = { render: () => <Screen main={empty} reason={t('shell.composer.joining')} /> };
export const NotMember: Story = { render: () => <Screen main={conversation} reason={t('shell.composer.notMember')} /> };
// A voice channel shows the voice view since web-3: Screens/Voice draws it.
export const NoServer: Story = {
  render: () => <Screen servers={[]} active={null}
    main={<EmptyState title={t('shell.noServer.title')} body={t('shell.noServer.body')} action={{ label: t('shell.noServer.action'), onAction: noop }} />} />,
};
export const Offline: Story = {
  render: () => <Screen main={conversation} status="offline" banner={<Banner tone="warn">{t('shell.banner.offline')}</Banner>} />,
};
export const CommandError: Story = {
  render: () => <Screen main={conversation}
    banner={<Banner tone="danger" action={{ label: t('shell.banner.dismiss'), onAction: noop }}>{t('shell.banner.commandError', { code: 'E_NOT_READY' })}</Banner>} />,
};
export const JoinDialog: Story = {
  render: () => <Screen main={conversation} after={
    <Dialog open title={t('join.title')} onClose={noop}
      footer={<Button variant="accent" type="submit" form="story-join">{t('join.submit')}</Button>}>
      <form id="story-join" noValidate onSubmit={e => e.preventDefault()}>
        <TextField id="story-join-invite" label={t('join.invite')} value="https://dilla.test/i/ABCD-EFGH" onChange={noop}
          hint={t('join.inviteHint')} error={t('join.error.notCommunity')} />
      </form>
    </Dialog>} />,
};

const dmLog = (
  <MessageLog label={t('shell.dm.log', { name: 'bob' })} emptyLabel={t('shell.log.empty')}>
    <MessageRow author="bob" time={time} body="are you around later?" state="ok" />
    <MessageRow author="Ada L" time={time} body="after nine" own tag="web" state="ok" />
  </MessageLog>
);
export const Badges: Story = { render: () => <Screen main={conversation} badges /> };
export const DirectMessages: Story = { render: () => <Screen main={conversation} tab="dms" badges /> };
export const DmConversation: Story = { render: () => <Screen main={dmLog} tab="dms" dm /> };
export const DmPicker: Story = {
  render: () => <Screen main={conversation} tab="dms" after={
    <Dialog open title={t('shell.dms.newTitle')} onClose={noop} closeLabel={t('dialog.close')}>
      <p>{t('shell.dms.newBody')}</p>
      <ul style={{ listStyle: 'none', margin: 0, padding: 0 }}>
        {['bob', 'Helga', 'zed'].map(name => (
          <li key={name} style={{ display: 'flex', justifyContent: 'space-between', gap: 'var(--row-gap)' }}>
            <span id={`story-dm-${name}`}>{name}</span>
            <Button size="sm" aria-describedby={`story-dm-${name}`}>{t('shell.dms.open')}</Button>
          </li>
        ))}
      </ul>
    </Dialog>} />,
};

// web3.8 (U2): the two borders can be dragged or keyed. The story holds the widths the app would keep. The copy is story text:
// the shipped `pane.` strings come with the web wiring.
function ResizableScreen() {
  const [widths, setWidths] = useState<{ sidebar?: number; aside?: number }>({ sidebar: 288 });
  return (
    <Screen main={conversation} resize={{
      paneWidths: widths,
      onPaneResize: (pane, px) => setWidths(w => ({ ...w, [pane]: px ?? undefined })),
      resizeLabels: { sidebar: 'resize the channel list', aside: 'resize the member list' },
    }} />
  );
}
export const Resizable: Story = { render: () => <ResizableScreen /> };
