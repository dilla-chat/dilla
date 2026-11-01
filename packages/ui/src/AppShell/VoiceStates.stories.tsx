import type { Meta, StoryObj } from '@storybook/react-vite';
import { DOCK_STATS_CONTRAST } from '../VoiceDock/dockStatsContrast.ts';
import type { ReactNode } from 'react';
import { AppShell } from './AppShell.tsx';
import { CommunityRail } from '../CommunityRail/CommunityRail.tsx';
import { ChannelList } from '../ChannelList/ChannelList.tsx';
import { ChannelHeader } from '../ChannelHeader/ChannelHeader.tsx';
import { MessageLog } from '../MessageLog/MessageLog.tsx';
import { MessageRow } from '../MessageRow/MessageRow.tsx';
import { Composer } from '../Composer/Composer.tsx';
import { Button } from '../Button/Button.tsx';
import { IconButton } from '../IconButton/IconButton.tsx';
import { TopBar } from '../TopBar/TopBar.tsx';
import { VoiceDock } from '../VoiceDock/VoiceDock.tsx';
import { CallStage, type CallStageNotice } from '../CallStage/CallStage.tsx';
import { CallControls } from '../CallControls/CallControls.tsx';
import type { CallTileProps, VoiceParticipantView } from '../CallTile/CallTile.tsx';
import { RingBanner } from '../RingBanner/RingBanner.tsx';
import { AttachmentCard } from '../AttachmentCard/AttachmentCard.tsx';
// The shipped copy and the shell's own layout of @dilla/web, so the screenshots show what the app says and draws
// (web-3 task 9: the voice view, the dock, the sidebar's voice sections, the DM call strip, the ring and the players).
import { en, formatTime, t } from '../../../web/src/strings/index.ts';

const meta = { title: 'Screens/Voice', parameters: { layout: 'fullscreen', webStyles: true, ...DOCK_STATS_CONTRAST } } satisfies Meta;
export default meta;
type Story = StoryObj<typeof meta>;

const noop = () => {};
const time = formatTime(1_790_000_000);
const STATE = {
  speaking: t('voice.state.speaking'), muted: t('voice.state.muted'), deafened: t('voice.state.deafened'), notHeard: t('voice.state.notHeard'),
  unverified: t('voice.state.unverified'), connecting: t('voice.state.connecting'), video: t('voice.state.video'), screen: t('voice.state.screen'),
  web: t('voice.tag.web'), bot: t('voice.tag.bot'), noSound: t('voice.state.noSound'),
};
const ROW_LABELS = { open: en['voice.open'], count: en['voice.count'], playbackOnly: t('voice.bot.playbackOnly'), state: STATE };
const person = (name: string, over: Partial<VoiceParticipantView> = {}): VoiceParticipantView => ({
  deviceId: name, userId: name, name, state: 'ok', speaking: false, muted: false, deafened: false, video: false, screen: false, web: true,
  bot: false, ...over,
});
const ADA = person('ada', { speaking: true });
const BO = person('bo', { muted: true });
const HELPER = person('helper', { bot: true, web: false });
const tile = (view: VoiceParticipantView, own = false): CallTileProps => ({
  view, own, volume: 100, media: null,
  labels: { state: STATE, volume: t('voice.volume', { name: view.name }), volumeValue: en['voice.volumeValue'] },
});
const STAGE_LABELS = { code: t('voice.code'), codeHelp: t('voice.codeHelp'), empty: t('voice.empty'), tiles: t('voice.tiles') };
const CODE = '123451234512345123451234512345';

function controls(p: { paused?: boolean; busy?: boolean; dm?: boolean } = {}) {
  return (
    <CallControls muted={false} deafened={false} camera={false} sharing={false} canCamera={p.paused !== true} canShare={p.paused !== true}
      busy={p.busy === true}
      labels={{ region: t(p.dm === true ? 'voice.controls.region' : 'voice.controls.voice'), mute: t('voice.mute'), deafen: t('voice.deafen'),
        camera: t('voice.cameraOn'), share: t('voice.shareOn'), leave: t('voice.leave'),
        ...(p.dm === true ? {} : { bar: { mute: t('callStage.bar.mute'), unmute: t('callStage.bar.unmute'), deafen: t('callStage.bar.deafen'),
          undeafen: t('callStage.bar.undeafen'), camera: t('callStage.bar.camera'), share: t('callStage.bar.share'), stopShare: t('callStage.bar.stopShare'),
          leave: t('callStage.bar.leave') } }) }}
      onMute={noop} onDeafen={noop} onCamera={noop} onShare={noop} onLeave={noop} />
  );
}

function dock(phase: 'connected' | 'paused' | 'reconnecting' | 'connecting', announcement = '') {
  return (
    <div className="dw-voice-dock" data-doc-state="voice-dock">
      <VoiceDock phase={phase} where="harbour" muted={false} deafened={false} announcement={announcement}
        labels={{ region: t('voice.dock.region'), connecting: t('voice.dock.connecting'),
          ping: en['voice.dock.ping'], pingSub: en['voice.dock.pingSub'], pingNone: en['voice.dock.pingNone'], rate: en['voice.dock.rate'], stats: en['voice.dock.stats'],
          quality: { good: en['voice.dock.quality.good'], poor: en['voice.dock.quality.poor'], bad: en['voice.dock.quality.bad'] },
          reconnecting: t('voice.dock.reconnecting'), paused: en['voice.dock.paused'], mute: t('voice.mute'),
          deafen: t('voice.deafen'), leave: t('voice.leave') }}
        onMute={noop} onDeafen={noop} onLeave={noop} />
    </div>
  );
}

const ring = (busy: boolean) => (
  <div className="dw-ring" data-doc-state="call-ring">
    <RingBanner rings={[{ callId: 'r1', busy, name: 'eve', text: busy ? t('call.ring.busy', { name: 'eve', where: 'harbour' }) : t('call.ring.incoming', { name: 'eve' }) }]}
      labels={{ region: t('call.ring.region'), answer: t('call.ring.answer'), decline: t('call.ring.decline') }} onAnswer={noop} onDecline={noop} />
  </div>
);

function Screen(p: { main: ReactNode; header?: ReactNode; joined?: boolean; paused?: boolean; tab?: 'channels' | 'dms'; composer?: ReactNode;
  ring?: ReactNode; dock?: ReactNode; phone?: boolean }) {
  const tab = p.tab ?? 'channels';
  return (
    <div style={{ ['--d-shell-h' as string]: 'calc(100dvh - 32px)' }}>
      <AppShell skipLabel={t('shell.skip')} ring={p.ring} {...(p.phone === true ? { layout: 'phone' as const, phonePane: 'view' as const } : {})}
        rail={<CommunityRail label={t('shell.rail.label')} items={[{ id: 'a', name: 'Midgard' }]} activeId="a" onSelect={noop}
          addLabel={t('layout.rail.add')} onAdd={noop} settingsLabel={t('shell.rail.settings')} onSettings={noop} />}
        sidebar={<div className="dw-sidebar">
          <ChannelList title="Midgard" label={t(tab === 'dms' ? 'shell.dms.label' : 'shell.channels.label')}
            channels={tab === 'dms' ? [{ id: 'bob', name: 'bob', kind: 'dm' as const, readable: false }]
              : [{ id: 'general', name: 'general', kind: 'text' as const, readable: false },
                { id: 'harbour', name: 'harbour', kind: 'voice' as const, readable: false },
                { id: 'build', name: 'build', kind: 'text' as const, readable: false },
                { id: 'workshop', name: 'workshop', kind: 'voice' as const, readable: false }]}
            voice={tab === 'dms' ? undefined : { labels: { ...ROW_LABELS, join: t('voice.join'), joined: t('voice.row.joined') }, onJoin: noop, rooms: {
              harbour: { joined: p.joined === true, participants: [ADA, BO, HELPER] }, workshop: { joined: false, participants: [] } } }}
            activeId={tab === 'dms' ? 'bob' : 'harbour'} onSelect={noop} emptyLabel={t('shell.channels.empty')}
            footer={tab === 'channels' ? undefined : <Button variant="ghost">{t('shell.dms.new')}</Button>} />
          {p.joined === true ? p.dock ?? dock(p.paused === true ? 'paused' : 'connected') : null}
        </div>}
        header={p.header ?? null}
        composer={p.composer ?? null}
        topBar={<TopBar searchLabel={t('topbar.search')} searchShortcut="⌘K" onSearch={noop} voice={p.joined === true ? { room: 'harbour', pingMs: 24, onOpen: noop } : undefined} remindersDue={0} onReminders={noop} onSettings={noop}
        labels={{ reminders: t('reminder.inbox'), settings: t('workspace.open'), voice: t('nav.call.open', { where: 'harbour' }) }} />}>
        {p.main}
      </AppShell>
    </div>
  );
}

const stage = (p: { notice?: CallStageNotice | null; code?: string | null; outgoing?: string; title?: string; joined?: boolean; paused?: boolean; dm?: boolean;
  empty?: boolean; header?: ReactNode; afk?: boolean }) => (
  <div className="dw-voice-stage" data-doc-state={p.notice?.kind === 'blocked' ? 'voice-blocked' : 'voice-stage'}
    data-call-phase={p.joined === false ? 'idle' : undefined}>
    <CallStage title={p.title ?? t('voice.open', { name: 'harbour' })} header={p.header}
      tiles={p.empty === true ? p.joined === false ? [] : [tile(person('you'), true)]
        : p.joined === false ? [tile(ADA), tile(BO), tile(HELPER)] : [tile(person('you'), true), tile(ADA), tile(BO), tile(HELPER)]}
      code={p.code ?? null} notice={p.notice ?? null} outgoing={p.outgoing !== undefined ? { text: p.outgoing } : null}
      // Task 8: a channel not joined is the preview (VoiceView), a joined channel ends in the call bar; the AFK page and a DM keep theirs.
      {...(p.joined === false && p.afk !== true && p.dm !== true ? { preview: { name: 'harbour', join: p.notice?.kind === 'blocked' ? null : { label: t('callStage.preview.join'), onJoin: noop },
        text: p.empty === true ? t('callStage.preview.nobody') : t('callStage.preview.many', { names: ['ada', 'bo', 'helper'].join(t('callStage.preview.and')) }),
        people: p.empty === true ? [] : [ADA, BO, HELPER].map(v => ({ id: v.deviceId, name: v.name, speaking: v.speaking })) } } : {})}
      {...(p.joined !== false && p.dm !== true ? { bar: { name: 'harbour', stats: '24 ms · ↑ 38 ↓ 71 kb/s' } } : {})}
      controls={p.afk === true ? <div className="dw-afk-page"><Button variant="accent" className="dw-voice-join">{t('afk.go')}</Button></div>
        : p.joined === false ? null : controls({ paused: p.paused, dm: p.dm })}
      labels={p.afk === true ? { ...STAGE_LABELS, empty: t('afk.empty') } : STAGE_LABELS} />
  </div>
);

export const Connected: Story = { render: () => <Screen joined main={stage({ code: CODE })} /> };
export const Ringing: Story = { render: () => <Screen joined ring={ring(true)} main={stage({ code: CODE })} /> };
export const Paused: Story = {
  render: () => <Screen joined paused main={stage({ paused: true, notice: { kind: 'paused', text: t('voice.paused', { name: 'bo' }), focusOnce: false,
    action: { label: t('voice.pausedAction'), onAction: noop } } })} />,
};
export const BlockedBeforeJoin: Story = {
  render: () => <Screen main={stage({ joined: false, notice: { kind: 'blocked', text: t('voice.blockedChanged', { name: 'bo' }), focusOnce: false, action: null } })} />,
};
export const LiveNotJoined: Story = { render: () => <Screen main={stage({ joined: false })} /> };

const composer = (
  <Composer label={t('shell.dm.composer', { name: 'bob' })} placeholder={t('shell.dm.composer', { name: 'bob' })} maxLength={4000} value=""
    onChange={noop} onSend={noop} sendLabel={t('shell.composer.send')}
    counterLabel={n => (n < 0 ? t('shell.composer.over', { n: -n }) : t('shell.composer.remaining', { n }))} />
);
const players = (
  <div className="dw-attachments">
    <div data-doc-state="media-player">
      <AttachmentCard kind="audio" name="voice-note.ogg" size="412 KB" thumbUrl={null} w={null} h={null} openLabel="" saveLabel={t('shell.attachment.save', { name: 'voice-note.ogg' })}
        onSave={noop} state="idle" tabbable
        player={{ src: null, state: 'idle', labels: { play: en['media.play'], loading: t('media.loading'), error: t('media.error'), save: t('media.save') }, onPlay: noop, onError: noop }} />
    </div>
    <div data-doc-state="media-player">
      <AttachmentCard kind="video" name="harbour-walk.webm" size="12.4 MB" thumbUrl={null} w={null} h={null} openLabel="" saveLabel={t('shell.attachment.save', { name: 'harbour-walk.webm' })}
        onSave={noop} state="idle" tabbable
        player={{ src: null, state: 'error', labels: { play: en['media.play'], loading: t('media.loading'), error: t('media.error'), save: t('media.save') }, onPlay: noop, onError: noop }} />
    </div>
  </div>
);
const dmLog = (
  <MessageLog label={t('shell.dm.log', { name: 'bob' })} emptyLabel={t('shell.log.empty')}>
    <MessageRow author="bob" time={time} body="here is the walk, and a note" state="ok" attachments={players} />
    <MessageRow author="Ada L" time={time} body="calling you now" own tag="web" state="ok" />
  </MessageLog>
);
export const DmCallRinging: Story = {
  render: () => <Screen tab="dms" joined header={<ChannelHeader kind="dm" name="bob" />} composer={composer}
    main={<><div className="dw-dm-call" data-view="stage">
      <div className="dw-dm-call__bar"><Button pressed={false}>{t('voice.messages')}</Button></div>
      {stage({ title: 'bob', outgoing: t('call.outgoing.ringing', { names: 'bob' }), code: null, dm: true })}
    </div><div className="dw-conversation">{dmLog}</div></>} />,
};
export const DmIncomingRing: Story = {
  render: () => <Screen tab="dms" ring={ring(false)} header={<ChannelHeader kind="dm" name="bob" actions={<IconButton icon="phone" label={t('call.start')} />} />} composer={composer}
    main={dmLog} />,
};

// WB a11y m8: the rest of flow 06's states as screens.
const infoNotice = (key: Parameters<typeof t>[0]): CallStageNotice => ({ kind: 'info', text: t(key), action: null, focusOnce: false });
/** A failed join: the stage shows the notice and the join button again. */
export const JoinFailed: Story = { render: () => <Screen main={stage({ joined: false, notice: infoNotice('voice.failed') })} /> };
/** The connection was lost (WB client I3): the notice where the stage was, the join offered again. */
export const Disconnected: Story = { render: () => <Screen main={stage({ joined: false, notice: infoNotice('voice.disconnected') })} /> };
/** The hold after a refused rekey (WB security I1). */
export const Held: Story = {
  render: () => <Screen joined paused main={stage({ paused: true, notice: { kind: 'paused', text: t('voice.held'), focusOnce: false, action: null } })} />,
};
/** Reconnecting: the dock says so; the tiles keep their last state. */
export const Reconnecting: Story = { render: () => <Screen joined dock={dock('reconnecting')} main={stage({ code: CODE })} /> };
/** Rejoining: the dock stays at connecting and its status says why, once. */
export const Rejoining: Story = {
  render: () => <Screen joined dock={dock('connecting', t('voice.announce.rejoining', { where: 'harbour' }))} main={stage({ code: null })} />,
};
/** The phone DM call with `messages` pressed: the log and composer in the stage's place (flow 06 § 360×740). */
export const DmCallMessages: Story = {
  render: () => <Screen tab="dms" joined header={<ChannelHeader kind="dm" name="bob" />} composer={composer}
    main={<><div className="dw-dm-call" data-view="messages">
      <div className="dw-dm-call__bar"><Button pressed>{t('voice.messages')}</Button></div>
      {stage({ title: 'bob', code: null, dm: true })}
    </div><div className="dw-conversation">{dmLog}</div></>} />,
};
/** An info notice at 360 CSS px. */
export const InfoPhone: Story = {
  render: () => <div style={{ width: 360 }}><Screen main={stage({ joined: false, notice: infoNotice('voice.full') })} /></div>,
};


const nextFrame = () => new Promise<void>(r => { requestAnimationFrame(() => { r(); }); });
const centreOf = (r: DOMRect) => ({ x: r.left + r.width / 2, y: r.top + r.height / 2 });
/**
 * An empty voice channel is one group, the text above the join button, centred (the prototype's layout): the group's centre is
 * the centre of the stage's content box within 2 px — the preview's own content box for a channel not joined (Task 8: the room,
 * "Nobody is here yet", Join voice), the stage's content box under its voice header for the AFK page — the text sits above
 * the button, and the phone's button is the prototype's 40 px.
 */
function contentBox(el: HTMLElement, top?: number): DOMRect {
  const r = el.getBoundingClientRect();
  const cs = getComputedStyle(el);
  const t = Math.max(r.top + parseFloat(cs.paddingTop) + parseFloat(cs.borderTopWidth), top ?? -Infinity);
  const l = r.left + parseFloat(cs.paddingLeft) + parseFloat(cs.borderLeftWidth);
  const b = r.bottom - parseFloat(cs.paddingBottom) - parseFloat(cs.borderBottomWidth);
  const rr = r.right - parseFloat(cs.paddingRight) - parseFloat(cs.borderRightWidth);
  return new DOMRect(l, t, rr - l, b - t);
}
async function expectEmptyCentred(root: HTMLElement, story: string, phone: boolean) {
  await nextFrame(); await nextFrame();
  const stageEl = root.querySelector<HTMLElement>('.d-call-stage');
  const text = root.querySelector<HTMLElement>('.d-call-stage__preview-text, .d-call-stage__empty');
  const join = root.querySelector<HTMLElement>('.d-call-stage__join, .d-call-stage__controls .d-btn');
  if (!stageEl || !text || !join) throw new Error(`${story}: the stage, the empty text or the join button is missing`);
  const preview = text.closest<HTMLElement>('.d-call-stage__preview');
  const under = root.querySelector<HTMLElement>('.d-call-stage__bar')?.getBoundingClientRect().bottom;
  const m = centreOf(preview !== null ? contentBox(preview) : contentBox(stageEl, under));
  const tr = text.getBoundingClientRect(); const jr = join.getBoundingClientRect();
  // An outgoing line, when a stage has one, leads the group, above the message.
  const lead = root.querySelector<HTMLElement>('.d-call-stage__outgoing')?.getBoundingClientRect();
  if (lead && Math.abs(centreOf(lead).x - m.x) > 2) throw new Error(`${story}: the lead line is not centred across (main ${String(m.x)}, line ${String(centreOf(lead).x)})`);
  const top = lead?.top ?? tr.top;
  const group = centreOf(new DOMRect(Math.min(tr.left, jr.left), top, Math.max(tr.right, jr.right) - Math.min(tr.left, jr.left), jr.bottom - top));
  if (Math.abs(centreOf(tr).x - m.x) > 2 || Math.abs(centreOf(jr).x - m.x) > 2) throw new Error(`${story}: not centred across (main ${String(m.x)}, text ${String(centreOf(tr).x)}, button ${String(centreOf(jr).x)})`);
  if (Math.abs(group.y - m.y) > 2) throw new Error(`${story}: not centred down (main ${String(m.y)}, group ${String(group.y)})`);
  if (tr.bottom > jr.top) throw new Error(`${story}: the text is not above the button`);
  const label = document.createRange(); label.selectNodeContents(join);
  if (Math.abs(centreOf(label.getBoundingClientRect()).x - m.x) > 2) throw new Error(`${story}: the join label is not centred (main ${String(m.x)}, label ${String(centreOf(label.getBoundingClientRect()).x)})`);
  // Rule 0 (Jonas 2026-10-27): the phone keeps the prototype's 40px Join voice (no 44px enlargement).
  if (phone && jr.height < 40) throw new Error(`${story}: the join button is ${String(jr.height)} px tall, under 40`);
}
/** Nobody is in the voice channel and this device has not joined: the text and the join button, centred. */
export const EmptyNotJoined: Story = {
  render: () => <Screen header={null} main={stage({ joined: false, empty: true, header: <ChannelHeader kind="voice" name="harbour" /> })} />,
  play: async ({ canvasElement }) => { await expectEmptyCentred(canvasElement, 'EmptyNotJoined', false); },
};
/** The AFK page with nobody away (web3.11: it hosts no call): 'Nobody is away.' and go AFK are one centred group. */
export const EmptyNotJoinedAfk: Story = {
  render: () => <Screen header={null} main={stage({ joined: false, empty: true, afk: true, header: <ChannelHeader kind="voice" name="AFK" /> })} />,
  play: async ({ canvasElement }) => { await expectEmptyCentred(canvasElement, 'EmptyNotJoinedAfk', false); },
};
/** The same on a phone, the pushed view (the test runner visits it at 360 x 740). */
export const EmptyNotJoinedPhone: Story = {
  parameters: { phoneViewport: true },
  render: () => <Screen phone main={stage({ joined: false, empty: true })} />,
  play: async ({ canvasElement }) => { await expectEmptyCentred(canvasElement, 'EmptyNotJoinedPhone', true); },
};
