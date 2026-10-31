import { useId, useLayoutEffect, useRef, type MouseEvent } from 'react';
import { Icon } from '../Icon/Icon.tsx';
import { Avatar } from '../Avatar/Avatar.tsx';
import { graphemeInitials } from '../initials.ts';
import './PollCard.css';
export interface PollCardOption { text: string; pct: number; mine: boolean; winner: boolean; voters: readonly { name: string }[] }
export interface PollCardProps { question: string; closed: boolean; multi: boolean; meta: string; options: readonly PollCardOption[];
  canVote: boolean; canClose: boolean; busy?: boolean; pendingAnswer?: number; labels: { closed: string; close: string };
  onVote(index: number): void; onClose(): void }
export function PollCard({ question, closed, multi, meta, options, canVote, canClose, busy, pendingAnswer, labels, onVote, onClose }: PollCardProps) {
  const heading = useId(); const headingRef = useRef<HTMLHeadingElement>(null);
  const wasClosed = useRef(closed); const focusedInside = useRef(false); const closeRequested = useRef(false);
  // The prototype's div/span controls keep pointer focus where it was. Keyboard users still focus the native buttons.
  const keepMouseFocus = (event: MouseEvent<HTMLButtonElement>) => {
    if (event.button !== 0) return;
    event.preventDefault();
    focusedInside.current = headingRef.current?.closest('.d-poll-card')?.contains(document.activeElement) === true;
  };
  useLayoutEffect(() => {
    const active = document.activeElement;
    const card = headingRef.current?.closest('.d-poll-card');
    if (closed && !wasClosed.current && (focusedInside.current || closeRequested.current)
      && (active === document.body || active !== null && card?.contains(active))) headingRef.current?.focus();
    wasClosed.current = closed;
    if (closed) closeRequested.current = false;
  }, [closed]);
  return <section className="d-poll-card" aria-labelledby={heading} data-multi={multi} data-closed={closed}
    onFocusCapture={() => { focusedInside.current = true; }}
    onBlurCapture={event => { if (event.relatedTarget === null) return; focusedInside.current = event.relatedTarget instanceof Node && event.currentTarget.contains(event.relatedTarget); if (event.relatedTarget !== null && !focusedInside.current) closeRequested.current = false; }}>
    <div className="d-poll-card__head"><Icon name="vote" size={14} /><h3 ref={headingRef} id={heading} tabIndex={-1}>{question}</h3>{closed && <span className="d-poll-card__tag">{labels.closed}</span>}</div>
    {/* Answers retain the prototype's row tap, including closed results; vote guards remain independent. */}
    {options.map((option, index) => <button type="button" data-message-row-tap="true" key={index} className="d-poll-card__option" data-mine={option.mine} data-winner={closed && option.winner && option.pct > 0}
      aria-pressed={option.mine} disabled={!closed && !canVote && !busy} aria-disabled={closed || !!busy || !canVote || pendingAnswer !== undefined && pendingAnswer !== index}
      tabIndex={closed ? -1 : undefined} onMouseDown={keepMouseFocus}
      onClick={() => { if (!closed && canVote && !busy && (pendingAnswer === undefined || pendingAnswer === index)) onVote(index); }}>
      <span className="d-poll-card__fill" style={{ width: `${option.pct}%` }} />
      <span className="d-poll-card__row"><span className="d-poll-card__check">{option.mine && <Icon name="check" size={10} stroke={3} />}</span>
        <span className="d-poll-card__text">{option.text}</span><span className="d-poll-card__voters">{option.voters.slice(0, 5).map((voter, i) => <span title={voter.name} key={i}><Avatar name={voter.name} initials={graphemeInitials(voter.name)} size="mini" /></span>)}</span>
        <span className="d-poll-card__pct">{option.pct}%</span></span>
    </button>)}
    <div className="d-poll-card__meta"><span>{meta}</span>{canClose && !closed && <button type="button" aria-disabled={!!busy} onMouseDown={keepMouseFocus} onClick={() => { if (!busy) { closeRequested.current = focusedInside.current; onClose(); } }}>{labels.close}</button>}</div>
  </section>;
}
