import { useEffect, useRef, type ReactNode } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { AppShell } from './AppShell.tsx';
import { CommunityRail } from '../CommunityRail/CommunityRail.tsx';
import { ChannelList } from '../ChannelList/ChannelList.tsx';
import { ChannelHeader } from '../ChannelHeader/ChannelHeader.tsx';
import { MessageLog } from '../MessageLog/MessageLog.tsx';
import { MessageRow } from '../MessageRow/MessageRow.tsx';
import { MessageToolbar } from '../MessageToolbar/MessageToolbar.tsx';
import { ReactionBar } from '../ReactionBar/ReactionBar.tsx';
import { MessageEditor } from '../MessageEditor/MessageEditor.tsx';
import { AttachmentCard } from '../AttachmentCard/AttachmentCard.tsx';
import { AttachmentTray } from '../AttachmentTray/AttachmentTray.tsx';
import { DropOverlay } from '../DropOverlay/DropOverlay.tsx';
import { ReplyChip } from '../ReplyChip/ReplyChip.tsx';
import { MentionList } from '../MentionList/MentionList.tsx';
import { PinsDialog } from '../PinsDialog/PinsDialog.tsx';
import { Lightbox as LightboxView } from '../Lightbox/Lightbox.tsx';
import { Composer } from '../Composer/Composer.tsx';
import { Dialog } from '../Dialog/Dialog.tsx';
import { Banner } from '../Banner/Banner.tsx';
import { Button } from '../Button/Button.tsx';
import { TopBar } from '../TopBar/TopBar.tsx';
import { t } from '../../../web/src/strings/index.ts';
import { reactionName } from '../../../web/src/emoji.ts';

const meta = { title: 'Screens/Conversation', parameters: { layout: 'fullscreen' } } satisfies Meta;
export default meta;
type Story = StoryObj<typeof meta>;
const noop = () => {};
const time = '21:02';
const counter = (n: number) => n < 0 ? t('shell.composer.over', { n: -n }) : t('shell.composer.remaining', { n });
const rowLabel = (c: { name: string }) => c.name;
const itemLabel = (c: { name: string }) => c.name;

function FocusActiveRow({ children }: { children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => { ref.current?.querySelector<HTMLElement>('.d-message-row[data-active="true"]')?.focus(); }, []);
  return <div ref={ref}>{children}</div>;
}

function Screen({ main, composer, after }: { main: ReactNode; composer?: ReactNode; after?: ReactNode }) {
  return <div style={{ ['--d-shell-h' as string]: '768px' }}>
    <AppShell skipLabel={t('shell.skip')}
      rail={<CommunityRail label={t('shell.rail.label')} items={[{ id: 'a', name: 'Midgard' }]} activeId="a" onSelect={noop}
        addLabel={t('layout.rail.add')} onAdd={noop} settingsLabel={t('shell.rail.settings')} onSettings={noop} itemLabel={itemLabel} />}
      sidebar={<ChannelList label={t('shell.channels.label')} title="Midgard" activeId="general" onSelect={noop}
        emptyLabel={t('shell.channels.empty')} rowLabel={rowLabel}
        channels={[{ id: 'general', name: 'general', kind: 'text', readable: false }, { id: 'random', name: 'random', kind: 'text', readable: false }]} />}
      header={<ChannelHeader name="general" topic="say hi" actions={<Button variant="ghost" size="md">{t('shell.pins.open')}</Button>} />}
      composer={composer ?? <Composer label={t('shell.composer.label', { channel: 'general' })}
        placeholder={t('shell.composer.placeholder', { channel: 'general' })} maxLength={4000} value="" onChange={noop}
        onSend={noop} sendLabel={t('shell.composer.send')} counterLabel={counter} attachLabel={t('shell.composer.attach')} onAttach={noop} />}
      topBar={<TopBar searchLabel={t('topbar.search')} searchShortcut="⌘K" onSearch={noop} remindersDue={0} onReminders={noop} onSettings={noop}
        labels={{ reminders: t('reminder.inbox'), settings: t('workspace.open'), voice: t('nav.call.open', { where: 'harbour' }) }} />}>
      {main}
    </AppShell>{after}
  </div>;
}

const toolbar = <MessageToolbar moreLabel="more actions" label={t('shell.message.toolbar')} tabbable items={[
  { id: 'react', label: t('shell.message.react'), onAction: noop },
  { id: 'reply', label: t('shell.message.reply'), onAction: noop },
  { id: 'edit', label: t('shell.message.edit'), onAction: noop },
  { id: 'pin', label: t('shell.message.unpin'), onAction: noop },
  { id: 'delete', label: t('shell.message.delete'), danger: true, onAction: noop },
]} />;
const attachments = <div className="dw-attachments">
  <AttachmentCard kind="image" name="map.png" size="1.2 KB" thumbUrl={null} w={640} h={480}
    openLabel={t('shell.attachment.open', { name: 'map.png' })} onOpen={noop}
    saveLabel={t('shell.attachment.save', { name: 'map.png' })} onSave={noop} state="idle" tabbable />
  <AttachmentCard kind="file" name="data.bin" size="68 KB" thumbUrl={null} w={null} h={null}
    openLabel={t('shell.attachment.open', { name: 'data.bin' })} saveLabel={t('shell.attachment.save', { name: 'data.bin' })}
    onSave={noop} state="idle" tabbable />
</div>;
const reactions = <ReactionBar label={t('shell.message.reactions')} tabbable
  items={[{ emoji: '👍', count: 3, mine: true, name: t('shell.message.reaction', { name: reactionName('👍'), count: 3 }) },
    { emoji: '🎉', count: 1, mine: false, name: t('shell.message.reaction', { name: reactionName('🎉'), count: 1 }) }]}
  onToggle={noop} addLabel={t('shell.message.reactionAdd')} onAdd={noop} />;

function FeedRows() {
  return <FocusActiveRow><MessageLog label={t('shell.log.label', { channel: 'general' })} emptyLabel={t('shell.log.empty')}>
    <MessageRow author="bob" time={time} body="the original" state="ok" />
    <MessageRow author="Ada L" time={time} body="the map is here" state="ok" own active editedLabel={t('shell.message.edited')}
      pinnedLabel={t('shell.message.pinned')} toolbar={toolbar} attachments={attachments} reactions={reactions}
      reply={{ label: t('shell.message.replyLabel', { name: 'bob' }), author: 'bob', excerpt: 'the original', state: 'ok',
        jumpLabel: t('shell.message.replyJump'), onJump: noop }} />
    <MessageRow author="bob" time={time} body={[{ kind: 'mention', label: '@Ada L', me: true, broadcast: false },
      { kind: 'text', text: ' and ' }, { kind: 'mention', label: '@everyone', me: true, broadcast: true }]} state="ok" mention />
    <MessageRow author="Ada L" time={time} body="working on it" state="pending" stateLabel={t('shell.message.pending')}
      pendingAction={{ label: t('shell.message.saving'), tone: 'pending' }} />
    <MessageRow author="Ada L" time={time} body="try again" state="ok" pendingAction={{ label: t('shell.message.notSaved'),
      tone: 'failed', actions: [{ label: t('shell.message.retry'), onAction: noop }, { label: t('shell.message.discard'), onAction: noop }] }} />
  </MessageLog></FocusActiveRow>;
}

export const Feed: Story = { render: () => <Screen main={<FeedRows />} /> };
export const Editing: Story = { render: () => <Screen main={<MessageLog label={t('shell.log.label', { channel: 'general' })} emptyLabel={t('shell.log.empty')}>
  <MessageRow author="Ada L" time={time} body="" state="ok" own active editor={<MessageEditor label={t('shell.edit.label')}
    value="the map is here" onChange={noop} onSave={noop} onCancel={noop} hint={t('shell.edit.hint')}
    saveLabel={t('shell.edit.save')} cancelLabel={t('shell.edit.cancel')} maxLength={4000} counterLabel={counter} />} />
</MessageLog>} /> };
export const DeleteDialog: Story = { render: () => <Screen main={<FeedRows />}
  after={<Dialog open title={t('shell.delete.title')} closeLabel={t('shell.delete.cancel')} onClose={noop}
    footer={<Button variant="danger">{t('shell.delete.confirm')}</Button>}>{t('shell.delete.body')}</Dialog>} /> };
const pinItem = (i: number) => ({ id: String(i), author: i % 2 ? 'bob' : 'Ada L', time, excerpt: `message ${i}`,
  pinnedBy: t('shell.pins.by', { name: 'Ada L' }), jumpLabel: t('shell.pins.jump'), onJump: noop,
  unpinLabel: t('shell.message.unpin'), onUnpin: noop });
export const Pins: Story = { render: () => <Screen main={<FeedRows />}
  after={<PinsDialog open title={t('shell.pins.title', { channel: 'general' })} closeLabel={t('shell.pins.close')}
    onClose={noop} emptyLabel={t('shell.pins.empty')} items={[1, 2, 3, 4, 5].map(pinItem)} />} /> };
export const PinsEmpty: Story = { render: () => <Screen main={<FeedRows />}
  after={<PinsDialog open title={t('shell.pins.title', { channel: 'general' })} closeLabel={t('shell.pins.close')}
    onClose={noop} emptyLabel={t('shell.pins.empty')} items={[]} />} /> };
export const Mentions: Story = { render: () => <Screen main={<FeedRows />}
  composer={<Composer label={t('shell.composer.label', { channel: 'general' })} placeholder={t('shell.composer.placeholder', { channel: 'general' })}
    maxLength={4000} value="@" onChange={noop} onSend={noop} sendLabel={t('shell.composer.send')} counterLabel={counter}
    top={<><ReplyChip label={t('shell.composer.replying', { name: 'bob' })} excerpt="the original"
      cancelLabel={t('shell.composer.replyCancel')} onCancel={noop} />
      <MentionList id="story-mentions" label={t('shell.composer.mentions')} activeId="story-bob" onPick={noop} options={[
        { id: 'story-bob', primary: 'bob', secondary: '@bob' }, { id: 'story-mo', primary: 'Mo', secondary: '@mo' },
        { id: 'story-everyone', primary: '@everyone', secondary: t('shell.composer.mentionEveryone') },
      ]} /></>}
    combobox={{ expanded: true, controls: 'story-mentions', activeDescendant: 'story-bob', onKey: () => false }} />} /> };
const phases = ['reading', 'preparing', 'uploading', 'ready', 'failed'] as const;
export const Tray: Story = { render: () => <Screen main={<FeedRows />}
  composer={<Composer label={t('shell.composer.label', { channel: 'general' })} placeholder={t('shell.composer.placeholder', { channel: 'general' })}
    maxLength={4000} value="" onChange={noop} onSend={noop} sendLabel={t('shell.composer.send')} counterLabel={counter}
    top={<><AttachmentTray label={t('shell.tray.label')} items={phases.map((phase, i) => ({
      id: String(i), name: `file-${i}.bin`, size: '1.2 KB', phase: phase === 'failed' ? t('shell.tray.failed', { code: 'E_QUOTA' }) : t(`shell.tray.${phase}`),
      failed: phase === 'failed', step: phase, hint: phase === 'failed' ? t('shell.tray.failedHint') : undefined,
      removeLabel: t('shell.tray.remove', { name: `file-${i}.bin` }), onRemove: noop,
    }))} /><p role="alert" className="dw-tray-error">{t('shell.tray.notReady')}</p></>} />} /> };
export const Lightbox: Story = { render: () => <Screen main={<FeedRows />}
  after={<LightboxView open label={t('shell.lightbox.label', { name: 'map.png', size: '1.2 KB' })} src={null} alt="map.png"
    closeLabel={t('shell.lightbox.close')} onClose={noop} saveLabel={t('shell.lightbox.save')} onSave={noop}
    previous={{ label: t('shell.lightbox.previous'), onPrevious: noop }} next={{ label: t('shell.lightbox.next'), onNext: noop }} />} /> };
export const Dropping: Story = { render: () => <Screen main={<div className="dw-conversation"><FeedRows />
  <DropOverlay active title={t('shell.drop.title')} body={t('shell.drop.body')} /></div>} /> };
export const Narrow: Story = { render: () => <div style={{ width: '22.5rem', height: '46.25rem', overflow: 'hidden' }}>
  <Screen main={<FeedRows />} /></div> };
