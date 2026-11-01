import { Icon } from '../Icon/Icon.tsx';
import './ReplyChip.css';

export interface ReplyChipProps {
  /** "Replying to Ada": the whole line, read as one. */
  label: string;
  /** The replied-to person's name inside `label`, set in bold (the prototype's "Replying to **Ada**"). */
  name?: string;
  excerpt: string;
  cancelLabel: string;
  onCancel(): void;
}

/** `label` with the last `name` in it wrapped in <b>. */
function withName(label: string, name: string | undefined) {
  const at = name ? label.lastIndexOf(name) : -1;
  if (name === undefined || at < 0) return label;
  return <>{label.slice(0, at)}<b>{name}</b>{label.slice(at + name.length)}</>;
}

/**
 * The reply bar above the composer's box (README §5.5): ↳, "Replying to **Ada**", a one-line excerpt and ×. Its bottom
 * edge joins the box, whose top corners square off while it shows (the Composer's `data-replying`).
 */
export function ReplyChip({ label, name, excerpt, cancelLabel, onCancel }: ReplyChipProps) {
  return (
    <div className="d-reply-chip">
      <Icon name="reply" size={12} />
      <span className="d-reply-chip__text">
        <span className="d-reply-chip__label">{withName(label, name)}</span>
        {excerpt !== '' ? <span className="d-reply-chip__excerpt"> · {excerpt}</span> : null}
      </span>
      <button type="button" className="d-reply-chip__cancel" aria-label={cancelLabel} onClick={() => onCancel()}>
        <Icon name="x" size={13} />
      </button>
    </div>
  );
}
