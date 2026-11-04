import { useEffect, type KeyboardEvent, type MouseEvent, type ReactNode, type Ref } from 'react';
import { Avatar } from '../Avatar/Avatar.tsx';
import { Icon, type IconName } from '../Icon/Icon.tsx';
import './CommandPicker.css';

/*
 * Signal wave B Task 2 (README §6.1, §7.2): the prototype's command picker. Four pieces the composer assembles:
 * the `/` list above the box (CommandList), the chips that replace the textarea (CommandChips + CommandField), the
 * suggestions above the active chip (CommandSuggestions) and the 11px line above the chips (CommandBar). None of them
 * owns state: the web's CommandPicker holds the draft, the active field and the run.
 */

export interface CommandListItem { id: string; icon: IconName; name: string; args: string; description: string; source: string }

export interface CommandListProps {
  id: string;
  items: readonly CommandListItem[];
  activeId: string | null;
  /** The header's "↑↓ move · tab complete · esc close" (never on a phone; CSS also hides it there). */
  showHint: boolean;
  labels: { title: string; hint: string; noMatch: string; sendAsText: string };
  /** The `/` draft the list filters by (the no-match row's subject). */
  query: string;
  onActive(id: string): void;
  onPick(id: string): void;
  onSendAsText(): void;
}

/** The option id a command row carries: the field that drives the list points at it with `aria-activedescendant`. */
export const commandOptionId = (listId: string, itemId: string): string => `${listId}-${itemId.replaceAll(' ', '-')}`;

/**
 * The commands that match the `/` draft, as a listbox the draft field points at: no row takes focus. A pointer down on a
 * row picks it and keeps the focus in the field (its default is prevented). With no match the popover says so and offers
 * "Send as text", which posts the draft literally.
 */
export function CommandList({ id, items, activeId, showHint, labels, query, onActive, onPick, onSendAsText }: CommandListProps) {
  const active = activeId === null ? undefined : commandOptionId(id, activeId);
  // The field keeps focus while ↑↓ move the selection, so the list scrolls the active row into view itself.
  useEffect(() => {
    if (active === undefined) return;
    const row = document.getElementById(active);
    if (row !== null && typeof row.scrollIntoView === 'function') row.scrollIntoView({ block: 'nearest' });
  }, [active]);
  return (
    <div className="d-cmd-list" data-query={query}>
      <div className="d-cmd-list__head" aria-hidden="true">
        {labels.title}<span className="d-cmd-list__spacer" />
        {showHint ? <span className="d-cmd-list__hint">{labels.hint}</span> : null}
      </div>
      {/* Out of the tab order: the draft field keeps focus and ↑↓ move the selection, which scrolls into view (the effect above). */}
      <div className="d-cmd-list__scroll" tabIndex={-1}>
        <div role="listbox" id={id} aria-label={labels.title} aria-activedescendant={items.length > 0 ? active : undefined} className="d-cmd-list__list">
          {items.map(item => (
            <div key={item.id} role="option" id={commandOptionId(id, item.id)} aria-selected={item.id === activeId}
              aria-label={`/${item.name}, ${item.description}`} className="d-cmd-row"
              onMouseEnter={() => onActive(item.id)}
              onMouseDown={(e: MouseEvent<HTMLDivElement>) => { e.preventDefault(); onPick(item.id); }}>
              <span className="d-cmd-row__icon" aria-hidden="true"><Icon name={item.icon} size={14} /></span>
              <span className="d-cmd-row__text" aria-hidden="true">
                <span className="d-cmd-row__line"><b className="d-cmd-row__name">/{item.name}</b>{item.args === '' ? null : <> <span className="d-cmd-row__args">{item.args}</span></>}</span>
                <span className="d-cmd-row__desc">{item.description}</span>
              </span>
              <span className="d-cmd-row__source" aria-hidden="true">{item.source}</span>
            </div>
          ))}
        </div>
        {items.length === 0 ? (
          <div className="d-cmd-list__none">
            <span className="d-cmd-list__none-text">{labels.noMatch}</span>
            <button type="button" className="d-cmd-list__send" onMouseDown={e => e.preventDefault()} onClick={() => onSendAsText()}>{labels.sendAsText}</button>
          </div>
        ) : null}
      </div>
    </div>
  );
}

export interface CommandChipsProps { icon: IconName; name: string; children: ReactNode }

/** The chip row in the composer box: the accent `/name` token, then one CommandField per argument. */
export function CommandChips({ icon, name, children }: CommandChipsProps) {
  return (
    <div className="d-cmd-chips">
      {/* The prototype writes `<icon>/{name}` in a flex row: the slash and the name are two flex items, 6px apart. */}
      <span className="d-cmd-chips__name"><Icon name={icon} size={12} /><span>/</span><span>{name}</span></span>
      {children}
    </div>
  );
}

export interface CommandFieldProps {
  label: string;
  optional: boolean;
  value: string;
  placeholder: string;
  active: boolean;
  invalid: boolean;
  /** A command without arguments: one label-less chip that says "press ↵ to run" and takes no text. */
  ghost?: boolean;
  /**
   * A free-text argument (Jonas, fix round 1): a textarea that looks exactly like the input chip while it holds one line
   * and grows a line at a time (Shift+Enter breaks the line; Enter keeps the picker's accept/run rules).
   */
  multiline?: boolean;
  /** The suggestions listbox this chip drives, while it shows. */
  listId?: string;
  activeOptionId?: string;
  inputRef?: Ref<HTMLInputElement | HTMLTextAreaElement>;
  onChange(value: string): void;
  onFocus(): void;
  onKeyDown(e: KeyboardEvent<HTMLInputElement | HTMLTextAreaElement>): void;
  onBlur?(e: React.FocusEvent<HTMLInputElement | HTMLTextAreaElement>): void;
}

/**
 * One argument chip: the label tile (accent while active, danger while invalid; optional labels end in "?") and an input
 * as wide as its value or placeholder (`max(len, 4)ch + 16px`, the longest line for a free-text chip). The input keeps the
 * field's own name.
 */
export function CommandField({ label, optional, value, placeholder, active, invalid, ghost, multiline, listId, activeOptionId, inputRef, onChange, onFocus, onKeyDown, onBlur }: CommandFieldProps) {
  const lines = value.split('\n');
  const longest = Math.max(...lines.map(line => line.length));
  const width = `calc(${String(Math.max(longest, placeholder.length, 4))}ch + 1rem)`;
  const expanded = listId !== undefined;
  const name = ghost || label === '' ? placeholder : label;
  const common = {
    className: 'd-cmd-field__input', style: { width }, value, placeholder, 'aria-label': name,
    'aria-invalid': invalid ? true : undefined, spellCheck: false, autoComplete: 'off',
    onFocus: () => onFocus(), onKeyDown, onBlur,
  } as const;
  return (
    <label className="d-cmd-field" data-active={active ? 'true' : undefined} data-invalid={invalid ? 'true' : undefined} data-ghost={ghost ? 'true' : undefined}
      data-multiline={multiline === true && lines.length > 1 ? 'true' : undefined}>
      {ghost || label === '' ? null : <span className="d-cmd-field__label" aria-hidden="true">{label}{optional ? '?' : ''}</span>}
      {multiline === true && !ghost
        ? <textarea ref={inputRef as Ref<HTMLTextAreaElement>} {...common} rows={lines.length} data-multiline=""
          onChange={e => onChange(e.currentTarget.value)} />
        : <input ref={inputRef as Ref<HTMLInputElement>} {...common} readOnly={ghost === true}
          {...(ghost ? {} : { role: 'combobox', 'aria-autocomplete': 'list' as const, 'aria-expanded': expanded })}
          {...(expanded ? { 'aria-controls': listId } : {})}
          {...(expanded && activeOptionId !== undefined ? { 'aria-activedescendant': activeOptionId } : {})}
          onChange={e => { if (!ghost) onChange(e.currentTarget.value); }} />}
    </label>
  );
}

export interface CommandSuggestion { id: string; label: string; sub?: string; avatar?: { name: string }; icon?: IconName }

export interface CommandSuggestionsProps {
  id: string;
  title: string;
  items: readonly CommandSuggestion[];
  activeId: string | null;
  onActive(id: string): void;
  onPick(id: string): void;
}

/** The suggestions for the active chip: Members, Durations, When, Suggestions or Commands; picked with a pointer down. */
export function CommandSuggestions({ id, title, items, activeId, onActive, onPick }: CommandSuggestionsProps) {
  const active = activeId === null ? undefined : commandOptionId(id, activeId);
  useEffect(() => {
    if (active === undefined) return;
    const row = document.getElementById(active);
    if (row !== null && typeof row.scrollIntoView === 'function') row.scrollIntoView({ block: 'nearest' });
  }, [active]);
  return (
    <div className="d-cmd-sugs">
      <div className="d-cmd-sugs__title" aria-hidden="true">{title}</div>
      <div role="listbox" id={id} aria-label={title} className="d-cmd-sugs__list">
        {items.map(item => (
          <div key={item.id} role="option" id={commandOptionId(id, item.id)} aria-selected={item.id === activeId} className="d-cmd-sug"
            aria-label={item.sub !== undefined && item.sub !== '' ? `${item.label}, ${item.sub}` : item.label}
            onMouseEnter={() => onActive(item.id)}
            onMouseDown={(e: MouseEvent<HTMLDivElement>) => { e.preventDefault(); onPick(item.id); }}>
            {item.avatar !== undefined
              ? <span className="d-cmd-sug__avatar" aria-hidden="true"><Avatar name={item.avatar.name} size="sug" /></span>
              : <span className="d-cmd-sug__icon" aria-hidden="true"><Icon name={item.icon ?? 'hash'} size={12} /></span>}
            <span className="d-cmd-sug__text" aria-hidden="true">
              <span className="d-cmd-sug__label">{item.label}</span>
              {item.sub !== undefined && item.sub !== '' ? <span className="d-cmd-sug__sub">{item.sub}</span> : null}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}

export interface CommandBarProps { icon: IconName; text: string; tone: 'info' | 'danger'; hint?: string }

/**
 * The 11px line above the chips: the command's description (or the reminder's preview), and in `danger` an error with the
 * warning icon (announced). The keyboard hint shows on a desktop only.
 */
export function CommandBar({ icon, text, tone, hint }: CommandBarProps) {
  return (
    <div className="d-cmd-bar" data-tone={tone}>
      <span className="d-cmd-bar__icon" aria-hidden="true"><Icon name={icon} size={12} /></span>
      <span className="d-cmd-bar__text" {...(tone === 'danger' ? { role: 'alert' } : {})}>{text}</span>
      {hint !== undefined ? <span className="d-cmd-bar__hint" aria-hidden="true">{hint}</span> : null}
    </div>
  );
}
