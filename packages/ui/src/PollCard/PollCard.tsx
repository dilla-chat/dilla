import { useId } from 'react';
import { Icon } from '../Icon/Icon.tsx';
import { Avatar } from '../Avatar/Avatar.tsx';
import './PollCard.css';
export interface PollCardOption { text: string; pct: number; mine: boolean; winner: boolean; voters: readonly { name: string }[] }
export interface PollCardProps { question: string; closed: boolean; multi: boolean; meta: string; options: readonly PollCardOption[];
  canVote: boolean; canClose: boolean; busy?: boolean; labels: { closed: string; close: string };
  onVote(index: number): void; onClose(): void }
export function PollCard({ question, closed, multi, meta, options, canVote, canClose, busy, labels, onVote, onClose }: PollCardProps) {
  const heading = useId();
  return <section className="d-poll-card" aria-labelledby={heading} data-multi={multi} data-closed={closed}>
    <div className="d-poll-card__head"><Icon name="vote" size={14} /><h3 id={heading}>{question}</h3>{closed && <span className="d-poll-card__tag">{labels.closed}</span>}</div>
    {options.map((option, index) => <button type="button" key={index} className="d-poll-card__option" data-mine={option.mine} data-winner={closed && option.winner && option.pct > 0}
      aria-pressed={option.mine} disabled={closed || !canVote || busy} onClick={() => onVote(index)}>
      <span className="d-poll-card__fill" style={{ width: `${option.pct}%` }} />
      <span className="d-poll-card__row"><span className="d-poll-card__check">{option.mine && <Icon name="check" size={10} stroke={3} />}</span>
        <span className="d-poll-card__text">{option.text}</span><span className="d-poll-card__voters">{option.voters.slice(0, 5).map((voter, i) => <span title={voter.name} key={i}><Avatar name={voter.name} initials={voter.name.slice(0, 2).toUpperCase()} size="mini" /></span>)}</span>
        <span className="d-poll-card__pct">{option.pct}%</span></span>
    </button>)}
    <div className="d-poll-card__meta"><span>{meta}</span>{canClose && !closed && <button type="button" disabled={busy} onClick={onClose}>{labels.close}</button>}</div>
  </section>;
}
