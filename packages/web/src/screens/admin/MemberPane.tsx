// Flow 08: the member pane. W3 mounts it in the shell's aside; it reads the members, roles and permissions slices.
import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { MemberList, focusReturnTarget, type MemberListGroup } from '@dilla/ui';
import { useSlice } from '../../core/use-slice.ts';
import { t } from '../../strings/index.ts';
import { memberGroups } from './members-model.ts';
import { useCore } from '../../core/context.tsx';
import { useBotRoomLine } from '../voice/music.tsx';
import { MemberCardHost, presenceLabels, type OpenCard, type OpenMenu } from './MemberCardHost.tsx';

export interface MemberPaneProps {
  communityId: string;
  selfId: string;
  /** Opens (or finds) the direct message with `userId` and goes there. */
  onMessage: (userId: string) => void;
  /** Goes to the privacy screen's safety-number comparison for `userId`. */
  onCompare: (userId: string) => void;
  /** The shell's polite announcer. */
  announce: (text: string) => void;
  /** The card open from a mention pill or an author name (the shell holds it). While one is open the pane's own card is closed. */
  messageCard?: OpenCard | null;
  /** The pane opened a card from a row: the shell closes its message card. */
  onCardOpen?: () => void;
  /** Namespaces focus restoration to a settings workspace instead of the covered application shell. */
  focusScope?: string;
  /** The person menu's Mention: puts "@username " in the open channel's composer. Absent where there is no composer (no item). */
  onMention?: (username: string) => void;
  /** The members head's close × (the shell's aside on a desktop; Signal §5.7). Absent: no ×. */
  onClose?: () => void;
}

export function MemberPane(props: MemberPaneProps): React.JSX.Element {
  const { communityId, selfId } = props;
  const members = useSlice(`members:${communityId}`);
  const presence = useSlice(`presence:${communityId}`);
  const roles = useSlice(`roles:${communityId}`);
  const communities = useSlice('communities');
  const channels = useSlice(`channels:${communityId}`);
  const voice = useSlice('voice');
  const call = useSlice('call');
  const client = useCore();
  // The card open from a row of this list. One card at a time across the app: a message card closes it.
  const [card, setCard] = useState<OpenCard | null>(null);
  // The person menu open on a row (task 9): one at a time with the card.
  const [menu, setMenu] = useState<OpenMenu | null>(null);

  // A presence snapshot can briefly replace every row with a loading message. Keep the old row key while focus rests on
  // the pane, then return to its new trigger when the snapshot brings the row back.
  const region = useRef<HTMLDivElement>(null);
  const focusedRow = useRef<HTMLElement | null>(null);
  // A card opened from a pill or an author name has taken focus away from the rows: the pane must not take it back
  // (when that card closes, focus goes to the pill) if a row was re-mounted meanwhile.
  const fromMessage = props.messageCard != null;
  useEffect(() => {
    if (!fromMessage) return;
    focusedRow.current = null;
    setCard(null);
  }, [fromMessage, props.messageCard]);
  useLayoutEffect(() => {
    const old = focusedRow.current;
    if (old === null || old.isConnected) return;
    const active = document.activeElement;
    if (active !== document.body && active !== region.current) return;
    const target = focusReturnTarget(old, undefined, props.focusScope ? region.current : undefined);
    if (target !== null) {
      target.focus();
      focusedRow.current = target;
    } else {
      if (active === document.body) region.current?.focus();
      if (members !== undefined && presence != null && !members.some(m => old.dataset.focusKey === `member:${m.userId}`)) focusedRow.current = null;
    }
  });

  const ownerId = communities?.find(c => c.id === communityId)?.owner ?? '';
  const list = members ?? [];
  const roleList = roles ?? [];

  // The status line (Signal §5.7): a custom status, else where they are in voice in this server ("in harbour" for yourself, "in voice"
  // for others, as the prototype says), else the presence word.
  const words = presenceLabels();
  const statusOf = new Map(list.map(m => [m.userId, m.statusMsg ?? '']));
  const inVoice = new Map<string, string>();
  for (const ch of channels ?? []) {
    for (const p of voice?.[ch.id]?.participants ?? []) inVoice.set(p.userId, ch.name);
  }
  // Task 11 (Q5): the Bot in a voice channel of this server says what its player does there.
  const botIds = new Set(list.filter(m => m.kind === 1).map(m => m.userId));
  let botRoom: { userId: string; channelId: string; name: string } | null = null;
  for (const ch of channels ?? []) {
    const bot = voice?.[ch.id]?.participants.find(p => p.bot || botIds.has(p.userId));
    if (bot !== undefined && botRoom === null) botRoom = { userId: bot.userId, channelId: ch.id, name: ch.name };
  }
  const botLine = useBotRoomLine(client, communityId, botRoom?.channelId ?? null, botRoom?.name ?? '');
  if (call?.phase === 'in' && call.channelId !== null) {
    const ch = (channels ?? []).find(c => c.id === call.channelId);
    if (ch !== undefined) inVoice.set(selfId, ch.name);
  }
  const lineOf = (userId: string): string => {
    // Task 11 (the prototype's member rows): your own row says where your call is, else your presence; your custom status is the
    // user panel's line.
    if (botRoom !== null && botLine !== null && userId === botRoom.userId) return botLine;
    const custom = userId === selfId ? '' : statusOf.get(userId) ?? '';
    if (custom !== '') return custom;
    const room = inVoice.get(userId);
    if (room !== undefined) return userId === selfId ? t('members.inChannel', { channel: room }) : t('members.inVoice');
    return words[presence?.[userId] ?? 'offline'];
  };
  const groups: MemberListGroup[] = memberGroups(list, roleList, ownerId, selfId, presence ?? undefined).map(g => ({
    id: g.id,
    kind: g.role !== null ? 'role' as const : g.id === 'offline' ? 'offline' as const : g.id === 'bots' ? 'bots' as const : 'online' as const,
    title: g.role?.name ?? t(g.id === 'offline' ? 'layout.members.offline' : g.id === 'bots' ? 'layout.members.bots' : 'layout.members.online'),
    color: g.role?.color ?? null,
    members: g.members.map(row => ({ id: row.userId, name: row.name, color: row.color, presence: presence?.[row.userId] ?? 'offline',
      isOwner: row.tags.includes('owner'), isBot: row.tags.includes('bot'), status: lineOf(row.userId) })),
  }));

  // A primary click on the open member's row closes the card, another row switches it (the card itself ignores a pointerdown on its anchor).
  // Enter, a right-click, Shift+F10 and the ContextMenu key only ever open.
  const onOpen = (userId: string, anchor: HTMLElement, via: 'click' | 'menu') => {
    const closes = card !== null && card.id === userId && via === 'click';
    if (!closes) props.onCardOpen?.();
    setMenu(null);
    setCard(closes ? null : { id: userId, source: 'list', anchor });
  };
  const onMenu = (userId: string, at: { x: number; y: number }, row: HTMLElement) => {
    setCard(null);
    setMenu({ id: userId, at, row });
  };

  return (
    <div ref={region} tabIndex={-1} data-member-focus-scope={props.focusScope} onFocusCapture={event => {
      if (event.target instanceof HTMLElement && event.target.dataset.focusKey?.startsWith('member:')) focusedRow.current = event.target;
    }}>
      {/* The shell's aside carries the name "members of {server}"; the list's own 44px head (Signal §5.7) is "Members" and the close ×,
          on the header's hairline, on a phone's full-screen sheet too (Task 11, Jonas Q4). The settings workspace has its section
          heading: no second head there. */}
      <MemberList label={props.focusScope !== undefined ? undefined : t('members.toggle')} closeLabel={t('members.close')} onClose={props.onClose} groups={groups} loading={members === undefined || presence === undefined || presence === null}
        loadingLabel={t('members.loading')} emptyLabel={t('members.empty')} presenceLabels={presenceLabels()}
        ownerLabel={t('layout.profile.owner')} botLabel={t('layout.profile.bot')} openId={card?.id ?? null} onOpen={onOpen} onMenu={onMenu} />
      <MemberCardHost communityId={communityId} selfId={selfId} card={card} onCard={next => { if (next !== null) props.onCardOpen?.(); setCard(next); }}
        menu={menu} onMenu={setMenu} onMention={props.onMention} onMessage={props.onMessage} onCompare={props.onCompare}
        announce={props.announce} fallbackFocus={() => region.current} focusScope={props.focusScope ? region.current ?? undefined : undefined} />
    </div>
  );
}
