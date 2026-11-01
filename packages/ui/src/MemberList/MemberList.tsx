import { useId, useRef, type CSSProperties, type KeyboardEvent, type MouseEvent } from 'react';
import { Avatar } from '../Avatar/Avatar.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { useContextMenuTrigger } from '../internal/contextMenuTrigger.ts';
import { PRESENCE_WORD } from '../internal/presenceWord.ts';
import { roleNameColor, useThemeName } from '../internal/roleColor.ts';
import type { ThemeName } from '@dilla/design-tokens';
import './MemberList.css';

export interface MemberListMember {
  id: string; name: string;
  /** The member owns the server: a crown after the name, and the word `ownerLabel` for screen readers. */
  isOwner?: boolean;
  /** A bot: the rounded-square avatar and the prototype's BOT tag (`botLabel`) after its name. */
  isBot?: boolean;
  /** The line under the name (10.5px): a custom status, where they are in voice, or the presence word. */
  status?: string;
  /** The colour of the member's top coloured role, 0xRRGGBB. Null or 0: none. The name takes it when it keeps 4.5:1 in the current theme, else the text token. */
  color?: number | null;
  presence?: 'online' | 'idle' | 'dnd' | 'offline';
}
export interface MemberListGroup {
  id: string;
  /** `role` is a hoisted role, `online` and `offline` are the presence sections (offline rows are dimmed, R28), `bots` the bots. */
  kind: 'role' | 'online' | 'offline' | 'bots';
  /** The section's raw name; the heading reads `{title} — {n}`, the count is drawn here. */
  title: string;
  color?: number | null;
  members: readonly MemberListMember[];
}
export interface MemberListProps {
  /** The region's name, drawn as the list's 44px head ("Members"). Left out inside a landmark that already names the list (a phone's sheet): a plain container with no head. */
  label?: string;
  /** The head's close × (the prototype's members head); both or neither. */
  closeLabel?: string;
  onClose?(): void;
  groups: readonly MemberListGroup[];
  loading: boolean;
  loadingLabel: string;
  emptyLabel: string;
  /** The word read after an owner's name (the crown itself is decorative). Default `owner`. */
  ownerLabel?: string;
  /** The word of a bot's tag. Default `bot`. */
  botLabel?: string;
  /** The words read for the presence states; each defaults to the English word. */
  presenceLabels?: Partial<Record<'online' | 'idle' | 'dnd' | 'offline', string>>;
  /** The member whose card is open, if any: that row's button reports `aria-expanded` and keeps the hover fill. */
  openId?: string | null;
  /** `via` is `click` for a primary pointer click and `menu` for Enter (and, without `onMenu`, a right-click, Shift+F10 and the ContextMenu key): a caller that toggles closes only on `click`.
   * A click or Enter on a row: open that member's profile card beside `anchor`, the row's button. */
  onOpen?(memberId: string, anchor: HTMLElement, via: 'click' | 'menu'): void;
  /**
   * The member's context menu (the prototype's person menu): a right-click, Shift+F10, the ContextMenu key or a held touch on a row,
   * at `at`; `row` is the row's button, where focus goes back. Without it those open the profile card.
   */
  onMenu?(memberId: string, at: { x: number; y: number }, row: HTMLElement): void;
}

/** The row button's name: the visible name first (WCAG 2.5.3), then owner, bot, the presence word and the status line, as "Astrid, owner, online". */
function rowName(m: MemberListMember, ownerLabel: string, botLabel: string, presenceLabels: MemberListProps['presenceLabels']): string {
  const parts = [m.name];
  if (m.isOwner) parts.push(ownerLabel);
  if (m.isBot) parts.push(botLabel);
  const word = m.presence ? presenceLabels?.[m.presence] ?? PRESENCE_WORD[m.presence] : undefined;
  if (word !== undefined) parts.push(word);
  if (m.status && m.status !== word) parts.push(m.status);
  return parts.join(', ');
}

/** A role's colour as drawn on a name or a heading: the role colour while it keeps 4.5:1 in the theme, else none (the text token). */
function groupColor(color: number | null | undefined, theme: ThemeName, opacity = 1): string | null {
  return roleNameColor(color, theme, opacity);
}

interface RowProps {
  m: MemberListMember; kind: MemberListGroup['kind']; theme: ThemeName; open: boolean; ownerLabel: string; botLabel: string;
  presenceLabels: MemberListProps['presenceLabels']; onOpen: MemberListProps['onOpen']; onMenu: MemberListProps['onMenu'];
}

function MemberRow({ m, kind, theme, open, ownerLabel, botLabel, presenceLabels, onOpen, onMenu }: RowProps) {
  const tint = groupColor(m.color, theme, kind === 'offline' ? 0.8 : 1);
  // With onMenu: the prototype's person menu (right-click, Shift+F10, the ContextMenu key, a held touch). Without: those open the card.
  const menu = useContextMenuTrigger<HTMLLIElement>(onMenu === undefined ? undefined : (li, at) => {
    const button = li.querySelector<HTMLElement>('.d-member-list__open');
    if (button !== null) onMenu(m.id, at, button);
  });
  const keyOpenedAt = useRef(-Infinity);
  const openFrom = (row: HTMLElement) => {
    const button = row.querySelector<HTMLElement>('.d-member-list__open');
    if (button !== null) onOpen?.(m.id, button, 'menu');
  };
  const cardRoutes = onMenu !== undefined || onOpen === undefined ? {} : {
    onContextMenu: (e: MouseEvent<HTMLLIElement>) => {
      e.preventDefault();
      if (e.timeStamp - keyOpenedAt.current < 500) return;
      openFrom(e.currentTarget);
    },
    onKeyDown: (e: KeyboardEvent<HTMLLIElement>) => {
      if (e.key !== 'ContextMenu' && !(e.key === 'F10' && e.shiftKey)) return;
      e.preventDefault();
      keyOpenedAt.current = e.timeStamp;
      openFrom(e.currentTarget);
    },
  };
  const word = m.presence ? presenceLabels?.[m.presence] ?? PRESENCE_WORD[m.presence] : undefined;
  const line = m.status ?? word;
  return (
    <li className="d-member-list__row" data-row="" data-open={open ? 'true' : undefined} {...cardRoutes} {...menu}>
      <button type="button" className="d-member-list__open" data-focus-key={`member:${m.id}`} aria-label={rowName(m, ownerLabel, botLabel, presenceLabels)} aria-haspopup="dialog" aria-expanded={open} onClick={e => onOpen?.(m.id, e.currentTarget, e.detail > 0 ? 'click' : 'menu')}>
        {/* The name is the button's own text, so the avatar is decorative; `aria-label` repeats it with the owner word and the presence word after it. */}
        <span className="d-member-list__avatar" aria-hidden="true"><Avatar name={m.name} size="member" shape={m.isBot ? 'bot' : 'circle'} presence={m.presence} /></span>
        <span className="d-member-list__text">
          <span className="d-member-list__line1">
            <span className="d-member-list__name" style={tint === null ? undefined : { color: tint }}>{m.name}</span>
            {m.isOwner ? <span className="d-member-list__crown"><Icon name="crown" size={11} /></span> : null}
            {m.isBot ? <span className="d-member-list__bot">{botLabel}</span> : null}
          </span>
          {line ? <span className="d-member-list__line2">{line}</span> : null}
        </span>
      </button>
    </li>
  );
}

/**
 * The members of a server (Signal §5.7): a 44px head ("Members" and a close ×), then one section per group: a 10px uppercase
 * heading in the role colour with a 3×10 colour bar and "— n", then 44px rows (a 28px avatar with its solid status dot, the name
 * in the role colour, a crown for the owner, the 10.5px status line). Bots have their own group and the rounded-square avatar.
 * Each row is one button, so Enter and a click open the member's profile card; a right-click, Shift+F10, the ContextMenu key and
 * a held touch open the person menu (`onMenu`).
 */
export function MemberList({ label, closeLabel, onClose, groups, loading, loadingLabel, emptyLabel, ownerLabel = 'owner', botLabel = 'bot', presenceLabels, openId = null, onOpen, onMenu }: MemberListProps) {
  const shown = groups.filter(g => g.members.length > 0);
  const theme = useThemeName();
  const Root = label === undefined ? 'div' : 'section';
  const barId = useId();
  return (
    <Root className="d-member-list" aria-labelledby={label === undefined ? undefined : barId}>
      {label !== undefined ? (
        <div className="d-member-list__bar">
          <h2 id={barId} className="d-member-list__label">{label}</h2>
          {onClose !== undefined && closeLabel !== undefined ? <button type="button" className="d-member-list__close" aria-label={closeLabel} onClick={onClose}><Icon name="x" size={15} /></button> : null}
        </div>
      ) : null}
      <div className="d-member-list__body">
        {loading ? (
          <p className="d-member-list__note" role="status">{loadingLabel}</p>
        ) : shown.length === 0 ? (
          <p className="d-member-list__note">{emptyLabel}</p>
        ) : shown.map(group => {
          const color = group.kind === 'role' ? groupColor(group.color, theme) : null;
          return (
            <div key={group.id} className="d-member-list__group" data-kind={group.kind}>
              <h3 className="d-member-list__title" style={color === null ? undefined : { '--d-group-color': color } as CSSProperties}>
                {color === null ? null : <span className="d-member-list__title-bar" aria-hidden="true" />}{`${group.title} — ${String(group.members.length)}`}
              </h3>
              <ul className="d-member-list__rows" role="list">
                {group.members.map(m => <MemberRow key={m.id} m={m} kind={group.kind} theme={theme} open={openId === m.id} ownerLabel={ownerLabel} botLabel={botLabel}
                  presenceLabels={presenceLabels} onOpen={onOpen} onMenu={onMenu} />)}
              </ul>
            </div>
          );
        })}
      </div>
    </Root>
  );
}
