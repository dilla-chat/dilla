import { useMemo } from 'react';
import type { ChannelSummary } from '@dilla/client-core';
import { Avatar, EmptyState, Icon, PhoneHeader } from '@dilla/ui';
import { useSlice, useSlices } from '../core/use-slice.ts';
import { t } from '../strings/index.ts';
import { requestThread } from '../thread-request.ts';
import { mergeMembers, nameBook, plainBody, rowBadge } from './shell-model.ts';

const CATEGORY = 2;
const CHANNEL_GLYPH = 'hash';

/** One row of the list: the 32px avatar (or a channel's glyph), "where" over "who text". */
interface Row { key: string; who: string; avatar: { name: string; bot?: boolean } | 'channel'; where: string; text: string; onOpen(el: HTMLElement): void;
  popup?: boolean }

/**
 * The phone's Activity tab (flow 12; Task 11, Jonas Q3: the prototype's one list of avatar rows). Each due reminder comes first
 * ("Reminder · #general", Glenn, its text; a tap opens the reminders sheet, README §6.7), then the mentions and direct messages
 * the worker noticed, newest first ("Valhalla Kitchen · #general", "Liv mentioned you: …"; "Direct message", "Ada …"), then
 * every other channel or DM with something unread ("Midgard Crew · #loot", "#loot 1 new"). A row opens what it names, and a
 * reply that shows in a thread opens that thread too.
 */
export function Activity(props: {
  onOpen(target: { communityId: string | null; channelId: string }): void;
  reminders?: { due: readonly { key: string; text: string; where: string }[]; onOpen(from: HTMLElement): void };
}): React.JSX.Element {
  const notices = useSlice('notices');
  const badges = useSlice('badges');
  const settings = useSlice('settings');
  const dms = useSlice('dms');
  const communities = useSlice('communities');
  const account = useSlice('account');
  const memberLists = useSlices((communities ?? []).map(c => `members:${c.id}` as const));
  const members = useMemo(() => mergeMembers(memberLists), [memberLists]);
  // A mention in a body names its person, as it does in the conversation, never the raw id.
  const book = useMemo(() => nameBook(account?.user ?? null, members), [account, members]);
  const channelLists = useSlices((communities ?? []).map(c => `channels:${c.id}` as const));
  const channels = useMemo(() => channelLists.flatMap(list => (list ?? []) as readonly ChannelSummary[]).filter(c => c.kind !== CATEGORY), [channelLists]);
  const communityName = (id: string | null) => communities?.find(c => c.id === id)?.name ?? null;
  const nameOf = (channelId: string) => channels.find(c => c.id === channelId)?.name ?? dms?.find(d => d.id === channelId)?.name ?? channelId.slice(0, 8);
  const bot = members.find(m => m.kind === 1);

  const listed = [...(notices?.items ?? [])].filter(n => n.kind !== 'message').reverse();
  const named = new Set(listed.map(n => n.channelId));
  const rows: Row[] = [
    ...(props.reminders?.due ?? []).map(r => ({
      key: `reminder:${r.key}`, who: bot?.display || bot?.username || '', avatar: { name: bot?.display || bot?.username || 'Reminder', bot: true },
      where: r.where === '' ? t('reminder.text') : t('activity.reminder', { where: r.where }), text: r.text, popup: true,
      onOpen: (el: HTMLElement) => props.reminders?.onOpen(el),
    })),
    ...listed.map(n => {
      const server = communityName(n.communityId);
      const body = n.body === '' ? '' : plainBody(n.body, book);
      return {
        key: String(n.id), who: n.senderName, avatar: { name: n.senderName },
        where: n.kind === 'mention' && server !== null ? t('activity.where', { server, channel: nameOf(n.channelId) }) : t('activity.dm'),
        text: n.kind === 'mention' ? (body === '' ? t('activity.mentionEmpty') : t('activity.mention', { text: body })) : body,
        onOpen: () => {
          props.onOpen({ communityId: n.communityId, channelId: n.channelId });
          if (n.threadRoot !== null && n.communityId !== null) requestThread(n.channelId, n.threadRoot);
        },
      };
    }),
    // "Unread elsewhere" folds into the list (Q3): a channel by its glyph, a DM by its person.
    ...[
      ...channels.map(c => ({ id: c.id, communityId: c.communityId, name: `#${c.name}`, place: communityName(c.communityId), channel: c.name })),
      ...(dms ?? []).map(d => ({ id: d.id, communityId: null as string | null, name: d.name, place: null as string | null, channel: null as string | null })),
    ].map(row => ({ ...row, badge: rowBadge(badges, settings, row.id, row.communityId ?? undefined) }))
      .filter(row => (row.badge.unread > 0 || row.badge.mentions > 0) && !named.has(row.id))
      .map(row => ({
        key: `unread:${row.id}`, who: row.name, avatar: row.channel === null ? { name: row.name } : 'channel' as const,
        where: row.place !== null && row.channel !== null ? t('activity.where', { server: row.place, channel: row.channel }) : t('activity.dm'),
        text: row.badge.mentions > 0 ? (row.badge.mentions === 1 ? t('activity.mentionOne') : t('activity.mentionMany', { n: row.badge.mentions }))
          : t('activity.newCount', { n: row.badge.unread }),
        onOpen: () => props.onOpen({ communityId: row.communityId, channelId: row.id }),
      })),
  ];

  if (rows.length === 0) {
    return (
      <div className="dw-phone-pane dw-phone-pane--empty dw-activity">
        <PhoneHeader title={t('activity.title')} />
        <div className="dw-phone-pane__frame"><EmptyState title={t('activity.emptyTitle')} body={t('activity.empty')} /></div>
      </div>
    );
  }
  return (
    <div className="dw-phone-pane dw-activity">
      <PhoneHeader title={t('activity.title')} />
      <ul className="dw-activity__list" aria-label={t('activity.title')}>
        {rows.map(r => (
          <li key={r.key}>
            <button type="button" className="dw-activity__row" {...(r.popup === true ? { 'aria-haspopup': 'dialog' as const } : {})}
              onClick={e => r.onOpen(e.currentTarget)}>
              <span className="dw-activity__avatar" aria-hidden="true">
                {r.avatar === 'channel' ? <span className="dw-activity__glyph"><Icon name={CHANNEL_GLYPH} size={14} /></span>
                  : <Avatar name={r.avatar.name} size="row" shape={r.avatar.bot === true ? 'bot' : 'circle'} />}
              </span>
              <span className="dw-activity__text">
                <span className="dw-activity__where">{r.where}</span>
                <span className="dw-activity__line">{r.who === '' ? null : <><strong>{r.who}</strong>{' '}</>}{r.text}</span>
              </span>
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}
