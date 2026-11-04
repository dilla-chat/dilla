import { useEffect, useId, useRef, useState, type ChangeEvent, type FocusEvent, type KeyboardEvent } from 'react';
import './GifPicker.css';

export interface GifPickerItem {
  id: string;
  title: string;
  width: number;
  height: number;
  /** The preview as a page-minted URL, or null while it loads. It is never a remote URL. */
  src: string | null;
}

export interface GifPickerStatus {
  kind: 'loading' | 'empty' | 'error' | 'busy';
  text: string;
}

export interface GifPickerProps {
  /** The dialog's id, for the opener's `aria-controls`. */
  id?: string;
  label: string;
  searchLabel: string;
  searchPlaceholder: string;
  query: string;
  onQuery(query: string): void;
  items: readonly GifPickerItem[];
  /** Said politely in the one status line; null says nothing. */
  status: GifPickerStatus | null;
  /** The attribution Giphy asks for. */
  poweredBy: string;
  /** The accessible name of a result that has no title. */
  untitled: string;
  moreLabel: string;
  hasMore: boolean;
  onMore(): void;
  /** The result being downloaded to be attached; a second pick is ignored meanwhile. */
  pickingId: string | null;
  onPick(id: string): void;
  /** Escape asks to close and return focus to the opener; a pointer down outside closes and leaves focus where it landed. */
  onClose(reason: 'escape' | 'outside'): void;
}

/** The columns when the grid cannot say (no layout): three on a desktop, two on a phone (the CSS decides). */
const COLUMNS = 3;
/** The grid's row unit, row gap and the gap between tiles, in CSS px: a tile spans its height plus one gap in 1 px rows. */
const TILE_GAP = 4;
/** The width a column is assumed to have before the grid is measured (and where there is no layout). */
const FALLBACK_COLUMN_PX = 128;
/** A phone result is at least this tall (the 44 px target rule). */
const PHONE_MIN_TILE_PX = 44;

/** The tile's own proportions, as a CSS aspect-ratio; 1 / 1 while the instance did not say. */
export const tileRatio = (item: Pick<GifPickerItem, 'width' | 'height'>): string =>
  item.width > 0 && item.height > 0 ? `${String(item.width)} / ${String(item.height)}` : '1 / 1';

/** How many 1 px rows a tile spans in a column `columnPx` wide: its height from its own proportions plus the gap below it. */
export function tileSpan(item: Pick<GifPickerItem, 'width' | 'height'>, columnPx: number, minPx = 0): number {
  const ratio = item.width > 0 && item.height > 0 ? item.height / item.width : 1;
  return Math.max(1, Math.ceil(Math.max(columnPx * ratio, minPx) + TILE_GAP));
}

/**
 * A non-modal popover dialog above the composer: a search box (focused on mount), a masonry grid of preview buttons in
 * three columns (two on a phone) in reading order, each tile as tall as its own GIF's proportions make it, reserved
 * before the preview arrives and never cropped, "more" when there is more, and the attribution. The grid is one tab stop that follows focus;
 * arrows move left and right in reading order and up and down within a column, without wrapping, ArrowDown from the search box enters the grid, ArrowUp from the
 * first row goes back to it; Home and End go to the ends; Enter, Space or a click picks. Escape closes and goes no
 * further; a pointer down outside closes it, except on the opener (`[data-gif-toggle]`), which toggles it itself.
 * The previews are decorative (`alt=""`): each button is named by the GIF's title. Returning focus is the caller's.
 */
export function GifPicker({
  id, label, searchLabel, searchPlaceholder, query, onQuery, items, status, poweredBy, untitled, moreLabel, hasMore, onMore,
  pickingId, onPick, onClose,
}: GifPickerProps) {
  const ref = useRef<HTMLDivElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const [focus, setFocus] = useState(0);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  const statusId = useId();
  const gridRef = useRef<HTMLDivElement>(null);
  // The column width and count, measured from the grid itself, so each tile reserves the height its own proportions give it.
  const [metrics, setMetrics] = useState({ column: FALLBACK_COLUMN_PX, columns: COLUMNS, minTile: 0 });

  const cells = (): HTMLButtonElement[] => Array.from(ref.current?.querySelectorAll<HTMLButtonElement>('button[data-gif-id]') ?? []);

  useEffect(() => {
    const grid = gridRef.current;
    if (grid === null) return;
    const measure = () => {
      const tracks = getComputedStyle(grid).gridTemplateColumns.split(' ').filter(x => x !== '').length;
      const columns = tracks > 0 ? tracks : COLUMNS;
      const width = grid.clientWidth;
      const column = width > 0 ? (width - TILE_GAP * (columns - 1)) / columns : FALLBACK_COLUMN_PX;
      const minTile = grid.closest('[data-layout="phone"]') !== null ? PHONE_MIN_TILE_PX : 0;
      setMetrics(cur => cur.columns === columns && Math.abs(cur.column - column) < 0.5 && cur.minTile === minTile ? cur : { column, columns, minTile });
    };
    measure();
    if (typeof ResizeObserver === 'undefined') return;
    const watcher = new ResizeObserver(measure);
    watcher.observe(grid);
    return () => { watcher.disconnect(); };
  }, []);

  useEffect(() => {
    searchRef.current?.focus();
    const onPointerDown = (e: Event) => {
      const target = e.target;
      if (!(target instanceof Node) || ref.current === null || ref.current.contains(target)) return;
      if (target instanceof Element && target.closest('[data-gif-toggle]') !== null) return;
      onCloseRef.current('outside');
    };
    document.addEventListener('pointerdown', onPointerDown);
    return () => document.removeEventListener('pointerdown', onPointerDown);
  }, []);

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      onClose('escape');
      return;
    }
    if (e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
    const list = cells();
    if (e.target === searchRef.current) {
      if (e.key === 'ArrowDown' && list.length > 0) {
        e.preventDefault();
        setFocus(0);
        list[0].focus();
      }
      return;
    }
    const i = e.target instanceof HTMLButtonElement ? list.indexOf(e.target) : -1;
    if (i < 0) return;
    const n = list.length;
    const cols = metrics.columns;
    // A masonry column has no fixed row: Up and Down go to the tile above or below in the same column when the grid has a
    // layout, and by a row of columns when it has none. Left and Right are reading order either way.
    const nearest = (dir: 1 | -1): number | null => {
      const here = list[i].getBoundingClientRect();
      if (here.width === 0) return i + dir * cols >= 0 && i + dir * cols < n ? i + dir * cols : null;
      let best: number | null = null;
      let bestGap = Infinity;
      list.forEach((el, j) => {
        if (j === i) return;
        const r = el.getBoundingClientRect();
        const mid = (r.left + r.right) / 2;
        if (mid < here.left || mid > here.right) return;
        const gap = dir === 1 ? r.top - here.bottom : here.top - r.bottom;
        if (gap >= -1 && gap < bestGap) { best = j; bestGap = gap; }
      });
      return best;
    };
    // Nothing below in this column: the nearest tile below in a column to the right, else the "more" button.
    const aside = (): number | null => {
      const here = list[i].getBoundingClientRect();
      if (here.width === 0) return null;
      let best: number | null = null;
      let bestGap = Infinity;
      list.forEach((el, j) => {
        const r = el.getBoundingClientRect();
        const gap = r.top - here.bottom;
        if ((r.left + r.right) / 2 > here.right && gap >= -1 && gap < bestGap) { best = j; bestGap = gap; }
      });
      return best;
    };
    let next: number;
    switch (e.key) {
      case 'ArrowRight': next = i + 1 < n ? i + 1 : i; break;
      case 'ArrowLeft': next = i - 1 >= 0 ? i - 1 : i; break;
      case 'ArrowDown': {
        const down = nearest(1) ?? aside();
        if (down === null) {
          const more = ref.current?.querySelector<HTMLButtonElement>('.d-gif-picker__more');
          e.preventDefault();
          if (more) more.focus();
          return;
        }
        next = down;
        break;
      }
      case 'ArrowUp': {
        const up = nearest(-1);
        if (up === null) {
          e.preventDefault();
          searchRef.current?.focus();
          return;
        }
        next = up;
        break;
      }
      case 'Home': next = 0; break;
      case 'End': next = n - 1; break;
      default: return;
    }
    e.preventDefault();
    setFocus(next);
    list[next].focus();
  };

  const onFocus = (e: FocusEvent<HTMLDivElement>) => {
    const i = e.target instanceof HTMLButtonElement ? cells().indexOf(e.target) : -1;
    if (i >= 0) setFocus(i);
  };

  const tab = Math.min(focus, Math.max(items.length - 1, 0));
  return (
    <div ref={ref} id={id} role="dialog" aria-label={label} className="d-gif-picker" onKeyDown={onKeyDown}>
      <div className="d-gif-picker__field"><input ref={searchRef} type="search" className="d-gif-picker__search" aria-label={searchLabel} placeholder={searchPlaceholder}
        value={query} autoComplete="off" spellCheck={false} aria-describedby={statusId}
        onChange={(e: ChangeEvent<HTMLInputElement>) => onQuery(e.currentTarget.value)} /><span className="d-gif-picker__powered">{poweredBy}</span></div>
      <div ref={gridRef} role="group" aria-label={label} className="d-gif-picker__grid" onFocus={onFocus}>
        {items.map((item, i) => (
          <button key={item.id} type="button" className="d-gif-picker__cell" data-gif-id={item.id}
            aria-label={item.title === '' ? untitled : item.title} aria-busy={pickingId === item.id ? true : undefined}
            tabIndex={i === tab ? 0 : -1}
            style={{ aspectRatio: tileRatio(item), gridRowEnd: `span ${String(tileSpan(item, metrics.column, metrics.minTile))}` }}
            onClick={() => { if (pickingId === null) onPick(item.id); }}>
            {item.src === null
              ? <span className="d-gif-picker__loading" data-loading="" aria-hidden="true" />
              : <img className="d-gif-picker__img" src={item.src} alt="" draggable={false} />}
          </button>
        ))}
      </div>
      <p id={statusId} className="d-gif-picker__status" role="status" data-kind={status?.kind}>{status?.text ?? ''}</p>
      <div className="d-gif-picker__foot">
        {hasMore ? <button type="button" className="d-gif-picker__more" onClick={onMore}>{moreLabel}</button> : <span />}
      </div>
    </div>
  );
}
