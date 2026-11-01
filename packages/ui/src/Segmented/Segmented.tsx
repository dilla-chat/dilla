import { useRef, type KeyboardEvent } from 'react';
import { arrowIndex } from '../internal/arrow-select.ts';
import './Segmented.css';

export interface SegmentedProps {
  id: string;
  label: string;
  options: readonly { value: string; label: string }[];
  value: string;
  onChange(value: string): void;
  /** Shown but not changeable (a setting this account may read but not change). */
  disabled?: boolean;
}

/**
 * A single choice among a few options: the ARIA radio group with selection following focus. One
 * tab stop, on the checked option (the first when none is checked); the arrows move and select,
 * wrapping, and Home and End go to the ends. The visible label is the row's; the group carries the
 * same text as a visually hidden label.
 */
export function Segmented({ id, label, options, value, onChange, disabled }: SegmentedProps) {
  const refs = useRef<(HTMLButtonElement | null)[]>([]);
  const checkedIndex = options.findIndex(o => o.value === value);
  const stop = checkedIndex >= 0 ? checkedIndex : 0;

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const target: EventTarget = event.target;
    const from = refs.current.findIndex(el => el !== null && el === target);
    if (from < 0) return;
    const next = arrowIndex(event, from, options.length, 'both');
    if (next === null) return;
    event.preventDefault();
    if (disabled) return;
    refs.current[next]?.focus();
    const option = options[next];
    if (option.value !== value) onChange(option.value);
  };

  return (
    <div className="d-segmented">
      <span id={`${id}-label`} className="d-sr-only">{label}</span>
      <div id={id} role="radiogroup" aria-labelledby={`${id}-label`} className="d-segmented__group" onKeyDown={onKeyDown}>
        {options.map((o, i) => (
          <button key={o.value} ref={el => { refs.current[i] = el; }} type="button" role="radio" className="d-segmented__option"
            aria-checked={o.value === value} aria-disabled={disabled || undefined} tabIndex={i === stop ? 0 : -1}
            onClick={() => { if (!disabled) onChange(o.value); }}>{o.label}</button>
        ))}
      </div>
    </div>
  );
}
