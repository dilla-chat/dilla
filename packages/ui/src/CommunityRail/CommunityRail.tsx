import { Icon } from '../Icon/Icon.tsx';
import { Pill } from '../Pill/Pill.tsx';
import { useRovingFocus } from '../internal/roving.ts';
import { usePhoneLayout } from '../internal/phoneLayout.ts';
import './CommunityRail.css';

export interface CommunityRailProps {
  label: string;
  items: readonly { id: string; name: string; unread?: number; mentions?: number }[];
  activeId: string | null;
  onSelect(id: string): void;
  /** The one dashed "+" tile (`layout.rail.add`): it calls `onAdd`, and the caller opens an AddServerDialog whose two choices are the join and create flows. */
  addLabel: string;
  onAdd(): void;
  /**
   * The `@` tile (direct messages and Home), first in the rail and followed by a hairline: `label` is `home.tile`'s name, `itemLabel`
   * the filled `home.tileLabel` it takes while it has unread or mentions.
   */
  home?: { label: string; selected: boolean; unread: number; mentions: number; itemLabel: string; onSelect(): void };
  settingsLabel?: string;
  onSettings?(): void;
  itemLabel?(i: { name: string; unread: number; mentions: number }): string;
}

/** The tile's initials, by the rule Avatar uses: two words → their first letters, else the first two letters. */
function initials(name: string): string {
  const trimmed = name.trim();
  const parts = trimmed.split(/\s+/);
  const raw = parts.length >= 2 ? `${parts[0].charAt(0)}${parts[1].charAt(0)}` : trimmed.slice(0, 2);
  return raw.toUpperCase();
}

/**
 * The server rail (Signal, README §5.3), --rail-w wide: the `@` tile first when given, a 20px hairline, one round tile per
 * server (named by the full name, the initials are decoration) that morphs to a 10px square when selected or hovered, one
 * dashed "+" circle that asks to add a server and, when given, the settings button last. A tile with mentions carries a
 * count at its bottom right; unread alone is read by a screen reader only (the prototype draws no unread mark on the rail).
 * A tile with unread messages or mentions is named by `itemLabel` when one is given. One tab stop with roving focus over every tile, the settings
 * button included (Global Constraints keyboard map).
 */
export function CommunityRail({
  label, items, activeId, onSelect, addLabel, onAdd, home, settingsLabel, onSettings, itemLabel,
}: CommunityRailProps) {
  // The roving stops are the buttons in order: the `@` tile (when given), then the servers (the hairline has none).
  const itemIndex = items.findIndex(i => i.id === activeId);
  const activeIndex = home?.selected === true ? 0 : itemIndex >= 0 ? itemIndex + (home !== undefined ? 1 : 0) : -1;
  const roving = useRovingFocus(activeIndex);
  // On a phone the You tab is the way to settings: the rail has no settings tile there.
  const phone = usePhoneLayout();
  // Unread is read by a screen reader only; a mention is a count at the tile's bottom right.
  const badge = (unread: number, mentions: number) => mentions > 0
    ? <span className="d-community-rail__badge"><Pill kind="mention" count={mentions} /></span>
    : unread > 0 ? <span className="d-sr-only"><Pill kind="unread" count={unread} /></span> : null;
  return (
    <nav className="d-community-rail" aria-label={label}>
      <ul role="list" className="d-community-rail__list" ref={roving.ref} onKeyDown={roving.onKeyDown} onFocus={roving.onFocus}>
        {home !== undefined ? (
          <>
            <li>
              <button type="button" className="d-community-rail__item d-community-rail__home" aria-current={home.selected ? 'page' : undefined}
                data-unread={String(home.unread)} data-mentions={String(home.mentions)}
                aria-label={home.unread > 0 || home.mentions > 0 ? home.itemLabel : home.label} onClick={home.onSelect}>
                <Icon name="at-sign" size={17} />
                {badge(home.unread, home.mentions)}
              </button>
            </li>
            <li aria-hidden="true" className="d-community-rail__rule" />
          </>
        ) : null}
        {items.map(item => {
          const unread = item.unread ?? 0;
          const mentions = item.mentions ?? 0;
          const selected = item.id === activeId;
          const ariaLabel = itemLabel && (unread > 0 || mentions > 0) ? itemLabel({ name: item.name, unread, mentions }) : undefined;
          return (
            <li key={item.id}>
              <button type="button" className="d-community-rail__item" aria-current={selected ? 'page' : undefined}
                data-unread={String(unread)} data-mentions={String(mentions)} aria-label={ariaLabel} onClick={() => onSelect(item.id)}>
                <span className="d-community-rail__initials" aria-hidden="true">{initials(item.name)}</span>
                <span className="d-sr-only">{item.name}</span>
                {badge(unread, mentions)}
              </button>
            </li>
          );
        })}
        <li>
          <button type="button" className="d-community-rail__join" aria-label={addLabel} aria-haspopup="dialog" onClick={onAdd}>
            <span className="d-community-rail__glyph" aria-hidden="true"><Icon name="plus" size={15} /></span>
          </button>
        </li>
        {!phone && settingsLabel !== undefined && onSettings !== undefined ? (
          <li className="d-community-rail__settings-item">
            <button type="button" className="d-community-rail__settings" aria-label={settingsLabel} onClick={onSettings}>
              <svg className="d-community-rail__cog" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true" focusable="false">
                <path fillRule="evenodd" clipRule="evenodd" d="M9.4 1.4l.25 1.5c.5.15.95.35 1.4.6l1.25-.85 1.55 1.55-.85 1.25c.25.45.45.9.6 1.4l1.5.25v2.2l-1.5.25c-.15.5-.35.95-.6 1.4l.85 1.25-1.55 1.55-1.25-.85c-.45.25-.9.45-1.4.6l-.25 1.5H6.6l-.25-1.5c-.5-.15-.95-.35-1.4-.6l-1.25.85-1.55-1.55.85-1.25c-.25-.45-.45-.9-.6-1.4l-1.5-.25v-2.2l1.5-.25c.15-.5.35-.95.6-1.4L2.15 4.45 3.7 2.9l1.25.85c.45-.25.9-.45 1.4-.6l.25-1.5h2.8zM8 5.5a2.5 2.5 0 100 5 2.5 2.5 0 000-5z" />
              </svg>
            </button>
          </li>
        ) : null}
      </ul>
    </nav>
  );
}
