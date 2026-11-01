import { useId, useRef, type CSSProperties, type KeyboardEvent } from 'react';
import { arrowIndex } from '../internal/arrow-select.ts';
import { hex } from '../internal/hex.ts';
import './ColorSwatches.css';
import { Icon } from '../Icon/Icon.tsx';

export interface ColorSwatchesProps {
  label: string;
  hint?: string;
  /** The chosen colour as 0xRRGGBB, or 0 for none. */
  value: number;
  options: readonly { value: number; name: string }[];
  /** The "no colour" choice drawn first; without it there is none (the Roles editor's six colours, Task 10c). */
  noneLabel?: string;
  onChange(v: number): void;
}


/**
 * A single choice of colour: the ARIA radio group with selection following focus. The "none" option (value 0)
 * comes first. Each swatch is named by its colour name, never by its colour; the selected one also shows a check
 * mark. One tab stop, on the checked swatch (the first when none matches); the arrows move and select, wrapping.
 */
export function ColorSwatches({ label, hint, value, options, noneLabel, onChange }: ColorSwatchesProps) {
  const uid = useId();
  const refs = useRef<(HTMLButtonElement | null)[]>([]);
  const all = noneLabel === undefined ? [...options] : [{ value: 0, name: noneLabel }, ...options];
  const checked = all.findIndex(o => o.value === value);
  const stop = checked >= 0 ? checked : 0;

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const from = refs.current.findIndex(el => el !== null && el === event.target);
    if (from < 0) return;
    const next = arrowIndex(event, from, all.length, 'both');
    if (next === null) return;
    event.preventDefault();
    refs.current[next]?.focus();
    if (all[next].value !== value) onChange(all[next].value);
  };

  return (
    <div className="d-color-swatches">
      <span id={`${uid}-label`} className="d-color-swatches__label">{label}</span>
      {hint ? <span id={`${uid}-hint`} className="d-color-swatches__hint">{hint}</span> : null}
      <div role="radiogroup" className="d-color-swatches__group" aria-labelledby={`${uid}-label`}
        aria-describedby={hint ? `${uid}-hint` : undefined} onKeyDown={onKeyDown}>
        {all.map((o, i) => {
          const none = o.value === 0;
          const style = none ? undefined : ({ '--d-swatch': hex(o.value) } as CSSProperties);
          return (
            <button key={i} ref={el => { refs.current[i] = el; }} type="button" role="radio" aria-label={o.name} title={o.name}
              className={`d-color-swatches__swatch${none ? ' d-color-swatches__swatch--none' : ''}`} style={style}
              aria-checked={i === checked} tabIndex={i === stop ? 0 : -1} onClick={() => { if (o.value !== value) onChange(o.value); }}>
              {i === checked ? <span className="d-color-swatches__check" aria-hidden="true"><Icon name="check" size={14} /></span> : null}
            </button>
          );
        })}
      </div>
    </div>
  );
}
