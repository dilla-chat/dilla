import { useEffect, useId, useRef, type KeyboardEvent } from 'react';
import { Icon } from '../Icon/Icon.tsx';
import './RemindersPopover.css';

export interface ReminderItem {
  id: string;
  text: string;
  /** When it is (was) due, as the prototype writes it: "Today, 20:00", "Fri 30 Oct, 20:00". */
  when: string;
  /** Where it was set: "#general" (empty when the place is unknown). */
  where: string;
  due: boolean;
  /** A change to it is in flight: its action is disabled. */
  busy?: boolean;
  /** With `onOpen`: its text is a button that opens the message it was set from (no visual change). */
  canOpen?: boolean;
}

export interface RemindersPopoverProps {
  items: readonly ReminderItem[];
  tab: 'due' | 'upcoming';
  onTab(tab: 'due' | 'upcoming'): void;
  /** The row's one action: Dismiss a due reminder, Cancel an upcoming one. */
  onAction(id: string, action: 'dismiss' | 'cancel'): void;
  /** Opens the message a reminder was set from (an item with `canOpen`). */
  onOpen?(id: string): void;
  /** Escape, or a press outside the popover (and outside `anchor`). */
  onClose(): void;
  /** Where Escape hands focus back (the bell, the Activity row). */
  returnFocus?(): HTMLElement | null;
  /** The control that opens it: a press on it is not "outside" (it toggles the popover itself). */
  anchor?(): HTMLElement | null;
  /** 'popover' hangs under the top bar's bell (desktop); 'sheet' rises from the bottom (phone). */
  layout?: 'popover' | 'sheet';
  /** The bot's initials for the header tile (the prototype's "GL"); without it no tile. */
  botInitials?: string;
  /** A loading or error line, spoken politely. */
  status?: string;
  labels: {
    title: string; tabs: string; due: string; upcoming: string; dismiss: string; cancel: string;
    emptyDue: string; emptyUpcoming: string; footer: string;
  };
}

/**
 * The reminders popover (Signal README §6.7, the prototype's bell popover): a 340px non-modal panel under the bell. Its head is
 * the bot tile, "Reminders" and the Due / Upcoming tabs with their counts; then the rows of the picked tab (a 26px alarm-clock
 * tile, --warn on a due one, the text, "when · where", and Dismiss or Cancel), or the tab's empty line; then the footer note.
 * It is a dialog that does not trap focus: focus goes to the picked tab when it opens, Escape closes it and gives focus back,
 * a press outside closes it. On a phone the same panel is a bottom sheet.
 */
export function RemindersPopover({ items, tab, onTab, onAction, onOpen, onClose, returnFocus, anchor, layout = 'popover', botInitials, status, labels }: RemindersPopoverProps) {
  const id = useId();
  const root = useRef<HTMLDivElement>(null);
  const due = items.filter(i => i.due);
  const upcoming = items.filter(i => !i.due);
  const shown = tab === 'due' ? due : upcoming;

  useEffect(() => {
    root.current?.querySelector<HTMLElement>('[role="tab"][aria-selected="true"]')?.focus();
  }, []);
  useEffect(() => {
    const down = (e: PointerEvent) => {
      if (!(e.target instanceof Node)) return;
      if (root.current?.contains(e.target) || anchor?.()?.contains(e.target)) return;
      onClose();
    };
    document.addEventListener('pointerdown', down, true);
    return () => document.removeEventListener('pointerdown', down, true);
  }, [onClose, anchor]);

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'Escape') return;
    e.preventDefault();
    e.stopPropagation();
    onClose();
    returnFocus?.()?.focus();
  };
  const onTabKey = (e: KeyboardEvent<HTMLButtonElement>) => {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight' && e.key !== 'Home' && e.key !== 'End') return;
    e.preventDefault();
    const next = e.key === 'Home' ? 'due' : e.key === 'End' ? 'upcoming' : tab === 'due' ? 'upcoming' : 'due';
    onTab(next);
    root.current?.querySelector<HTMLElement>(`[data-tab="${next}"]`)?.focus();
  };
  const tabButton = (which: 'due' | 'upcoming', count: number) => (
    <button type="button" role="tab" id={`${id}-${which}`} data-tab={which} className="d-reminders__tab" aria-selected={tab === which}
      aria-controls={`${id}-panel`} tabIndex={tab === which ? 0 : -1} onClick={() => onTab(which)} onKeyDown={onTabKey}>
      {which === 'due' ? labels.due : labels.upcoming}<span className="d-reminders__count">{count}</span>
    </button>
  );
  return (
    <div ref={root} className="d-reminders" data-layout={layout} role="dialog" aria-modal="false" aria-labelledby={`${id}-title`} onKeyDown={onKeyDown}>
      <div className="d-reminders__head">
        {botInitials === undefined ? null : <span className="d-reminders__bot" aria-hidden="true">{botInitials}</span>}
        <h2 id={`${id}-title`} className="d-reminders__title">{labels.title}</h2>
        <div className="d-reminders__tabs" role="tablist" aria-label={labels.tabs}>
          {tabButton('due', due.length)}
          {tabButton('upcoming', upcoming.length)}
        </div>
      </div>
      <div id={`${id}-panel`} className="d-reminders__body" role="tabpanel" aria-labelledby={`${id}-${tab}`}>
        {status !== undefined ? <p className="d-reminders__status" role="status">{status}</p> : null}
        {shown.length === 0 ? (
          <p className="d-reminders__empty">{tab === 'due' ? labels.emptyDue : labels.emptyUpcoming}</p>
        ) : (
          <ul className="d-reminders__list">
            {shown.map(item => (
              <li key={item.id} className="d-reminders__row" data-due={item.due ? 'true' : undefined}>
                <span className="d-reminders__icon" aria-hidden="true"><Icon name="alarm-clock" size={13} /></span>
                <span className="d-reminders__text">
                  {item.canOpen === true && onOpen !== undefined
                    ? <button type="button" id={`${id}-t-${item.id}`} className="d-reminders__what d-reminders__open" onClick={() => onOpen(item.id)}>{item.text}</button>
                    : <span id={`${id}-t-${item.id}`} className="d-reminders__what">{item.text}</span>}
                  <span className="d-reminders__meta">{item.where === '' ? item.when : `${item.when} · ${item.where}`}</span>
                </span>
                <button type="button" className="d-reminders__action" aria-describedby={`${id}-t-${item.id}`} disabled={item.busy === true}
                  onClick={() => onAction(item.id, item.due ? 'dismiss' : 'cancel')}>{item.due ? labels.dismiss : labels.cancel}</button>
              </li>
            ))}
          </ul>
        )}
      </div>
      <p className="d-reminders__foot">{labels.footer}</p>
    </div>
  );
}
