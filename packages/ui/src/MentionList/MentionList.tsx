import type { MouseEvent } from 'react';
import { Avatar } from '../Avatar/Avatar.tsx';
import { Icon } from '../Icon/Icon.tsx';
import './MentionList.css';

export interface MentionListProps {
  id: string;
  label: string;
  /** The popover's visible heading ("Members"); the listbox keeps `label` as its name. */
  title?: string;
  /** `person`: a member, drawn with their avatar; otherwise a role or `@everyone`, drawn with an @ tile. */
  options: readonly { id: string; primary: string; secondary: string; person?: boolean }[];
  activeId: string | null;
  onPick(id: string): void;
}

/**
 * The people (and `@everyone`) to mention, as a listbox the composer's textarea points at with
 * `aria-activedescendant`: no option takes focus. A pointer down on an option picks it and keeps the focus in the
 * textarea (its default is prevented). Drawn as the prototype's suggestion popover (§5.5).
 */
export function MentionList({ id, label, title, options, activeId, onPick }: MentionListProps) {
  return (
    <div className="d-mention-list">
      {title !== undefined ? <div className="d-mention-list__title" aria-hidden="true">{title}</div> : null}
      <ul role="listbox" id={id} aria-label={label} className="d-mention-list__list">
        {options.map(option => (
          <li key={option.id} role="option" id={option.id} aria-selected={option.id === activeId}
            onMouseDown={(e: MouseEvent<HTMLLIElement>) => { e.preventDefault(); onPick(option.id); }}>
            {option.person === true
              ? <span className="d-mention-list__avatar" aria-hidden="true"><Avatar name={option.primary} size="sug" /></span>
              : <span className="d-mention-list__icon" aria-hidden="true"><Icon name="at-sign" size={12} /></span>}
            <span className="d-mention-list__text">
              <span className="d-mention-list__primary">{option.primary}</span>
              <span className="d-mention-list__secondary">{option.secondary}</span>
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}
