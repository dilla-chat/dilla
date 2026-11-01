import { useRef, useState, type ReactNode } from 'react';
import { ActionMenu, type ActionMenuItem } from '../ActionMenu/ActionMenu.tsx';
import { Icon, type IconName } from '../Icon/Icon.tsx';
import { useContextMenuTrigger } from '../internal/contextMenuTrigger.ts';
import './SettingsRow.css';

export interface SettingsRowProps {
  id: string;
  label: string;
  hint?: string;
  /** The row's 32px icon tile (README §5.9): an accent glyph on --surface-tile. */
  icon?: IconName;
  /** A small uppercase tag after the label (the prototype's BETA, THIS BROWSER). */
  tag?: string;
  /** Renders the label as a heading of this level, so assistive technology can navigate a section by its rows. */
  headingLevel?: 3 | 4;
  /** Complex forms sit below their introduction instead of inside the compact control column. */
  layout?: 'inline' | 'stacked';
  /**
   * The row's context menu (Ruling X, as DeviceRow's): a right-click, a long press, Shift+F10 or the ContextMenu key on the
   * row opens it. Task 10c puts Glenn's Rename, Refresh status and Disable here, beside the prototype's one Restart button.
   */
  menu?: { label: string; items: readonly ActionMenuItem[]; onSelect(itemId: string): void };
  /** A settings-search anchor (`data-setting`) on the row itself, so a row can be found without a wrapper. */
  anchor?: string;
  children: ReactNode;
}

/**
 * One labelled row of Settings (README §5.9): an optional icon tile, the label and hint, and the control on the right.
 * The row is a group named by its visible label and described by its hint. Consecutive rows draw one 10px-radius group
 * with hairlines between them; the row wraps on narrow widths. Complex forms can always stack below their introduction.
 */
export function SettingsRow({ id, label, hint, icon, tag, headingLevel, layout = 'inline', menu, anchor, children }: SettingsRowProps) {
  const Label = headingLevel === undefined ? 'span' : (`h${headingLevel}` as const);
  const [at, setAt] = useState<{ x: number; y: number } | null>(null);
  const rowRef = useRef<HTMLDivElement>(null);
  const trigger = useContextMenuTrigger<HTMLDivElement>(menu === undefined ? undefined : (_el, point) => setAt(point));
  // A press on the row's own controls is theirs: it never opens the row's menu.
  const onControl = (e: { target: EventTarget }) => e.target instanceof Element && e.target.closest('button, input, select, textarea, a') !== null;
  const rowTrigger = Object.fromEntries(Object.entries(trigger).map(([name, handler]) => [name,
    name === 'onKeyDown' || name === 'onClickCapture' ? handler : (e: { target: EventTarget }) => { if (!onControl(e)) (handler as (ev: unknown) => void)(e); }]));
  return (
    <div ref={rowRef} className={`d-settings-row${layout === 'stacked' ? ' d-settings-row--stacked' : ''}`} data-row role="group" aria-labelledby={`${id}-label`} aria-describedby={hint ? `${id}-hint` : undefined}
      data-setting={anchor}
      {...(menu === undefined ? {} : { tabIndex: 0, 'aria-keyshortcuts': 'Shift+F10', ...rowTrigger })}>
      {icon ? <span className="d-settings-row__icon" aria-hidden="true"><Icon name={icon} size={15} /></span> : null}
      <div className="d-settings-row__text">
        <Label id={`${id}-label`} className="d-settings-row__label">{label}{tag ? <span className="d-settings-row__tag">{tag}</span> : null}</Label>
        {hint ? <p id={`${id}-hint`} className="d-settings-row__hint">{hint}</p> : null}
      </div>
      <div className="d-settings-row__control">{children}</div>
      {menu !== undefined && at !== null ? (
        <ActionMenu label={menu.label} items={menu.items} open trigger="none" at={at} returnFocus={() => rowRef.current}
          onOpenChange={open => { if (!open) setAt(null); }} onSelect={item => menu.onSelect(item)} />
      ) : null}
    </div>
  );
}
