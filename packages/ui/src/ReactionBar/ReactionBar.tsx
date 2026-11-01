import { useRef, useState, type FocusEvent, type KeyboardEvent } from 'react';
import { arrowIndex } from '../internal/arrow-select.ts';
import { Icon } from '../Icon/Icon.tsx';
import './ReactionBar.css';

export interface ReactionBarProps {
  label: string;
  tabbable: boolean;
  items: readonly { emoji: string; display?: string; count: number; mine: boolean; name: string }[];
  onToggle(emoji: string): void;
  onChipFocus?(emoji: string | null): void;
  addLabel: string;
  onAdd(anchor: HTMLElement): void;
}

/** The index of the event's target among the buttons, or -1. */
function indexOfTarget(list: readonly HTMLButtonElement[], target: EventTarget): number {
  return target instanceof HTMLButtonElement ? list.indexOf(target) : -1;
}

/**
 * A message's reactions: one pressed-state chip per emoji, named by `name` (`{name}, {count}`), then the add
 * button. One roving tab stop across the chips and the add button, as the toolbar; none when not `tabbable`. A
 * pressed chip carries `aria-pressed` and the prototype's accent chip (README §6.5). The add button (a dashed 26 × 22
 * smile) shows while the row is hovered or holds focus. Nothing renders
 * while there is no reaction (the toolbar's `react` opens the grid then).
 */
export function ReactionBar({ label, tabbable, items, onToggle, onChipFocus, addLabel, onAdd }: ReactionBarProps) {
  const ref = useRef<HTMLDivElement>(null);
  const [stop, setStop] = useState(0);
  if (items.length === 0) return null;
  const buttons = (): HTMLButtonElement[] => Array.from(ref.current?.querySelectorAll('button') ?? []);
  const current = stop <= items.length ? stop : 0;

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const list = buttons();
    const from = indexOfTarget(list, e.target);
    if (from < 0) return;
    const next = arrowIndex(e, from, list.length, 'horizontal');
    if (next === null) return;
    e.preventDefault();
    setStop(next);
    list[next].focus();
  };

  const onFocus = (e: FocusEvent<HTMLDivElement>) => {
    const i = indexOfTarget(buttons(), e.target);
    if (i >= 0) {
      setStop(i);
      onChipFocus?.(i < items.length ? items[i].emoji : null);
    }
  };

  const tab = (i: number) => (tabbable && i === current ? 0 : -1);
  return (
    <div ref={ref} role="group" aria-label={label} className="d-reaction-bar" onKeyDown={onKeyDown} onFocus={onFocus}>
      {items.map((item, i) => (
        <button key={item.emoji} type="button" className="d-reaction-bar__chip" aria-pressed={item.mine} aria-label={item.name}
          data-emoji={item.emoji} tabIndex={tab(i)} onClick={() => onToggle(item.emoji)}>
          <span aria-hidden="true">{item.display ?? item.emoji}</span><span className="d-reaction-bar__count" aria-hidden="true">{item.count}</span>
        </button>
      ))}
      <button type="button" className="d-reaction-bar__add" aria-label={addLabel} tabIndex={tab(items.length)}
        onClick={e => onAdd(e.currentTarget)}>
        <Icon name="smile-plus" size={12} />
      </button>
    </div>
  );
}
