import { useEffect, useRef, useState } from 'react';
import type { ClipboardEvent, SyntheticEvent } from 'react';
import './CodeBoxes.css';

export interface CodeBoxesProps {
  id: string;
  /** The input's accessible name; not drawn (the prototype's code step has no visible label). */
  label: string;
  /** The digits typed so far (0 to 6). */
  value: string;
  onChange(value: string): void;
  /** Called once each time the sixth digit lands, after COMPLETE_DELAY_MS (the prototype's 350 ms), unless the value
   *  changes or the boxes unmount first. The callback current at that moment is the one called. */
  onComplete?(value: string): void;
  invalid?: boolean;
  /** Ids of the hint or error that describe the code. */
  describedBy?: string;
  autoFocus?: boolean;
}

const LENGTH = 6;
/** The prototype's pause between the sixth digit and the step going on (`this.later(() => next(), 350)`). */
export const COMPLETE_DELAY_MS = 350;
const digitsOf = (text: string): string => text.replace(/\D/g, '').slice(0, LENGTH);

/**
 * The prototype's six-digit code (`codeBoxes`): one real input, transparent over six drawn boxes, so typing, paste,
 * the platform's one-time-code autofill and a screen reader all meet a single ordinary field. The boxes are
 * decoration (aria-hidden); the box at the input's caret takes the accent edge while the input has focus. Non-digits
 * are dropped, and a pasted or autofilled code ("123 456", "123456\n") fills all six.
 */
export function CodeBoxes({ id, label, value, onChange, onComplete, invalid, describedBy, autoFocus }: CodeBoxesProps) {
  // Where the input's caret is (selectionStart), so the lit box follows arrows and clicks, not only the length.
  const [selection, setSelection] = useState<number | null>(null);
  const latest = useRef(onComplete);
  useEffect(() => { latest.current = onComplete; });
  // The value that became six digits through this field, waiting out the delay; null when none is pending.
  const armed = useRef<string | null>(null);
  useEffect(() => {
    if (armed.current === null || armed.current !== value) return undefined;
    const done = value;
    const timer = setTimeout(() => { armed.current = null; latest.current?.(done); }, COMPLETE_DELAY_MS);
    return () => { clearTimeout(timer); };
  }, [value]);

  const update = (next: string) => {
    if (next === value) return;
    armed.current = next.length === LENGTH ? next : null;
    onChange(next);
  };
  const track = (e: SyntheticEvent<HTMLInputElement>) => { setSelection(e.currentTarget.selectionStart); };
  // maxLength would cut a grouped code ("123 456") to five digits on paste, so a paste is read whole: a full code
  // replaces the field, a part is inserted at the caret.
  const onPaste = (e: ClipboardEvent<HTMLInputElement>) => {
    const pasted = digitsOf(e.clipboardData.getData('text'));
    e.preventDefault();
    if (pasted.length === LENGTH) { setSelection(LENGTH); update(pasted); return; }
    const el = e.currentTarget;
    const start = el.selectionStart ?? value.length;
    const end = el.selectionEnd ?? value.length;
    const next = digitsOf(value.slice(0, start) + pasted + value.slice(end));
    setSelection(Math.min(start + pasted.length, next.length));
    update(next);
  };
  // The box the next digit lands in: the caret, never past the digits typed; with all six typed, the last one, so focus
  // always shows (WCAG 2.4.7).
  const caret = Math.min(selection ?? value.length, value.length, LENGTH - 1);
  return (
    <div className="d-code-boxes" data-invalid={invalid ? 'true' : undefined}>
      <label className="d-sr-only" htmlFor={id}>{label}</label>
      <div className="d-code-boxes__row" aria-hidden="true">
        {Array.from({ length: LENGTH }, (_, i) => (
          <span key={i} className="d-code-boxes__box"
            data-filled={i < value.length ? 'true' : undefined} data-caret={i === caret ? 'true' : undefined}>{value[i] ?? ''}</span>
        ))}
      </div>
      <input id={id} className="d-code-boxes__input" type="text" value={value}
        inputMode="numeric" autoComplete="one-time-code" maxLength={LENGTH} pattern="[0-9]*" spellCheck={false}
        aria-invalid={invalid ? true : undefined} aria-describedby={describedBy}
        // The step opens on its code (the prototype's focusRef).
        autoFocus={autoFocus}
        onChange={e => { const el = e.currentTarget; update(digitsOf(el.value)); setSelection(el.selectionStart); }}
        onSelect={track} onKeyUp={track} onClick={track} onPaste={onPaste} />
    </div>
  );
}
