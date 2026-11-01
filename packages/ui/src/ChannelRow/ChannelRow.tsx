import { Icon } from '../Icon/Icon.tsx';
import { Avatar, type Presence } from '../Avatar/Avatar.tsx';
import { Pill } from '../Pill/Pill.tsx';
import { Tag } from '../Tag/Tag.tsx';
import { TrailingSlot } from '../TrailingSlot/TrailingSlot.tsx';
import './ChannelRow.css';

export type ChannelRowProps = {
  name: string; kind: 'text' | 'voice' | 'dm' | 'announcement' | 'afk'; active?: boolean; unread?: number; mentions?: number;
  muted?: boolean; readable?: boolean; private?: boolean; onSelect: () => void;
  ariaLabel?: string;
  /** A short note after the name ("slow mode"). It is part of the row's accessible name unless `ariaLabel` replaces it. */
  badge?: string;
  /**
   * The unread tag's visible text for a count (`layout.row.new`, '{n} new'; README §5.4). Without it the tag shows the bare
   * count. The tag's hidden "{n} unread" names it either way.
   */
  newLabel?(count: string): string;
  /**
   * Task 11 (Jonas, Q2; the prototype's DM row): a DM drawn as a person, 42px (52 on a phone): the 28px avatar with an online
   * dot, the name over `sub` (their status, or a group's members), and a 7px unread dot instead of the "n new" tag. `group`
   * draws the neutral avatar of a group DM.
   */
  person?: { sub?: string; presence?: Presence; group?: boolean; initials?: string };
};

const ICON = { text: 'hash', voice: 'volume-2', announcement: 'megaphone', afk: 'moon' } as const;

/** The glyph column: a 14px icon (hash, speaker, megaphone, moon), a dot for a person. */
function Glyph({ kind }: { kind: ChannelRowProps['kind'] }) {
  return (
    <span className="d-chrow__glyph" aria-hidden="true">
      {kind === 'dm' ? <span className="d-chrow__dot" /> : <Icon name={ICON[kind]} size={14} />}
    </span>
  );
}

const cap = (n: number) => (n > 99 ? '99+' : String(n));

/**
 * One channel row (Signal, README §5.4): the 14px glyph, the name, the lock after it, then one trailing slot holding the
 * "@2" mention tag, else the "1 new" unread tag, else nothing. Active is the selected fill, a 600 name, the accent glyph and
 * a 2px accent bar at the left edge that grows with scaleY; unread or a mention is a bright 600 name.
 */
export function ChannelRow({ name, kind, active, unread = 0, mentions = 0, muted, readable, private: isPrivate, onSelect, ariaLabel, badge, newLabel, person }: ChannelRowProps) {
  // A muted channel hides its unread count; its mention count still shows (ruling 15).
  const shown = muted ? 0 : unread;
  if (person !== undefined) {
    return (
      <button type="button" className="d-chrow" data-row data-kind={kind} data-person="true" data-group={person.group ? 'true' : undefined}
        data-unread={String(shown)} data-mentions={String(mentions)} data-muted={muted ? 'true' : undefined}
        aria-current={active ? 'page' : undefined} aria-label={ariaLabel} aria-description={person.sub !== undefined && person.sub !== '' ? person.sub : undefined} onClick={onSelect}>
        <span className="d-chrow__bar" aria-hidden="true" />
        <span className="d-chrow__avatar" aria-hidden="true">
          <Avatar name={name} size="member" {...(person.initials !== undefined ? { initials: person.initials } : {})}
            {...(person.presence === 'online' && person.group !== true ? { presence: 'online' as const } : {})} />
        </span>
        <span className="d-chrow__who">
          <span className="d-chrow__name">{name}</span>
          {/* The sub describes the row; its name stays the DM's (the rows are found and announced by name). */}
          {person.sub !== undefined && person.sub !== '' ? <span className="d-chrow__sub" aria-hidden="true">{person.sub}</span> : null}
        </span>
        {mentions > 0 ? <Pill kind="mention" count={mentions} text={`@${cap(mentions)}`} />
          : shown > 0 ? <span className="d-chrow__unread"><span className="d-chrow__unread-dot" aria-hidden="true" /><span className="d-sr-only">{`${String(shown)} unread`}</span></span> : null}
      </button>
    );
  }
  return (
    <button type="button" className="d-chrow" data-row data-kind={kind} data-unread={String(shown)} data-mentions={String(mentions)}
      data-muted={muted ? 'true' : undefined} aria-current={active ? 'page' : undefined} aria-label={ariaLabel} onClick={onSelect}>
      <span className="d-chrow__bar" aria-hidden="true" />
      <Glyph kind={kind} />
      <span className="d-chrow__name">{name}</span>
      {badge !== undefined ? <span className="d-chrow__badge">{badge}</span> : null}
      {readable ? <Tag kind="readable" /> : null}
      {isPrivate ? <span className="d-chrow__lock" role="img" aria-label="Private"><Icon name="lock" size={12} /></span> : null}
      <span className="d-chrow__spacer" />
      <TrailingSlot>
        {mentions > 0 ? <Pill kind="mention" count={mentions} text={`@${cap(mentions)}`} />
          : shown > 0 ? <Pill kind="unread" count={shown} text={newLabel?.(cap(shown)) ?? cap(shown)} /> : null}
      </TrailingSlot>
    </button>
  );
}
