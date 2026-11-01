import { useEffect, useId, useRef, useState, type CSSProperties, type KeyboardEvent } from 'react';
import { ActionMenu, type ActionMenuItem } from '../ActionMenu/ActionMenu.tsx';
import { ColorSwatches } from '../ColorSwatches/ColorSwatches.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { PermissionGrid, type PermissionGridGroup, type PermValue } from '../PermissionGrid/PermissionGrid.tsx';
import { hex } from '../internal/hex.ts';
import { useContextMenuTrigger } from '../internal/contextMenuTrigger.ts';
import '../TextField/TextField.css';
import './RoleEditor.css';

export interface RoleEditorRole {
  id: string;
  name: string;
  /** 0xRRGGBB, or 0 for none (drawn in the muted tone). */
  color: number;
  /** Members holding the role. */
  count: number;
  /** The accessible name of the row (the name with its member count, said in words). */
  label: string;
}

export interface RoleEditorProps {
  roles: readonly RoleEditorRole[];
  selectedId: string;
  onSelect(id: string): void;
  /** The "New role" row; absent when this account may not make roles. */
  onNew?: () => void;
  /** The selected role's name as it is being typed, and whether it can be edited (@everyone's cannot). */
  name: string;
  nameReadOnly: boolean;
  onName(value: string): void;
  /** A name is saved when the field loses focus or Enter is pressed. */
  onNameCommit(): void;
  nameError?: string;
  /** The six colours (prototype) with their names; absent for @everyone, which has no colour. */
  colors?: readonly { value: number; name: string }[];
  color: number;
  onColor(value: number): void;
  groups: readonly PermissionGridGroup[];
  values: Readonly<Record<string, PermValue>>;
  onPermission(id: string, on: boolean): void;
  /** A role row's context menu (Ruling X): move, delete and the display options the prototype draws no button for. */
  menu?: { label(role: RoleEditorRole): string; items(role: RoleEditorRole): readonly ActionMenuItem[]; onSelect(roleId: string, itemId: string): void };
  labels: { list: string; newRole: string; name: string; color: string; permissions: string; allow: string; inherit: string; deny: string };
  /** A live line under the editor (saving, saved, refused); errors are alerts. */
  status?: { text: string; alert: boolean; n: number } | null;
}

/**
 * Server settings → Roles as the prototype draws it (README §5.9): a 200px list of roles (colour dot, name, member count,
 * "New role") beside the selected role's editor (name, the six colour swatches, permission groups of toggle rows). On a
 * phone the list sits above the editor. Every change is reported at once; the page saves it.
 */
export function RoleEditor(props: RoleEditorProps) {
  const { roles, selectedId, onSelect, onNew, name, nameReadOnly, onName, onNameCommit, nameError, colors, color, onColor,
    groups, values, onPermission, menu, labels, status } = props;
  const uid = useId();
  const listRef = useRef<HTMLUListElement>(null);
  const [menuFor, setMenuFor] = useState<{ role: RoleEditorRole; at: { x: number; y: number } } | null>(null);

  // Arrow keys move between the role rows (one list, each row a button).
  const onListKey = (event: KeyboardEvent<HTMLUListElement>) => {
    if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
    const buttons = [...(listRef.current?.querySelectorAll<HTMLButtonElement>('.d-role-editor__role, .d-role-editor__new') ?? [])];
    const at = buttons.findIndex(b => b === document.activeElement);
    if (at < 0) return;
    event.preventDefault();
    buttons[(at + (event.key === 'ArrowDown' ? 1 : buttons.length - 1)) % buttons.length]?.focus();
  };

  return (
    <div className="d-role-editor">
      <div className="d-role-editor__grid">
        <ul ref={listRef} className="d-role-editor__list" aria-label={labels.list} onKeyDown={onListKey}>
          {roles.map(role => (
            <RoleRow key={role.id} role={role} selected={role.id === selectedId} onSelect={onSelect}
              onMenu={menu === undefined || menu.items(role).length === 0 ? undefined : at => setMenuFor({ role, at })} />
          ))}
          {onNew ? (
            <li>
              <button type="button" className="d-role-editor__new" onClick={onNew}><Icon name="plus" size={14} />{labels.newRole}</button>
            </li>
          ) : null}
        </ul>
        <div className="d-role-editor__editor">
          <div className="d-role-editor__head">
            <label className="d-role-editor__field d-text-field" htmlFor={`${uid}-name`}>
              <span className="d-text-field__label">{labels.name}</span>
              <NameInput id={`${uid}-name`} value={name} readOnly={nameReadOnly} error={nameError} onChange={onName} onCommit={onNameCommit} />
              {nameError ? <span id={`${uid}-name-error`} className="d-text-field__error" role="alert">{nameError}</span> : null}
            </label>
            {colors ? (
              <ColorSwatches label={labels.color} value={color} options={colors} onChange={onColor} />
            ) : null}
          </div>
          <PermissionGrid label={labels.permissions} groups={groups} mode="toggle" values={values}
            labels={{ allow: labels.allow, inherit: labels.inherit, deny: labels.deny }}
            onChange={(id, v) => onPermission(id, v === 'allow')} />
          <p className="d-role-editor__status" role={status?.alert ? 'alert' : 'status'} key={status?.n ?? 0}>{status?.text ?? ''}</p>
        </div>
      </div>
      {menu !== undefined && menuFor !== null ? (
        <ActionMenu label={menu.label(menuFor.role)} items={menu.items(menuFor.role)} open trigger="none" at={menuFor.at}
          returnFocus={() => listRef.current?.querySelector<HTMLButtonElement>(`[data-role-id="${menuFor.role.id}"]`) ?? null}
          onOpenChange={open => { if (!open) setMenuFor(null); }} onSelect={item => menu.onSelect(menuFor.role.id, item)} />
      ) : null}
    </div>
  );
}

function RoleRow({ role, selected, onSelect, onMenu }: {
  role: RoleEditorRole; selected: boolean; onSelect(id: string): void; onMenu?: (at: { x: number; y: number }) => void;
}) {
  const trigger = useContextMenuTrigger<HTMLButtonElement>(onMenu === undefined ? undefined : (_el, at) => onMenu(at));
  const dot = { '--d-role-dot': role.color === 0 ? 'var(--fg-2)' : hex(role.color) } as CSSProperties;
  return (
    <li>
      <button type="button" className="d-role-editor__role" data-role-id={role.id} aria-current={selected ? 'true' : undefined}
        aria-label={role.label} {...(onMenu === undefined ? {} : { 'aria-keyshortcuts': 'Shift+F10' })} {...trigger}
        onClick={() => onSelect(role.id)}>
        <span className="d-role-editor__dot" style={dot} aria-hidden="true" />
        <span className="d-role-editor__name">{role.name}</span>
        <span className="d-role-editor__count" aria-hidden="true">{role.count}</span>
      </button>
    </li>
  );
}

/** The name field: typing reports each change, and the name is committed on blur or Enter (Escape puts focus back). */
function NameInput({ id, value, readOnly, error, onChange, onCommit }: {
  id: string; value: string; readOnly: boolean; error?: string; onChange(v: string): void; onCommit(): void;
}) {
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => { if (error) ref.current?.focus(); }, [error]);
  return (
    <input ref={ref} id={id} className="d-text-field__input" value={value} disabled={readOnly} autoComplete="off" spellCheck={false}
      aria-invalid={error ? true : undefined} aria-describedby={error ? `${id}-error` : undefined}
      onChange={e => onChange(e.currentTarget.value)} onBlur={() => { if (!readOnly) onCommit(); }}
      onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); onCommit(); } }} />
  );
}
