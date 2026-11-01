// Flow 08/14: the one open profile card of the app, its actions and the dialogs they open. The member pane (a row) and the
// conversation (a mention pill, an author name or avatar) both open it; the shell holds the one open state.
import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { MemberSummary, PresenceStatus } from '@dilla/client-core';
import { PersonContextMenu, ProfileCard, focusReturnTarget, type ActionMenuItem, type IconName } from '@dilla/ui';
import { useSlice, useSlices } from '../../core/use-slice.ts';
import { usePhoneLayout } from '../../layout.ts';
import { t } from '../../strings/index.ts';
import { memberName, type MenuAction } from './members-model.ts';
import { dmCardData, memberCardData } from './member-card.ts';
import { BanFlow, KickFlow, TimeoutFlow, WarnFlow, type ModTarget } from './Moderation.tsx';
import { MemberRolesFlow } from './MemberRolesFlow.tsx';

/**
 * The open card: whose it is, and what it is anchored to. A `list` card follows its member's row (found again by id on
 * every render, so a re-mounted row neither closes it nor strands it). A `mention` or `author` card is anchored to that
 * element and closes when the element is gone. `name` is the label to show when the person is no longer a member.
 */
export interface OpenCard { id: string; source: 'list' | 'mention' | 'author'; anchor: HTMLElement; name?: string; /** The message row it was opened from (its msg id), where focus goes if the anchor is gone. */ rowId?: string; /** The container of that row (the log or the thread pane), so the fallback picks the row in the same place. */ scope?: HTMLElement }

/** Where focus goes when a message card's anchor is gone: its message row if that still exists, else the log. */
export const messageFallback = (card: OpenCard) => (): HTMLElement | null => {
  const find = (root: ParentNode) => (card.rowId === undefined ? null : root.querySelector<HTMLElement>(`.d-message-row[data-msg-id="${card.rowId}"]`));
  return (card.scope?.isConnected === true ? find(card.scope) : null) ?? find(document) ?? document.querySelector<HTMLElement>('[role="log"]');
};

export const presenceLabels = (): Record<'online' | 'idle' | 'dnd' | 'offline', string> => ({
  online: t('presence.online'), idle: t('presence.idle'), dnd: t('presence.dnd'), offline: t('presence.offline'),
});

/** The person menu open on a member row (task 9, the prototype's context menu): whose, where, and the row to give focus back to. */
export interface OpenMenu { id: string; at: { x: number; y: number }; row: HTMLElement }

export interface MemberCardHostProps {
  /** The server the card belongs to; null in a DM, where the card has no roles or moderation (`dm` then names the people). */
  communityId: string | null;
  /** A DM's context: everyone the shell knows by id, and the name to show for a person none of them lists. */
  dm?: { members: readonly MemberSummary[]; /** The other person of a 1:1 DM (null in a group); only they are named by `peerName` when no server lists them. */ peerId: string | null; peerName: string };
  selfId: string;
  card: OpenCard | null;
  onCard: (next: OpenCard | null) => void;
  onMessage: (userId: string) => void;
  onCompare: (userId: string) => void;
  announce: (text: string) => void;
  /** Where focus goes when a dialog closes and its trigger is gone. */
  fallbackFocus?: () => HTMLElement | null;
  /** Restricts list-row re-anchoring and card focus return to the active settings pane. */
  focusScope?: ParentNode;
  /** The person menu open on a member row, and how to close it (the member pane's right-click, Shift+F10, held touch). */
  menu?: OpenMenu | null;
  onMenu?: (next: OpenMenu | null) => void;
  /** The person menu's Mention (fix round 1): "@username " into the open channel's composer; absent, no Mention item. */
  onMention?: (username: string) => void;
}

export function MemberCardHost(props: MemberCardHostProps): React.JSX.Element {
  const { communityId, selfId, card, onCard } = props;
  const phone = usePhoneLayout();
  const communities = useSlice('communities');
  const inDm = communityId === null;
  const cid = communityId ?? '';
  const communitySlice = useSlice(`members:${cid}`);
  const members = inDm ? props.dm?.members : communitySlice;
  // In a DM there is no server: a peer's presence is the best any shared server reports.
  const presences = useSlices((inDm ? (communities ?? []).map(c => c.id) : [cid]).map(id => `presence:${id}` as const));
  const presence = bestPresence(presences);
  const roles = useSlice(`roles:${cid}`);
  const access = useSlice(`permissions:${cid}`);
  const [kick, setKick] = useState<ModTarget | null>(null);
  const [ban, setBan] = useState<ModTarget | null>(null);
  const [timeout, setTimeoutTarget] = useState<ModTarget | null>(null);
  const [warn, setWarnTarget] = useState<ModTarget | null>(null);
  const [kickOpen, setKickOpen] = useState(false);
  const [banOpen, setBanOpen] = useState(false);
  const [timeoutOpen, setTimeoutOpen] = useState(false);
  const [warnOpen, setWarnOpen] = useState(false);
  const [rolesFor, setRolesFor] = useState<string | null>(null);

  // Where focus goes when a dialog closes: the trigger that opened it (the card's anchor), else the caller's fallback.
  const opener = useRef<HTMLElement | null>(null);
  const restore = useRef(false);
  const anyOpen = kickOpen || banOpen || timeoutOpen || warnOpen || rolesFor !== null;
  useEffect(() => {
    if (anyOpen || !restore.current) return;
    restore.current = false;
    focusReturnTarget(opener.current, () => props.fallbackFocus?.() ?? null)?.focus();
  }, [anyOpen]); // eslint-disable-line react-hooks/exhaustive-deps

  const community = communities?.find(c => c.id === communityId);
  const ownerId = community?.owner ?? '';
  const byId = new Map((members ?? []).map(m => [m.userId, m]));

  // A list card follows its member's row: look the row up by id after every render and re-anchor when it is a new element.
  // A message card closes when its element is gone.
  useLayoutEffect(() => {
    if (card === null) return;
    if (card.source !== 'list') {
      if (!card.anchor.isConnected) onCard(null);
      return;
    }
    const row = (props.focusScope ?? document).querySelector<HTMLElement>(`[data-focus-key="member:${card.id}"]`);
    if (row !== null && row !== card.anchor) onCard({ ...card, anchor: row });
  });
  // A member who leaves or is kicked closes their card; a rejoin never brings it back.
  const seen = useRef<string | null>(null);
  useLayoutEffect(() => {
    if (card === null) { seen.current = null; return; }
    if (members === undefined) return;
    if (byId.has(card.id)) seen.current = card.id;
    else if (seen.current === card.id || card.source === 'list') { seen.current = null; onCard(null); }
  });

  const member = card === null ? undefined : byId.get(card.id);
  const dmName = card === null ? '' : card.id === props.dm?.peerId ? props.dm.peerName : card.name !== undefined && card.name !== '' ? card.name : t('layout.profile.formerMember');
  const data = inDm ? (card === null ? null : dmCardData(card.id, member, { selfId, fallbackName: dmName }))
    : member === undefined ? null : memberCardData(member, { selfId, ownerId, roles: roles ?? [], access });

  const labelOf: Record<MenuAction, string> = {
    message: t('members.menu.message'), compare: t('members.menu.compare'), roles: t('layout.profile.manageRoles'), warn: t('mod.warn.menu'), timeout: t('mod.timeout.menu'), kick: t('members.menu.kick'), ban: t('members.menu.ban'),
  };
  const onAction = (userId: string, action: string, from?: HTMLElement) => {
    const row = byId.get(userId);
    if (row === undefined && !(inDm && (action === 'message' || action === 'compare'))) return;
    const target = { userId, name: row === undefined ? '' : memberName(row) };
    opener.current = from ?? card?.anchor ?? (document.activeElement instanceof HTMLElement ? document.activeElement : null);
    restore.current = action === 'kick' || action === 'ban' || action === 'timeout' || action === 'warn' || action === 'roles';
    if (action === 'message') props.onMessage(userId);
    else if (action === 'compare') props.onCompare(userId);
    else if (action === 'roles') setRolesFor(userId);
    else if (action === 'warn') { setWarnTarget(target); setWarnOpen(true); }
    else if (action === 'kick') { setKick(target); setKickOpen(true); }
    else if (action === 'ban') { setBan(target); setBanOpen(true); }
    else if (action === 'timeout') { setTimeoutTarget(target); setTimeoutOpen(true); }
    onCard(null);
  };

  const labels = {
    bioLabel: t('profile.card.about'), ownerLabel: t('layout.profile.owner'), youLabel: t('layout.profile.you'), rolesLabel: t('layout.profile.roles'),
    closeLabel: t('layout.profile.close'), presenceLabels: presenceLabels(),
  };
  const layout = phone ? 'sheet' : 'popover';
  // Task 9: the card draws the prototype's row (Message, the safety-number shield, Edit roles); moderation is in the row's person menu.
  const CARD: Partial<Record<MenuAction, IconName>> = { message: 'message-square', compare: 'shield', roles: 'square-pen' };
  const cardActions = (ids: readonly MenuAction[]) => ids.filter(id => CARD[id] !== undefined)
    .map(id => ({ id, label: labelOf[id], icon: CARD[id], primary: id === 'message' }));

  // The person menu: Profile, Message, Copy username, Edit roles, then the moderation the viewer may use after a rule.
  const menu = props.menu ?? null;
  const menuMember = menu === null ? undefined : byId.get(menu.id);
  const menuData = menuMember === undefined ? null : memberCardData(menuMember, { selfId, ownerId, roles: roles ?? [], access });
  const MOD: Partial<Record<MenuAction, IconName>> = { warn: 'triangle-alert', timeout: 'alarm-clock', kick: 'log-out', ban: 'lock' };
  const menuItems: ActionMenuItem[] = menuData === null ? [] : [
    { id: 'profile', label: t('members.menu.profile'), icon: 'user' },
    ...(menuData.actions.includes('message') ? [{ id: 'message', label: t('members.menu.message'), icon: 'message-square' as const }] : []),
    ...(props.onMention !== undefined && !menuData.isYou ? [{ id: 'mention', label: t('members.menu.mention'), icon: 'at-sign' as const }] : []),
    { id: 'copy', label: t('members.menu.copy'), icon: 'copy' },
    ...(menuData.actions.includes('roles') ? [{ id: 'roles', label: t('members.menu.roles'), icon: 'shield' as const }] : []),
    ...menuData.actions.filter(id => MOD[id] !== undefined).map((id, i) => ({
      id, icon: MOD[id], danger: true, sep: i === 0,
      label: id === 'kick' ? t('members.menu.kickName', { name: menuData.name }) : id === 'ban' ? t('members.menu.banName', { name: menuData.name }) : labelOf[id],
    })),
  ];
  const onMenuSelect = (id: string) => {
    if (menu === null || menuData === null) return;
    props.onMenu?.(null);
    if (id === 'profile') { onCard({ id: menu.id, source: 'list', anchor: menu.row }); return; }
    if (id === 'mention') { props.onMention?.(menuData.username); return; }
    if (id === 'copy') {
      const text = `@${menuData.username}`;
      void navigator.clipboard?.writeText(text).then(() => props.announce(t('members.menu.copied', { name: menuData.username })), () => undefined);
      return;
    }
    onAction(menu.id, id, menu.row);
  };
  return (
    <>
      {card !== null && data !== null ? (
        <ProfileCard name={data.name} username={data.username} presence={presence?.[data.userId] ?? 'offline'} status={data.status} bio={data.bio} isOwner={data.isOwner} isYou={data.isYou}
          isBot={member?.kind === 1} client={member?.kind === 1 ? undefined : t('layout.profile.web')} roles={data.roles} actions={cardActions(data.actions)}
          onAction={id => onAction(data.userId, id)} onClose={() => onCard(null)} anchor={card.anchor} layout={layout} placement={card.source === 'list' ? 'side' : 'below'} fallbackFocus={card.source === 'list' ? undefined : messageFallback(card)} focusScope={props.focusScope} {...labels} />
      ) : card !== null && card.source !== 'list' && card.name !== undefined && members !== undefined ? (
        <ProfileCard name={card.name === '' ? t('layout.profile.formerMember') : card.name} username="" roles={[]} actions={[]} onAction={() => {}} onClose={() => onCard(null)} anchor={card.anchor} layout={layout} placement="below" fallbackFocus={messageFallback(card)} focusScope={props.focusScope} {...labels} />
      ) : null}
      {menu !== null && menuData !== null ? (
        <PersonContextMenu person={{ name: menuData.name, where: menuData.username === '' ? '' : `@${menuData.username}`, bot: menuMember?.kind === 1 }}
          label={t('members.menu.label', { name: menuData.name })} open at={menu.at} items={menuItems} onSelect={onMenuSelect}
          onOpenChange={next => { if (!next) props.onMenu?.(null); }} returnFocus={() => (menu.row.isConnected ? menu.row : null)} />
      ) : null}
      <KickFlow open={kickOpen} communityId={cid} serverName={community?.name ?? ''} target={kick} announce={text => props.announce(text)}
        onClose={() => setKickOpen(false)} onDone={() => setKickOpen(false)} />
      <BanFlow open={banOpen} communityId={cid} serverName={community?.name ?? ''} target={ban} announce={text => props.announce(text)}
        onClose={() => setBanOpen(false)} onDone={() => setBanOpen(false)} />
      <TimeoutFlow open={timeoutOpen} communityId={cid} serverName={community?.name ?? ''} target={timeout} announce={text => props.announce(text)}
        onClose={() => setTimeoutOpen(false)} onDone={() => setTimeoutOpen(false)} />
      <WarnFlow open={warnOpen} communityId={cid} serverName={community?.name ?? ''} target={warn} announce={text => props.announce(text)}
        onClose={() => setWarnOpen(false)} onDone={() => setWarnOpen(false)} />
      {rolesFor !== null
        ? <MemberRolesFlow communityId={cid} userId={rolesFor} announce={text => props.announce(text)} onClose={() => setRolesFor(null)} />
        : null}
    </>
  );
}

const RANK: Record<PresenceStatus, number> = { offline: 0, idle: 1, dnd: 2, online: 3 };
/** The most present status any of the snapshots gives each person. */
function bestPresence(all: readonly (Record<string, PresenceStatus> | null | undefined)[]): Record<string, PresenceStatus> {
  const out: Record<string, PresenceStatus> = {};
  for (const p of all) for (const [id, st] of Object.entries(p ?? {})) { const cur = out[id]; if (cur === undefined || RANK[st] > RANK[cur]) out[id] = st; }
  return out;
}
