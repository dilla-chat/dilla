import './Pill.css';
export type PillProps = {
  kind: 'unread' | 'mention'; count: number;
  /** The visible text in place of the bare count (a channel row's "3 new" or "@2"); the hidden count still names it. */
  text?: string;
};
export function Pill({ kind, count, text }: PillProps) {
  if (count <= 0) return null;
  const label = kind === 'unread' ? `${count} unread` : `${count} ${count === 1 ? 'mention' : 'mentions'}`;
  // No role="status": a pill is part of the row it sits in, and a live
  // region would announce every count change over whatever the user is
  // doing. The digits are the sighted half (capped at 99+); the hidden
  // span carries the real count into the row's accessible name.
  return (
    <span className="d-pill" data-kind={kind}>
      <span aria-hidden="true">{text ?? (count > 99 ? '99+' : count)}</span>
      <span className="d-sr-only">{label}</span>
    </span>
  );
}
