import { useEffect, useId, useRef, useState, type KeyboardEvent } from 'react';
import { Avatar } from '../Avatar/Avatar.tsx';
import { Icon, type IconName } from '../Icon/Icon.tsx';
import { usePhoneLayout } from '../internal/phoneLayout.ts';
import './SearchPalette.css';

/** One result: a 24px glyph tile (`icon`) or a 24px avatar (`avatar`), the label and its sub-line. `id` is unique across the groups. */
export interface SearchPaletteItem { id: string; label: string; sub?: string; icon?: IconName; avatar?: { name: string; bot?: boolean } }
/** A group of results under its 10px uppercase heading (Channels, People, Messages, Actions, …), in the order given. */
export interface SearchPaletteGroup { id: string; label: string; items: readonly SearchPaletteItem[] }

export type SearchPaletteProps = {
  open: boolean;
  query: string;
  /** The result groups, in order; an empty group is not drawn. The arrows move through every item of every group. */
  groups: readonly SearchPaletteGroup[];
  /**
   * `search` names the textbox and the palette, `placeholder` is its hint, `empty` the line when nothing is listed, `close` names the
   * esc key cap (which closes), `hints` the footer's key words ("move", "open"), `note` the footer's right-hand line.
   */
  labels: { search: string; placeholder?: string; empty: string; close: string; hints?: { move: string; open: string }; note?: string };
  /** A status that replaces the empty text, such as a picked result that is no longer available. */
  status?: string;
  /** True: a desktop palette is a modal native dialog with a scrim, focus moves in on open and back to the opener on close (R-S4). Default: the inline section. */
  modal?: boolean;
  onQuery(s: string): void;
  onPick(id: string): void;
  onClose(): void;
};

/** True while an input method is composing: its Enter and Escape belong to the IME (Flow 05, W-07). */
function composing(event: KeyboardEvent): boolean {
  return event.nativeEvent.isComposing || event.keyCode === 229;
}

/**
 * The search palette (Signal §5.8): a 50px input row (the accent glyph, the combobox textbox, the esc key cap that closes), the
 * results in groups (a 10px uppercase heading, then rows of at least 38px: a 24px glyph tile or avatar, the label over its
 * sub-line, the selected row on --surface-hi with a 2px accent bar and ↵), and the footer of key words and the note. The textbox
 * keeps focus while the arrows move the active result across the groups. One polite status stays mounted and only its text
 * changes (M-03). On a desktop it is a modal native dialog at 14vh, centred by `margin: 0 auto` (never a transform), over the
 * --scrim; opening focuses the textbox and closing returns focus to the opener.
 */
export function SearchPalette({ open, query, groups, labels, status, modal = false, onQuery, onPick, onClose }: SearchPaletteProps) {
  // The active row is held by its id, so results that arrive later (the message search) never change what Enter opens (fix round 2).
  const [activeId, setActiveId] = useState<string | null>(null);
  const listId = useId();
  const phone = usePhoneLayout();
  const asDialog = modal && !phone;
  const inputRef = useRef<HTMLInputElement>(null);
  const dialogRef = useRef<HTMLDialogElement>(null);
  const picked = useRef(false);
  const resultsRef = useRef<HTMLDivElement>(null);
  // A desktop palette is a modal <dialog> (R-S1): the browser traps focus, makes the shell inert and draws the ::backdrop scrim.
  useEffect(() => {
    const el = dialogRef.current;
    if (!open || !asDialog || !el) return;
    if (!el.open) el.showModal();
    return () => { if (el.open) el.close(); };
  }, [open, asDialog]);
  // Focus goes to the textbox on open and back to whatever opened the palette when it closes (contract 2), unless a pick moved it on purpose.
  useEffect(() => {
    if (!open || !modal) return;
    picked.current = false;
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    inputRef.current?.focus();
    return () => { if (!picked.current && opener?.isConnected) opener.focus(); };
  }, [open, modal]);
  const pick = (id: string) => { picked.current = true; onPick(id); };
  const shown = groups.filter(g => g.items.length > 0);
  const items = shown.flatMap(g => g.items);
  const found = activeId === null ? -1 : items.findIndex(i => i.id === activeId);
  const activeIndex = found < 0 ? 0 : found;
  const currentId = items.length > 0 ? items[activeIndex].id : null;
  // The active row stays in view as the arrows move it (only when it changes).
  useEffect(() => {
    resultsRef.current?.querySelector<HTMLElement>('[role="option"][aria-selected="true"]')?.scrollIntoView?.({ block: 'nearest' });
  }, [currentId]);
  if (!open) return null;
  const domId = (id: string) => `${listId}-${id}`;
  const handleInputKey = (event: KeyboardEvent<HTMLInputElement>) => {
    if (composing(event)) return;
    if (event.key === 'ArrowDown' && items.length) { event.preventDefault(); setActiveId(items[(activeIndex + 1) % items.length].id); }
    else if (event.key === 'ArrowUp' && items.length) { event.preventDefault(); setActiveId(items[(activeIndex - 1 + items.length) % items.length].id); }
    else if (event.key === 'Enter' && items.length) { event.preventDefault(); pick(items[activeIndex].id); }
  };
  // Escape closes from anywhere in the palette (Flow 05 "any top overlay"), never during IME composition.
  const handleKey = (event: KeyboardEvent<HTMLElement>) => {
    if (event.key !== 'Escape') return;
    event.preventDefault();
    if (!composing(event)) onClose();
  };
  const text = status ?? (items.length ? '' : labels.empty);
  const palette = <section className="d-search-palette" data-layout={phone ? 'phone' : undefined} role="search" aria-label={labels.search} onKeyDown={handleKey}>
    <div className="d-search-palette__field">
      <span className="d-search-palette__glyph" aria-hidden="true"><Icon name="search" size={16} /></span>
      <input ref={inputRef} type="text" role="combobox" aria-label={labels.search} placeholder={labels.placeholder} aria-expanded={items.length > 0} aria-autocomplete="list"
        aria-controls={items.length ? listId : undefined} aria-activedescendant={items.length ? domId(items[activeIndex].id) : undefined}
        value={query} onChange={event => { setActiveId(null); onQuery(event.target.value); }} onKeyDown={handleInputKey} />
      <button type="button" className="d-search-palette__esc" aria-label={labels.close} onClick={onClose}>esc</button>
    </div>
    {/* A long list scrolls: the region is a tab stop so a keyboard can scroll it too (the arrows in the textbox keep the active row in view). */}
    <div ref={resultsRef} className="d-search-palette__results" tabIndex={items.length > 0 ? 0 : undefined}>
      {items.length ? <div id={listId} role="listbox" aria-label={labels.search}>{shown.map(group => (
        <div key={group.id} role="group" aria-label={group.label} className="d-search-palette__section" data-group={group.id}>
          <div className="d-search-palette__group" aria-hidden="true">{group.label}</div>
          {group.items.map(item => {
            const selected = item.id === currentId;
            return (
              <div role="option" id={domId(item.id)} key={item.id} aria-selected={selected} onClick={() => pick(item.id)} onMouseEnter={() => setActiveId(item.id)}>
                {item.avatar !== undefined
                  ? <span className="d-search-palette__avatar" aria-hidden="true"><Avatar name={item.avatar.name} size="xs" shape={item.avatar.bot === true ? 'bot' : 'circle'} /></span>
                  : <span className="d-search-palette__tile" aria-hidden="true"><Icon name={item.icon ?? 'search'} size={13} /></span>}
                <span className="d-search-palette__text">
                  <span className="d-search-palette__label">{item.label}</span>
                  {item.sub ? <span className="d-search-palette__meta">{item.sub}</span> : null}
                </span>
                {selected ? <span className="d-search-palette__enter" aria-hidden="true">↵</span> : null}
              </div>
            );
          })}
        </div>
      ))}</div> : null}
      <div className="d-search-palette__status" role="status" aria-live="polite">{text}</div>
    </div>
    {labels.hints || labels.note ? <div className="d-search-palette__foot">
      {labels.hints ? <span className="d-search-palette__hints"><span>↑↓ {labels.hints.move}</span><span>↵ {labels.hints.open}</span></span> : null}
      <span className="d-search-palette__spacer" />
      {labels.note ? <span className="d-search-palette__note">{labels.note}</span> : null}
    </div> : null}
  </section>;
  if (!asDialog) return palette;
  // Escape is handled on the section (it also covers IME composition); the native cancel is the fallback (a back gesture). A click on the scrim closes.
  return <dialog ref={dialogRef} className="d-search-palette-dialog" aria-label={labels.search}
    onCancel={event => { event.preventDefault(); onClose(); }}
    onClick={event => { if (event.target === dialogRef.current) onClose(); }}>{palette}</dialog>;
}
