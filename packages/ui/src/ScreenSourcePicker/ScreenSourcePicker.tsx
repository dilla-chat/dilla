import { useEffect, useId, useRef, useState, type KeyboardEvent } from 'react';
import { Button } from '../Button/Button.tsx';
import { Dialog } from '../Dialog/Dialog.tsx';
import { Icon } from '../Icon/Icon.tsx';
import './ScreenSourcePicker.css';

/** One screen or window offered for sharing. `id` is opaque; the images are URLs (data: in the desktop picker window). */
export interface ScreenSourceTile { id: string; name: string; kind: 'screen' | 'window'; thumbnail: string | null; icon: string | null }

export interface ScreenSourcePickerProps {
  open: boolean;
  /** Every source, both kinds; the caller replaces it on each refresh (the desktop sends one every 2 s). */
  sources: readonly ScreenSourceTile[];
  labels: { title: string; screens: string; windows: string; share: string; cancel: string; empty: string };
  /** Share, Enter on a tile, or a double-click: the chosen tile's id. */
  onShare(id: string): void;
  /** Cancel or Escape: nothing is shared. */
  onCancel(): void;
  /** The tab shown first (Screens by default). */
  defaultTab?: 'screen' | 'window';
}

type Kind = ScreenSourceTile['kind'];
const KINDS: readonly Kind[] = ['screen', 'window'];

/**
 * The desktop screen-share picker (an approved addition; the prototype has none): a modal "Share your screen" with Screens and
 * Windows tabs over a grid of thumbnails, the name under each and, for a window, its app icon. A tile is selected by click or the
 * arrow keys (selection follows focus); Share, Enter or a double-click shares it; Cancel or Escape shares nothing.
 */
export function ScreenSourcePicker({ open, sources, labels, onShare, onCancel, defaultTab = 'screen' }: ScreenSourcePickerProps) {
  const id = useId();
  const [kind, setKind] = useState<Kind>(defaultTab);
  const [chosen, setChosen] = useState<string | null>(null);
  const tabs = useRef(new Map<Kind, HTMLButtonElement>());
  const grid = useRef<HTMLDivElement>(null);
  const shown = sources.filter(s => s.kind === kind);
  // A refresh can drop the chosen source (its window closed): the selection goes with it.
  const selected = chosen !== null && shown.some(s => s.id === chosen) ? chosen : null;
  useEffect(() => { if (!open) { setKind(defaultTab); setChosen(null); } }, [open, defaultTab]);

  const pickTab = (next: Kind, focus: boolean) => {
    if (next !== kind) { setKind(next); setChosen(null); }
    if (focus) tabs.current.get(next)?.focus();
  };
  const onTabKey = (e: KeyboardEvent<HTMLButtonElement>) => {
    const at = KINDS.indexOf(kind);
    const next = e.key === 'ArrowRight' ? KINDS[(at + 1) % KINDS.length] : e.key === 'ArrowLeft' ? KINDS[(at + KINDS.length - 1) % KINDS.length]
      : e.key === 'Home' ? KINDS[0] : e.key === 'End' ? KINDS[KINDS.length - 1] : null;
    if (next === null) return;
    e.preventDefault();
    pickTab(next, true);
  };
  const focusTile = (tileId: string) => {
    setChosen(tileId);
    grid.current?.querySelector<HTMLElement>(`[data-tile="${CSS.escape(tileId)}"]`)?.focus();
  };
  /** Tiles per row, from the laid-out grid; 1 when there is no layout (all tiles on one line). */
  const columns = (): number => {
    const tiles = Array.from(grid.current?.querySelectorAll<HTMLElement>('[role="option"]') ?? []);
    const top = tiles[0]?.offsetTop;
    const row = tiles.filter(t => t.offsetTop === top).length;
    return row > 0 && row < tiles.length ? row : 1;
  };
  const onTileKey = (e: KeyboardEvent<HTMLDivElement>, index: number) => {
    if (e.key === 'Enter') { e.preventDefault(); onShare(shown[index].id); return; }
    if (e.key === ' ') { e.preventDefault(); setChosen(shown[index].id); return; }
    const step = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : e.key === 'ArrowDown' ? columns() : e.key === 'ArrowUp' ? -columns() : 0;
    const target = e.key === 'Home' ? 0 : e.key === 'End' ? shown.length - 1 : step !== 0 ? Math.max(0, Math.min(shown.length - 1, index + step)) : null;
    if (target === null) return;
    e.preventDefault();
    focusTile(shown[target].id);
  };
  // The one tile in the tab order: the selected one, else the first.
  const tabStop = selected ?? shown[0]?.id;
  const tabLabel = (k: Kind) => k === 'screen' ? labels.screens : labels.windows;

  return (
    <Dialog open={open} title={labels.title} onClose={onCancel} closeLabel={labels.cancel}
      footer={<Button variant="accent" disabled={selected === null} onClick={() => { if (selected !== null) onShare(selected); }}>{labels.share}</Button>}>
      <div className="d-screen-picker">
        <div className="d-screen-picker__tabs" role="tablist" aria-label={labels.title}>
          {KINDS.map(k => (
            <button key={k} type="button" role="tab" id={`${id}-tab-${k}`} aria-controls={`${id}-panel`} aria-selected={k === kind}
              tabIndex={k === kind ? 0 : -1} className="d-screen-picker__tab" data-autofocus={k === kind ? '' : undefined}
              ref={el => { if (el) tabs.current.set(k, el); else tabs.current.delete(k); }}
              onClick={() => pickTab(k, false)} onKeyDown={onTabKey}>{tabLabel(k)}</button>
          ))}
        </div>
        <div role="tabpanel" id={`${id}-panel`} aria-labelledby={`${id}-tab-${kind}`} className="d-screen-picker__panel">
          {shown.length === 0 ? <p className="d-screen-picker__empty">{labels.empty}</p> : (
            <div ref={grid} role="listbox" aria-labelledby={`${id}-tab-${kind}`} className="d-screen-picker__grid">
              {shown.map((s, index) => (
                <div key={s.id} role="option" data-tile={s.id} aria-selected={s.id === selected} tabIndex={s.id === tabStop ? 0 : -1}
                  className="d-screen-picker__tile" onClick={() => setChosen(s.id)} onDoubleClick={() => onShare(s.id)} onKeyDown={e => onTileKey(e, index)}>
                  <span className="d-screen-picker__thumb" aria-hidden="true">
                    {s.thumbnail === null ? <Icon name={s.kind === 'window' ? 'app-window' : 'monitor'} /> : <img src={s.thumbnail} alt="" draggable={false} />}
                  </span>
                  <span className="d-screen-picker__name">
                    {s.kind === 'window' && s.icon !== null ? <img className="d-screen-picker__icon" src={s.icon} alt="" draggable={false} /> : null}
                    <span className="d-screen-picker__label">{s.name}</span>
                  </span>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </Dialog>
  );
}
