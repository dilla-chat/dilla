import { useRef, useState, type FocusEvent, type KeyboardEvent } from 'react';
import { ActionMenu } from '../ActionMenu/ActionMenu.tsx';
import { Icon, type IconName } from '../Icon/Icon.tsx';
import { arrowIndex } from '../internal/arrow-select.ts';
import './MessageToolbar.css';

export type MessageToolbarAction = 'react' | 'reply' | 'thread' | 'edit' | 'pin' | 'delete';

export interface MessageToolbarProps {
  label: string;
  tabbable: boolean;
  items: readonly {
    id: MessageToolbarAction; label: string; danger?: boolean;
    /** `anchor` is the button that was pressed, or the toolbar for an item chosen from the "more" menu. */
    onAction(anchor: HTMLElement): void;
  }[];
  /** The "more" button's name (Ruling U): edit, pin and delete sit in its menu. */
  moreLabel: string;
}

/** The prototype's icon buttons (README §5.5): react, reply, thread; the rest go into "more" (⋯). */
const ICON: Partial<Record<MessageToolbarAction, IconName>> = { react: 'smile', reply: 'reply', thread: 'messages-square' };

/** The index of the event's target among the buttons, or -1. */
function indexOfTarget(list: readonly HTMLButtonElement[], target: EventTarget): number {
  return target instanceof HTMLButtonElement ? list.indexOf(target) : -1;
}

/**
 * A message's actions as one tab stop (`role="toolbar"`): icon buttons for react, reply and thread, named by their
 * labels, then "more" (⋯), a menu button with edit, pin and delete. The button that last had focus (initially the first)
 * holds the stop while `tabbable`, and none does otherwise (a row that is not the log's active row). ArrowLeft and
 * ArrowRight move with wrap, Home and End go to the ends; Escape is left to bubble, so the row takes focus.
 */
export function MessageToolbar({ label, tabbable, items, moreLabel }: MessageToolbarProps) {
  const ref = useRef<HTMLDivElement>(null);
  const [stop, setStop] = useState(0);
  const [menuOpen, setMenuOpen] = useState(false);
  const direct = items.filter(i => ICON[i.id] !== undefined);
  const more = items.filter(i => ICON[i.id] === undefined);
  const count = direct.length + (more.length > 0 ? 1 : 0);
  const buttons = (): HTMLButtonElement[] => Array.from(ref.current?.querySelectorAll<HTMLButtonElement>('.d-message-toolbar__btn') ?? []);
  const current = stop < count ? stop : 0;

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const list = buttons();
    const from = indexOfTarget(list, e.target);
    if (from < 0) return;
    const next = arrowIndex(e, from, list.length, 'horizontal');
    if (next === null) return;
    e.preventDefault();
    setStop(next);
    list[next].focus();
  };

  // A click or a programmatic focus moves the stop too.
  const onFocus = (e: FocusEvent<HTMLDivElement>) => {
    const i = indexOfTarget(buttons(), e.target);
    if (i >= 0) setStop(i);
  };

  return (
    <div ref={ref} role="toolbar" aria-label={label} aria-orientation="horizontal" className="d-message-toolbar"
      onKeyDown={onKeyDown} onFocus={onFocus}>
      {direct.map((item, i) => (
        <button key={item.id} type="button" className="d-message-toolbar__btn" aria-label={item.label} title={item.label} data-action={item.id}
          tabIndex={tabbable && i === current ? 0 : -1} onClick={e => item.onAction(e.currentTarget)}>
          <Icon name={ICON[item.id] as IconName} size={14} />
        </button>
      ))}
      {more.length > 0 ? (
        <ActionMenu label={moreLabel} open={menuOpen} onOpenChange={setMenuOpen} triggerClassName="d-message-toolbar__btn"
          triggerContent={<Icon name="ellipsis" size={14} />} triggerTabIndex={tabbable && current === direct.length ? 0 : -1}
          items={more.map(i => ({ id: i.id, label: i.label, danger: i.danger }))}
          onSelect={id => { const item = more.find(i => i.id === id); if (item && ref.current) item.onAction(ref.current); }} />
      ) : null}
    </div>
  );
}
