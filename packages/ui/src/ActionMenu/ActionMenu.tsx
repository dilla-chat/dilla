import { Fragment, useEffect, useId, useLayoutEffect, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { Icon, type IconName } from '../Icon/Icon.tsx';
import './ActionMenu.css';

/** The arrow order of a menu: its items, and a range in its head (the person menu's volume). */
const NAV = '[role^="menuitem"], input[type="range"]';

export interface ActionMenuItem {
  id: string; label: string; danger?: boolean;
  /** Set on a single-choice menu: the item is a `menuitemradio` and this is its `aria-checked`. */
  checked?: boolean;
  /** A 14px glyph before the label (the person menu, Signal §5.4). */
  icon?: IconName;
  /** An on/off item: a `menuitemcheckbox` with the prototype's 14px box; choosing it toggles and keeps the menu open. */
  check?: boolean;
  /** A 1px rule above the item (the prototype's separator before Kick, task 9). */
  sep?: boolean;
  /**
   * Consecutive items with the same `group` sit in one `role="group"` named by it (WAI-ARIA APG: a set of menuitemradio
   * items is a group with a label). Signal task 10: a conversation's or a server's notify mode.
   */
  group?: string;
  /** Wave B task 4: a trailing hint, 10px --fg-faint (a shortcut's keys, a room's head count). */
  hint?: string;
  /** Wave B task 4: a small uppercase heading above the item (the person menu's "Move to"). */
  heading?: string;
  /** Wave B task 4: a room row of the person menu's "Move to" block (30px, indented, the room glyph at 13px). */
  room?: boolean;
  /** `aria-keyshortcuts` of the item (Control+Shift+M). */
  keys?: string;
  /** The item's accessible name when its visible text (a label and a hint) reads badly ('lounge, 2 in voice'). */
  ariaLabel?: string;
}

export interface ActionMenuProps {
  /** The trigger's accessible name and the menu's (`actions for Astrid`). */
  label: string;
  items: readonly ActionMenuItem[];
  open: boolean;
  onOpenChange(open: boolean): void;
  onSelect(id: string): void;
  /** Where the popup sits when it was opened by a context menu: the pointer, clamped to the viewport. */
  at?: { x: number; y: number } | null;
  /**
   * A stable key for the trigger (`data-focus-key`): a Dialog opened from this menu finds the trigger by it on close,
   * even when the row was re-mounted meanwhile.
   */
  focusKey?: string;
  /** A monoline icon on the trigger in place of the "⋯" glyph. */
  icon?: IconName;
  /** The trigger's content in place of the icon or glyph (the user panel's avatar and name block). The trigger is still one button named by `label`. */
  triggerContent?: ReactNode;
  /** The id(s) of the element(s) that name the trigger, in place of `label` (a trigger whose visible text must be in its name, WCAG 2.5.3). The menu keeps `label`. */
  labelledBy?: string;
  /** Extra classes on the wrapper and on the trigger, for a caller that lays the trigger out itself. */
  className?: string;
  triggerClassName?: string;
  /**
   * 'none': no trigger is drawn; the menu exists only while `open`, at `at` (a context menu: a right-click, a long press,
   * Shift+F10 on a row; Ruling X). Escape, a choice and Tab hand focus to `returnFocus()` (the row) instead of a trigger.
   */
  trigger?: 'button' | 'none';
  returnFocus?(): HTMLElement | null;
  /** The trigger's tab index, for a trigger inside a roving toolbar (the message toolbar's "more"). */
  triggerTabIndex?: number;
  /**
   * Content above the items inside the popup (the person menu's header and its volume range). The items keep their own
   * `role="menu"`; a range in the head joins the arrow order (ArrowUp/Down move on, ArrowLeft/Right change it).
   */
  head?: ReactNode;
  /** Extra classes on the popup (the person menu's look). */
  popupClassName?: string;
}

/**
 * A WAI-ARIA menu button. Enter, Space or ArrowDown on the trigger opens the menu on its first item; ArrowUp opens it
 * on the last. Arrows wrap, Home and End jump. Escape closes and refocuses the trigger, Tab closes and moves on, and a
 * pointerdown outside closes. `open` is the caller's, so a row can also open the menu from a context menu (`at`).
 */
export function ActionMenu({ label, items, open, onOpenChange, onSelect, at, focusKey, icon, triggerContent, labelledBy, className, triggerClassName, trigger = 'button', returnFocus, triggerTabIndex, head, popupClassName }: ActionMenuProps) {
  const base = useId();
  const triggerRef = useRef<HTMLButtonElement>(null);
  const rootRef = useRef<HTMLSpanElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const startAt = useRef<'first' | 'last'>('first');
  const [clamped, setClamped] = useState<{ x: number; y: number } | null>(null);
  const [placement, setPlacement] = useState<'top' | 'bottom'>('bottom');
  // Where focus goes back when the menu closes from inside it: the trigger, or the caller's element when there is none.
  const focusBack = () => { (trigger === 'none' ? returnFocus?.() ?? null : triggerRef.current)?.focus(); };

  // Focus goes to the first item (the last after ArrowUp) every time the menu opens.
  useEffect(() => {
    if (!open) return;
    const list = menuRef.current?.querySelectorAll<HTMLElement>(NAV);
    if (!list || list.length === 0) return;
    (startAt.current === 'last' ? list[list.length - 1] : list[0])?.focus();
    startAt.current = 'first';
  }, [open]);

  // The pointer position, kept inside the popup's containing block once the popup's size is known. A fixed popup is
  // placed against the viewport, except inside the app shell, which is a size container and so its containing block:
  // there the position is relative to the shell's corner and clamped to the shell.
  useLayoutEffect(() => {
    if (!open || !at) { setClamped(null); return; }
    const w = menuRef.current?.offsetWidth ?? 0;
    const h = menuRef.current?.offsetHeight ?? 0;
    const shell = rootRef.current?.closest('.d-app-shell')?.getBoundingClientRect();
    const box = shell ?? { left: 0, top: 0, width: window.innerWidth, height: window.innerHeight };
    setClamped({
      x: Math.max(0, Math.min(at.x - box.left, box.width - w)),
      y: Math.max(0, Math.min(at.y - box.top, box.height - h)),
    });
  }, [open, at]);

  // Without `at` the popup hangs under the trigger, or above it when the trigger is in the bottom half of its nearest
  // scrollable ancestor (the viewport when there is none).
  useLayoutEffect(() => {
    if (!open || at || !triggerRef.current) return;
    let scroller: Element | null = triggerRef.current.parentElement;
    while (scroller && !/(auto|scroll)/.test(getComputedStyle(scroller).overflowY)) scroller = scroller.parentElement;
    const area = scroller?.getBoundingClientRect() ?? { top: 0, height: window.innerHeight };
    const t = triggerRef.current.getBoundingClientRect();
    setPlacement(t.top + t.height / 2 > area.top + area.height / 2 ? 'top' : 'bottom');
  }, [open, at]);

  // A pointerdown anywhere outside the trigger and the popup closes the menu.
  useEffect(() => {
    if (!open) return;
    const down = (e: PointerEvent) => {
      if (e.target instanceof Node && rootRef.current?.contains(e.target)) return;
      onOpenChange(false);
    };
    document.addEventListener('pointerdown', down, true);
    return () => document.removeEventListener('pointerdown', down, true);
  }, [open, onOpenChange]);

  const onTriggerKey = (e: KeyboardEvent<HTMLButtonElement>) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      startAt.current = e.key === 'ArrowUp' ? 'last' : 'first';
      onOpenChange(true);
    }
  };

  const onMenuKey = (e: KeyboardEvent<HTMLDivElement>) => {
    const list = Array.from(menuRef.current?.querySelectorAll<HTMLElement>(NAV) ?? []);
    const at0 = list.indexOf(document.activeElement as HTMLElement);
    const go = (i: number) => { e.preventDefault(); list[(i + list.length) % list.length]?.focus(); };
    switch (e.key) {
      case 'ArrowDown': go(at0 + 1); break;
      case 'ArrowUp': go(at0 < 0 ? list.length - 1 : at0 - 1); break;
      case 'Home': go(0); break;
      case 'End': go(list.length - 1); break;
      case 'Escape':
        e.preventDefault();
        e.stopPropagation();
        onOpenChange(false);
        focusBack();
        break;
      case 'Tab':
        // Focus goes to the trigger first, so the browser's Tab moves on from there rather than into the popup.
        focusBack();
        onOpenChange(false);
        break;
    }
  };

  const select = (item: ActionMenuItem) => {
    // An on/off item toggles in place: the menu stays open on it (the prototype's "Mute for me").
    if (item.check !== undefined) { onSelect(item.id); return; }
    onOpenChange(false);
    focusBack();
    onSelect(item.id);
  };

  const popupClass = popupClassName === undefined ? 'd-action-menu__popup' : `d-action-menu__popup ${popupClassName}`;
  const itemButton = (item: ActionMenuItem) => (
    <Fragment key={item.id}>{item.sep === true ? <div className="d-action-menu__sep" role="separator" /> : null}{item.heading !== undefined ? <div className="d-action-menu__heading" aria-hidden="true">{item.heading}</div> : null}<button type="button" role={item.check !== undefined ? 'menuitemcheckbox' : item.checked === undefined ? 'menuitem' : 'menuitemradio'}
      aria-checked={item.check ?? item.checked} tabIndex={-1} className="d-action-menu__item" data-id={item.id} data-room={item.room ? 'true' : undefined} aria-keyshortcuts={item.keys} aria-label={item.ariaLabel}
      data-danger={item.danger ? 'true' : undefined} onClick={() => select(item)}>
      {item.icon !== undefined ? <span className="d-action-menu__icon" aria-hidden="true"><Icon name={item.icon} size={item.room ? 13 : 14} /></span> : null}
      {/* A label longer than the menu ends in an ellipsis; its full text is the title (fix round 2). */}
      <span className="d-action-menu__label" title={item.label}>{item.label}</span>
      {item.hint !== undefined ? <span className="d-action-menu__hint" aria-hidden="true">{item.hint}</span> : null}
      {item.checked === true ? <span className="d-action-menu__tick" aria-hidden="true"><Icon name="check" size={13} /></span> : null}
      {item.check !== undefined ? <span className="d-action-menu__check" data-on={item.check ? 'true' : undefined} aria-hidden="true"><Icon name="check" size={10} stroke={3} /></span> : null}
    </button></Fragment>
  );
  const itemButtons: ReactNode[] = [];
  for (let i = 0; i < items.length;) {
    const group = items[i].group;
    if (group === undefined) { itemButtons.push(itemButton(items[i])); i += 1; continue; }
    const run: ActionMenuItem[] = [];
    while (i < items.length && items[i].group === group) { run.push(items[i]); i += 1; }
    itemButtons.push(<div key={`group:${group}:${run[0].id}`} role="group" aria-label={group} className="d-action-menu__group">{run.map(itemButton)}</div>);
  }
  const style = at
    ? { position: 'fixed' as const, left: `${clamped?.x ?? at.x}px`, top: `${clamped?.y ?? at.y}px` }
    : undefined;

  return (
    <span ref={rootRef} className={className === undefined ? 'd-action-menu' : `d-action-menu ${className}`}>
      {trigger === 'none' ? null : <button ref={triggerRef} type="button" className={triggerClassName === undefined ? 'd-action-menu__trigger' : `d-action-menu__trigger ${triggerClassName}`} aria-label={labelledBy === undefined ? label : undefined} aria-labelledby={labelledBy} aria-haspopup="menu" data-focus-key={focusKey} tabIndex={triggerTabIndex}
        aria-expanded={open} aria-controls={open ? `${base}-menu` : undefined}
        onClick={() => onOpenChange(!open)} onKeyDown={onTriggerKey}>
        {triggerContent ?? (icon !== undefined ? <Icon name={icon} size={22} /> : <span aria-hidden="true">⋯</span>)}
      </button>}
      {open ? (head === undefined ? (
        <div ref={menuRef} id={`${base}-menu`} role="menu" aria-label={label} className={popupClass} style={style} data-placement={at ? undefined : placement}
          onKeyDown={onMenuKey}>
          {itemButtons}
        </div>
      ) : (
        <div ref={menuRef} className={popupClass} style={style} data-placement={at ? undefined : placement} onKeyDown={onMenuKey}>
          {head}
          <div id={`${base}-menu`} role="menu" aria-label={label} className="d-action-menu__items">{itemButtons}</div>
        </div>
      )) : null}
    </span>
  );
}
