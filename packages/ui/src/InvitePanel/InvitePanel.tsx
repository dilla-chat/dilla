import { useId } from 'react';
import { Icon } from '../Icon/Icon.tsx';
import { Segmented } from '../Segmented/Segmented.tsx';
import './InvitePanel.css';

export interface InvitePanelRow {
  id: string;
  /** The link's masked code (the instance keeps only a fingerprint, so a code is never shown again). */
  code: string;
  uses: string;
  expires: string;
  /** The Revoke button's accessible name; absent when this account may not revoke the link. */
  revokeLabel?: string;
}

export interface InvitePanelProps {
  expiry: { label: string; options: readonly { value: string; label: string }[]; value: string; onChange(value: string): void };
  uses: { label: string; options: readonly { value: string; label: string }[]; value: string; onChange(value: string): void };
  /** The link just made, shown once with Copy and the note; null shows "Make invite link". */
  made: { url: string; note: string } | null;
  making: boolean;
  onMake(): void;
  onCopy(): void;
  /** A line under the maker (a refusal, or why "never" and "∞" are missing); an error is an alert. */
  message?: { text: string; alert: boolean; n: number } | null;
  activeTitle: string;
  rows: readonly InvitePanelRow[];
  busyId?: string | null;
  onRevoke(id: string): void;
  labels: { make: string; making: string; copy: string; link: string; revoke: string; list: string };
}

/**
 * Server settings → Invites as the prototype draws it (README §5.9): a card with the expiry and use segmented controls and
 * "Make invite link"; the link it makes is shown once with Copy and a note on its limits. Under it, "Active links — n" and
 * one 48px row per live link with its masked code, uses, expiry and Revoke.
 */
export function InvitePanel({ expiry, uses, made, making, onMake, onCopy, message, activeTitle, rows, busyId, onRevoke, labels }: InvitePanelProps) {
  const uid = useId();
  return (
    <div className="d-invite-panel">
      <div className="d-invite-panel__card">
        <div className="d-invite-panel__choices">
          <div className="d-invite-panel__choice">
            <span className="d-invite-panel__label" aria-hidden="true">{expiry.label}</span>
            <Segmented id={`${uid}-expiry`} label={expiry.label} options={expiry.options} value={expiry.value} onChange={expiry.onChange} />
          </div>
          <div className="d-invite-panel__choice">
            <span className="d-invite-panel__label" aria-hidden="true">{uses.label}</span>
            <Segmented id={`${uid}-uses`} label={uses.label} options={uses.options} value={uses.value} onChange={uses.onChange} />
          </div>
        </div>
        {made !== null ? (
          <>
            <div className="d-invite-panel__made">
              <input className="d-invite-panel__link" aria-label={labels.link} readOnly value={made.url} onFocus={e => e.currentTarget.select()} />
              <button type="button" className="d-invite-panel__primary" onClick={onCopy}><Icon name="copy" size={13} />{labels.copy}</button>
            </div>
            <p className="d-invite-panel__note">{made.note}</p>
          </>
        ) : (
          <button type="button" className="d-invite-panel__primary d-invite-panel__make" aria-disabled={making || undefined}
            onClick={() => { if (!making) onMake(); }}>
            <Icon name="plus" size={14} />{making ? labels.making : labels.make}
          </button>
        )}
        {message ? <p key={message.n} className="d-invite-panel__message" role={message.alert ? 'alert' : undefined}>{message.text}</p> : null}
      </div>
      <h3 className="d-invite-panel__title">{activeTitle}</h3>
      {rows.length > 0 ? (
        <ul className="d-invite-panel__list" aria-label={labels.list}>
          {rows.map(row => (
            <li key={row.id} className="d-invite-panel__row">
              <span className="d-invite-panel__code">{row.code}</span>
              <span className="d-invite-panel__meta">{row.uses}</span>
              <span className="d-invite-panel__meta">{row.expires}</span>
              {row.revokeLabel !== undefined ? (
                <button type="button" className="d-invite-panel__revoke" aria-label={row.revokeLabel}
                  aria-disabled={busyId === row.id || undefined} onClick={() => { if (busyId !== row.id) onRevoke(row.id); }}>{labels.revoke}</button>
              ) : null}
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}
