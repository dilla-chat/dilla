import { useId, useLayoutEffect, useRef } from 'react';
import { Button } from '../Button/Button.tsx';
import './RingBanner.css';
import { Avatar } from '../Avatar/Avatar.tsx';
import { StateDot } from '../StateDot/StateDot.tsx';

export interface RingBannerProps {
  /** `text` is the filled `call.ring.incoming` or, while this device is in voice, `call.ring.busy`. */
  rings: readonly { callId: string; text: string; busy: boolean; /** The caller, for the ringing avatar (a dot without it). */ name?: string }[];
  labels: { region: string; answer: string; decline: string };
  onAnswer(callId: string): void;
  onDecline(callId: string): void;
}

function Ring({ ring, labels, onAnswer, onDecline }: { ring: RingBannerProps['rings'][number] } & Omit<RingBannerProps, 'rings'>) {
  const textId = useId();
  const ref = useRef<HTMLLIElement>(null);
  // WB a11y m3: a ring that goes while focus is on its buttons hands focus to the next ring's answer. Its cleanup runs
  // before React removes the row, so focus never falls to the page; the banner itself going is the shell's to handle.
  useLayoutEffect(() => {
    const li = ref.current;
    return () => {
      if (li === null || !li.contains(document.activeElement)) return;
      const next = Array.from(li.closest('ul')?.querySelectorAll<HTMLButtonElement>('.d-ring-banner__answer') ?? []).find(b => !li.contains(b));
      next?.focus();
    };
  }, []);
  return (
    <li ref={ref} className="d-ring-banner__ring" data-ring={ring.busy ? 'busy' : 'incoming'}>
      <span className="d-ring-banner__glyph" aria-hidden="true">
        {ring.name !== undefined ? <Avatar name={ring.name} size="row" /> : <StateDot tone={ring.busy ? 'off' : 'ok'} size="md" />}
      </span>
      <p id={textId} role="alert" className="d-ring-banner__text">{ring.text}</p>
      <span className="d-ring-banner__actions">
        <Button variant="accent" className="d-ring-banner__answer" aria-describedby={textId} onClick={() => onAnswer(ring.callId)}>{labels.answer}</Button>
        <Button variant="default" className="d-ring-banner__decline" aria-describedby={textId} onClick={() => onDecline(ring.callId)}>{labels.decline}</Button>
      </span>
    </li>
  );
}

/**
 * Signal (README §13, the DM call not drawn in the prototype): each ring on --surface-2 with the --ok edge, the caller's 32px
 * avatar ringing (the d-ring wave, 1 → 1.5 and out) beside the text, then Answer (--ok) and Decline.
 *
 * Incoming calls, at the top of the main landmark while a ring lasts (L-UI-70): a section named `call.ring.region`
 * with one `role="alert"` per ring. A ring's alert node is keyed by its call id and inserted once, so it is
 * announced once. It never takes focus; its answer and decline buttons are described by the ring's text, so two
 * rings' buttons can be told apart. Nothing renders without rings.
 */
export function RingBanner({ rings, labels, onAnswer, onDecline }: RingBannerProps) {
  if (rings.length === 0) return null;
  return (
    <section className="d-ring-banner" aria-label={labels.region}>
      <ul role="list" className="d-ring-banner__list">
        {rings.map(r => <Ring key={r.callId} ring={r} labels={labels} onAnswer={onAnswer} onDecline={onDecline} />)}
      </ul>
    </section>
  );
}
