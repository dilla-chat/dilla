import { useId, useRef, type KeyboardEvent } from 'react';
import { Icon } from '../Icon/Icon.tsx';
import type { IconName } from '../Icon/Icon.tsx';
import './ChoiceCards.css';

export interface ChoiceCardOption {
  id: string;
  title: string;
  hint: string;
  disabled?: boolean;
  reason?: string;
  /** Wave B task 3b: the prototype's 32px tile at the card's left (smartphone, fingerprint). */
  icon?: IconName;
}

export interface ChoiceCardsProps {
  name: string;
  label: string;
  options: readonly ChoiceCardOption[];
  value: string;
  onChange(id: string): void;
}

/**
 * Cards that are native radios in a radiogroup. One tab stop (the chosen card, else the first enabled one);
 * the arrows move and select, wrap, and skip disabled cards. A disabled card stays visible, and its reason is
 * its description, so it is heard with the card rather than only seen.
 */
export function ChoiceCards({ name, label, options, value, onChange }: ChoiceCardsProps) {
  const uid = useId();
  const labelId = `${uid}-label`;
  const inputs = useRef(new Map<string, HTMLInputElement>());

  const enabled = options.filter(o => !o.disabled);
  const stop = enabled.find(o => o.id === value)?.id ?? enabled[0]?.id;

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    let step: number;
    switch (event.key) {
      case 'ArrowDown': case 'ArrowRight': step = 1; break;
      case 'ArrowUp': case 'ArrowLeft': step = -1; break;
      default: return;
    }
    const from = enabled.findIndex(o => inputs.current.get(o.id) === event.target);
    if (from < 0) return;
    event.preventDefault();
    const next = enabled[(from + step + enabled.length) % enabled.length]!;
    inputs.current.get(next.id)?.focus();
    if (next.id !== value) onChange(next.id);
  };

  return (
    <div className="d-choice-cards" role="radiogroup" aria-labelledby={labelId} onKeyDown={onKeyDown}>
      {/* Wave B task 3b: the prototype draws no label over the cards; it names the group for assistive technology. */}
      <p id={labelId} className="d-choice-cards__label d-sr-only">{label}</p>
      {options.map(o => {
        const titleId = `${uid}-${o.id}-title`;
        const hintId = `${uid}-${o.id}-hint`;
        const reasonId = `${uid}-${o.id}-reason`;
        const reason = o.disabled && o.reason ? o.reason : undefined;
        return (
          <label key={o.id} className="d-choice-cards__card" data-checked={o.id === value ? 'true' : undefined}
            data-disabled={o.disabled ? 'true' : undefined}>
            {o.icon ? <span className="d-choice-cards__tile" aria-hidden="true"><Icon name={o.icon} size={16} /></span> : null}
            <span className="d-choice-cards__body">
              <span id={titleId} className="d-choice-cards__title">{o.title}</span>
              <span id={hintId} className="d-choice-cards__hint">{o.hint}</span>
              {reason ? <span id={reasonId} className="d-choice-cards__reason">{reason}</span> : null}
            </span>
            {/* The native radio covers the whole card, transparent: the card is its target; the dot at the right is drawn. */}
            <input type="radio" className="d-choice-cards__radio" name={name} value={o.id} checked={o.id === value}
              disabled={o.disabled} tabIndex={o.id === stop ? 0 : -1}
              aria-labelledby={titleId} aria-describedby={reason ? `${hintId} ${reasonId}` : hintId}
              ref={el => { if (el) inputs.current.set(o.id, el); else inputs.current.delete(o.id); }}
              onChange={() => { if (!o.disabled) onChange(o.id); }} />
            <span className="d-choice-cards__dot" aria-hidden="true" />
          </label>
        );
      })}
    </div>
  );
}
