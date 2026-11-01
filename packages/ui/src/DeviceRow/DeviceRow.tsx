import { useId, useRef, useState } from 'react';
import { ActionMenu, type ActionMenuItem } from '../ActionMenu/ActionMenu.tsx';
import { Button } from '../Button/Button.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { useContextMenuTrigger } from '../internal/contextMenuTrigger.ts';
import './DeviceRow.css';

export interface DeviceRowProps {
  name: string;
  /**
   * The device id's first characters, shown beside `name` when the name is the owner's own label (the label is held by the
   * server, which can change it, so the id stays the identifier). Omit it when `name` is the id.
   */
  idHint?: string;
  tier: 'web' | 'native';
  /** The tier tag; Settings draws none (the prototype's row, Signal task 10), so it is optional. */
  tierLabel?: string;
  own?: boolean;
  ownLabel?: string;
  state?: string;
  lastSeen?: string;
  action?: { label: string; onAction(): void; danger?: boolean };
  /** A second, plain action drawn before `action` (rename, beside remove). */
  extraAction?: { label: string; onAction(): void };
  /**
   * The row's context menu (Signal task 10 fix round 2, Ruling X): a right-click, a long press, Shift+F10 or the ContextMenu
   * key on the row opens it (Settings → Devices puts Rename here, so the row draws only the prototype's one button).
   */
  menu?: { label: string; items: readonly ActionMenuItem[]; onSelect(itemId: string): void };
}

/**
 * One device of the account in Settings → Devices. The tier tag is where the browser's lower-trust
 * fact is shown, and nowhere in the chrome (F9). The action, when given, is described by the
 * device's name, so "remove" is heard with the device it removes; so is the extra one. Every text arrives as a prop.
 */
export function DeviceRow({ name, idHint, tier, tierLabel, own, ownLabel, state, lastSeen, action, extraAction, menu }: DeviceRowProps) {
  const [at, setAt] = useState<{ x: number; y: number } | null>(null);
  const rowRef = useRef<HTMLLIElement>(null);
  const trigger = useContextMenuTrigger<HTMLLIElement>(menu === undefined ? undefined : (_el, point) => setAt(point));
  // A press or a right-click on the row's own button (Sign out, Remove) is that button's: it never opens the row's menu.
  const onButton = (e: { target: EventTarget }) => e.target instanceof Element && e.target.closest('button') !== null;
  const rowTrigger = Object.fromEntries(Object.entries(trigger).map(([name, handler]) => [name,
    name === 'onKeyDown' || name === 'onClickCapture' ? handler : (e: { target: EventTarget }) => { if (!onButton(e)) (handler as (ev: unknown) => void)(e); }]));
  const nameId = useId();
  const idId = useId();
  const describedBy = idHint ? `${nameId} ${idId}` : nameId;
  return (
    <li ref={rowRef} className="d-device-row" data-tier={tier} data-own={own ? 'true' : undefined}
      {...(menu === undefined ? {} : { tabIndex: 0, 'aria-labelledby': nameId, 'aria-keyshortcuts': 'Shift+F10', ...rowTrigger })}>
      <span className="d-device-row__icon" aria-hidden="true"><Icon name="monitor-smartphone" size={15} /></span>
      <div className="d-device-row__main">
        <div className="d-device-row__head">
          {/* A label is free text: <bdi> keeps its direction from reordering the id and tags around it (the id may be on the
              line under it, fix round 2). */}
          <span id={nameId} className="d-device-row__name"><bdi>{name}</bdi></span>
          {idHint ? <span id={idId} className="d-device-row__id">{idHint}</span> : null}
          {tierLabel ? <span className="d-device-row__tag" data-kind="tier">{tierLabel}</span> : null}
          {own && ownLabel ? <span className="d-device-row__tag" data-kind="own">{ownLabel}</span> : null}
        </div>
        {state || lastSeen ? (
          <div className="d-device-row__meta">
            {state ? <span className="d-device-row__state">{state}</span> : null}
            {state && lastSeen ? <span aria-hidden="true"> · </span> : null}
            {lastSeen ? <span className="d-device-row__seen">{lastSeen}</span> : null}
          </div>
        ) : null}
      </div>
      {action || extraAction ? (
        <div className="d-device-row__actions">
          {extraAction ? <Button aria-describedby={describedBy} onClick={extraAction.onAction}>{extraAction.label}</Button> : null}
          {action ? <Button variant={action.danger ? 'danger' : 'default'} aria-describedby={describedBy} onClick={action.onAction}>{action.label}</Button> : null}
        </div>
      ) : null}
      {menu !== undefined && at !== null ? (
        <ActionMenu label={menu.label} items={menu.items} open trigger="none" at={at} returnFocus={() => rowRef.current}
          onOpenChange={open => { if (!open) setAt(null); }} onSelect={item => menu.onSelect(item)} />
      ) : null}
    </li>
  );
}
