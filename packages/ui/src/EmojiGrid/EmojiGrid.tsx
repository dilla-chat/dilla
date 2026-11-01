import { useEffect, useRef, useState, type FocusEvent, type KeyboardEvent } from 'react';
import './EmojiGrid.css';

export interface EmojiGridProps {
  label: string;
  items: readonly { emoji: string; name: string }[];
  onPick(emoji: string): void;
  onClose(): void;
  /** The composer's picker (§5.5): a search field over the grid, focused on open, that filters by name. */
  search?: { label: string; placeholder: string };
  /** The panel's id, for the opener's `aria-controls`. */
  id?: string;
}

const COLUMNS = 8;

/** The index of the event's target among the buttons, or -1. */
function indexOfTarget(list: readonly HTMLButtonElement[], target: EventTarget): number {
  return target instanceof HTMLButtonElement ? list.indexOf(target) : -1;
}

/**
 * The fixed reactions as a non-modal popover dialog: buttons in 8 columns named by their names, one tab stop that
 * follows focus, the first focused on mount. Arrows move in two dimensions without wrapping, Home and End go to the
 * ends; Enter, Space or a click picks (the native button). Escape closes and goes no further (the row never sees
 * it); a pointer down outside the grid closes it too. Returning focus to the opener is the caller's.
 */
export function EmojiGrid({ label, items: all, onPick, onClose, search, id }: EmojiGridProps) {
  const ref = useRef<HTMLDivElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const [focus, setFocus] = useState(0);
  const [query, setQuery] = useState('');
  const q = query.trim().toLowerCase();
  const items = q === '' ? all : all.filter(i => i.name.toLowerCase().includes(q));
  // The footer names the emoji under the pointer or the focus (the prototype's ":shield:" line).
  const [shown, setShown] = useState<{ emoji: string; name: string } | null>(null);
  const named = shown ?? items[Math.min(focus, items.length - 1)] ?? null;
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;

  const buttons = (): HTMLButtonElement[] => Array.from(ref.current?.querySelectorAll('.d-emoji-grid__emoji') ?? []);

  useEffect(() => {
    if (searchRef.current !== null) searchRef.current.focus();
    else buttons()[0]?.focus();
    const onPointerDown = (e: Event) => {
      const target = e.target;
      // A press on the composer's emoji button is its own toggle, not an outside press (the GIF picker's rule).
      if (target instanceof Element && target.closest('[data-emoji-toggle]') !== null) return;
      if (ref.current && target instanceof Node && !ref.current.contains(target)) onCloseRef.current();
    };
    document.addEventListener('pointerdown', onPointerDown);
    return () => document.removeEventListener('pointerdown', onPointerDown);
  }, []);

  const onKeyDown = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      onClose();
      return;
    }
    if (e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
    const list = buttons();
    const i = indexOfTarget(list, e.target);
    if (i < 0) return;
    const n = list.length;
    let next: number;
    switch (e.key) {
      case 'ArrowRight': next = i + 1 < n ? i + 1 : i; break;
      case 'ArrowLeft': next = i - 1 >= 0 ? i - 1 : i; break;
      case 'ArrowDown': next = i + COLUMNS < n ? i + COLUMNS : i; break;
      case 'ArrowUp': next = i - COLUMNS >= 0 ? i - COLUMNS : i; break;
      case 'Home': next = 0; break;
      case 'End': next = n - 1; break;
      default: return;
    }
    e.preventDefault();
    setFocus(next);
    list[next].focus();
  };

  const onFocus = (e: FocusEvent<HTMLDivElement>) => {
    const i = indexOfTarget(buttons(), e.target);
    if (i >= 0) setFocus(i);
  };

  return (
    <div ref={ref} id={id} role="dialog" aria-label={label} className="d-emoji-grid" data-search={search !== undefined ? 'true' : undefined}>
      {search !== undefined ? (
        <div className="d-emoji-grid__head">
          <input ref={searchRef} type="search" className="d-emoji-grid__search" aria-label={search.label} placeholder={search.placeholder} value={query}
            onChange={e => { setQuery(e.currentTarget.value); setFocus(0); }}
            onKeyDown={e => {
              if (e.key === 'Escape') { onKeyDown(e); return; }
              if (e.key === 'ArrowDown' || e.key === 'Enter') { e.preventDefault(); if (e.key === 'Enter' && items[0]) onPick(items[0].emoji); else buttons()[0]?.focus(); }
            }} />
        </div>
      ) : null}
      <div role="group" className="d-emoji-grid__grid" onKeyDown={onKeyDown} onFocus={onFocus}>
        {items.map((item, i) => (
          <button key={item.emoji} type="button" className="d-emoji-grid__emoji" aria-label={item.name} data-emoji={item.emoji}
            tabIndex={i === focus ? 0 : -1} onClick={() => onPick(item.emoji)}
            onMouseEnter={() => setShown(item)} onMouseLeave={() => setShown(null)}>
            <span aria-hidden="true">{item.emoji}</span>
          </button>
        ))}
      </div>
      {search !== undefined ? (
        <div className="d-emoji-grid__foot" aria-hidden="true">
          {named !== null ? <><span className="d-emoji-grid__foot-emoji">{named.emoji}</span>:{named.name.replace(/ /g, '_')}:</> : null}
        </div>
      ) : null}
    </div>
  );
}
