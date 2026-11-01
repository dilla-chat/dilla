import type { ReactNode } from 'react';
import { Button } from '../Button/Button.tsx';
import { Icon } from '../Icon/Icon.tsx';
import './PhoneHeader.css';

export interface PhoneHeaderProps {
  title: ReactNode;
  /** The channel topic after the title (Signal §5.10, the prototype's phone header): 12px, cut with an ellipsis. */
  topic?: string;
  backLabel?: string;
  onBack?(): void;
  actions?: ReactNode;
  /** A small cue beside the title (the readable glyph), outside the heading so its own name is kept. */
  marker?: ReactNode;
  /** A call is up while the call bar steps aside (typing): an 8px accent dot after the title, an image named by this label. */
  dot?: { label: string; onOpen: () => void } | undefined;
}

/**
 * The phone's 52px header over a pane (Signal §5.10): a 44px back control (when `onBack` is given), the title in a level-two
 * heading cut to one line, and the caller's actions at the end. Every control is at least 44 × 44 CSS px. It pads its top by the device's
 * safe area, so it clears a notch.
 */
export function PhoneHeader({ title, topic, backLabel, onBack, actions, marker, dot }: PhoneHeaderProps) {
  return (
    <header className="d-phone-header">
      {onBack !== undefined ? (
        <Button variant="ghost" className="d-phone-header__back" aria-label={backLabel} onClick={onBack}><Icon name="chevron-left" size={20} /></Button>
      ) : null}
      <h2 className="d-phone-header__title">{typeof title === 'string' ? <span className="d-phone-header__text">{title}</span> : title}</h2>
      {dot !== undefined ? <button type="button" className="d-phone-header__dot" aria-label={dot.label} onClick={dot.onOpen} onMouseDown={e => e.preventDefault()}><span aria-hidden="true" /></button> : null}
      {marker !== undefined ? <span className="d-phone-header__marker">{marker}</span> : null}
      {topic !== undefined && topic !== '' ? <p className="d-phone-header__topic">{topic}</p> : null}
      {actions !== undefined ? <div className="d-phone-header__actions">{actions}</div> : null}
    </header>
  );
}
