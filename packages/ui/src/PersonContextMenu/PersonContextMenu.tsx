import { useId } from 'react';
import { ActionMenu, type ActionMenuItem } from '../ActionMenu/ActionMenu.tsx';
import { Avatar } from '../Avatar/Avatar.tsx';
import './PersonContextMenu.css';

export interface PersonContextMenuProps {
  /** Who the menu is about: the name (the own one "{name} (you)"), where they are ("in harbour"), and a bot's square avatar. */
  person: { name: string; where: string; bot?: boolean; /** The avatar's name (initials, hue) when `name` carries a suffix. */ avatarName?: string };
  /** The menu's accessible name, e.g. "actions for Ada". */
  label: string;
  open: boolean;
  /** Where the menu sits: the pointer, or under the element for a key (clamped by ActionMenu). */
  at: { x: number; y: number } | null;
  onOpenChange(open: boolean): void;
  /** Where focus goes back when the menu closes (the row or the tile it was opened on). */
  returnFocus(): HTMLElement | null;
  /** The person's volume (0–200 % in steps of 10): "User volume" and its value over the prototype's range. Absent for yourself. */
  volume?: { label: string; value: number; valueText: string; onChange(value: number): void };
  /** The client-side items: Mute for me (an on/off item that keeps the menu open), Copy username. */
  items: readonly ActionMenuItem[];
  onSelect(id: string): void;
}

/**
 * The person menu (Signal §5.4, the prototype's context menu of a voice occupant and a stage tile), on ActionMenu: 236px on
 * --surface-2 with the --line-strong edge, 9px corners; a header with the 28px avatar, the name (12.5px 700) and where they
 * are (10.5px --fg-3); the "User volume" range; then the items, 32px, a 14px glyph, a check box on an on/off item. Wave C's
 * Move to, Server mute and Disconnect are not drawn.
 */
export function PersonContextMenu({ person, label, open, at, onOpenChange, returnFocus, volume, items, onSelect }: PersonContextMenuProps) {
  const volumeId = useId();
  const head = (
    <>
      <div className="d-person-menu__head">
        <Avatar name={person.avatarName ?? person.name} size="member" shape={person.bot === true ? 'bot' : 'circle'} />
        <span className="d-person-menu__who">
          <span className="d-person-menu__name">{person.name}</span>
          <span className="d-person-menu__where">{person.where}</span>
        </span>
      </div>
      {volume !== undefined ? (
        <div className="d-person-menu__volume">
          <span className="d-person-menu__volume-row" aria-hidden="true"><span>{volume.label}</span><span className="d-person-menu__volume-value">{volume.valueText}</span></span>
          <input id={volumeId} type="range" className="d-person-menu__range" min={0} max={200} step={10} value={volume.value} tabIndex={-1}
            aria-label={volume.label} aria-valuetext={volume.valueText} style={{ '--d-fill': `${String(volume.value / 2)}%` } as React.CSSProperties}
            onChange={e => volume.onChange(Number(e.target.value))} />
        </div>
      ) : null}
    </>
  );
  return (
    <ActionMenu label={label} items={items} open={open} trigger="none" at={at} returnFocus={returnFocus}
      onOpenChange={onOpenChange} onSelect={onSelect} head={head} popupClassName="d-person-menu" />
  );
}
