import './TextField.css';

export interface TextFieldProps {
  id: string;
  label: string;
  value: string;
  onChange(value: string): void;
  type?: 'text' | 'password';
  hint?: string;
  /** A second quiet line under the hint, in the same style (who can read the value, say); part of the description. */
  note?: string;
  error?: string;
  required?: boolean;
  disabled?: boolean;
  /** The value shows and selects but is not edited (a secret to copy); `onChange` is then never called. */
  readOnly?: boolean;
  autoComplete?: string;
  inputMode?: 'text' | 'numeric';
  maxLength?: number;
  spellCheck?: boolean;
  /** A several-line field: a `textarea` of `rows` rows (default 3) in place of the input. Line breaks are kept. */
  multiline?: boolean;
  rows?: number;
  /** Marks the input `data-autofocus`: a Dialog around it opens on this field. */
  initialFocus?: boolean;
  /** Called when the field gains focus (the sign-in step starts its passkey autofill then, wave B task 3). */
  onFocus?(): void;
  /** The input's placeholder (wave B task 3b: the prototype's auth fields, e.g. "at least 12 characters"). */
  placeholder?: string;
}

/**
 * A labelled single-line field (several lines with `multiline`). The hint, the note and the error are the input's
 * description, in that order; an error marks the field invalid and is
 * announced as an alert. Paste, copy and cut are never intercepted (WCAG 3.3.8).
 */
export function TextField({
  id, label, value, onChange, type, hint, note, error, required, disabled, readOnly, autoComplete, inputMode, maxLength, spellCheck, multiline, rows, initialFocus,
  onFocus, placeholder,
}: TextFieldProps) {
  const hintId = `${id}-hint`;
  const noteId = `${id}-note`;
  const errorId = `${id}-error`;
  const described = [hint ? hintId : null, note ? noteId : null, error ? errorId : null].filter((x): x is string => x !== null);
  const describedBy = described.length > 0 ? described.join(' ') : undefined;
  return (
    <div className="d-text-field" data-invalid={error ? 'true' : undefined}>
      <label className="d-text-field__label" htmlFor={id}>{label}</label>
      {multiline ? (
        <textarea id={id} className="d-text-field__input" value={value} rows={rows ?? 3}
          onChange={e => onChange(e.currentTarget.value)} aria-describedby={describedBy}
          aria-invalid={error ? true : undefined} required={required} disabled={disabled} readOnly={readOnly}
          autoComplete={autoComplete} maxLength={maxLength} spellCheck={spellCheck}
          data-autofocus={initialFocus ? '' : undefined} />
      ) : (
        <input id={id} className="d-text-field__input" type={type ?? 'text'} value={value}
          onChange={e => onChange(e.currentTarget.value)} aria-describedby={describedBy}
          aria-invalid={error ? true : undefined} required={required} disabled={disabled} readOnly={readOnly}
          autoComplete={autoComplete} inputMode={inputMode} maxLength={maxLength} spellCheck={spellCheck}
          data-autofocus={initialFocus ? '' : undefined} onFocus={onFocus} placeholder={placeholder} />
      )}
      {hint ? <p id={hintId} className="d-text-field__hint">{hint}</p> : null}
      {note ? <p id={noteId} className="d-text-field__hint">{note}</p> : null}
      {error ? <p id={errorId} className="d-text-field__error" role="alert">{error}</p> : null}
    </div>
  );
}
