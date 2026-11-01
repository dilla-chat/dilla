import { useEffect, useId, useRef, type ReactNode } from 'react';
import { Button } from '../Button/Button.tsx';
import { Avatar } from '../Avatar/Avatar.tsx';
import { Dialog } from '../Dialog/Dialog.tsx';
import './ConfirmDialog.css';

export interface ConfirmDialogProps {
  open: boolean;
  title: string;
  body: string;
  confirmLabel: string;
  busyLabel: string;
  cancelLabel: string;
  tone?: 'danger' | 'accent';
  /** True while the confirmed action runs. Contract: when it fails, the caller clears `busy` and sets `error` in the same update. */
  busy: boolean;
  /** The refusal to show, or null. Set it in the same update that clears `busy`: that is what moves focus to Cancel. */
  error: string | null;
  /** Counts the refusals the caller has shown. A new count with the same text is a new alert, so it is announced again. */
  attempt?: number;
  /** False keeps the confirm button disabled (a typed name that does not match yet). Defaults to true. */
  canConfirm?: boolean;
  /** The person the action is about (kick, ban, time out, warn): their 40px avatar beside the title (the prototype's confirmation). */
  person?: string;
  /** Extra fields under the body (a reason, the typed name). */
  children?: ReactNode;
  onConfirm(): void;
  onClose(): void;
}

/**
 * A confirmation over `Dialog`. Focus goes to the first field in `children`, otherwise to Cancel, never to the
 * destructive button. The confirm button is disabled while `!canConfirm` or busy. A refusal is a `role=alert` under the
 * fields, announced again when `attempt` changes.
 */
export function ConfirmDialog({
  open, title, body, confirmLabel, busyLabel, cancelLabel, tone = 'danger', busy, error, attempt = 0, canConfirm = true,
  person, children, onConfirm, onClose,
}: ConfirmDialogProps) {
  const bodyRef = useRef<HTMLDivElement>(null);
  // After Dialog's own open effect (a child's effects run first): a field or Cancel holds focus, not the dialog.
  const cancelButton = () => bodyRef.current?.closest('dialog')?.querySelector<HTMLElement>('.d-dialog__footer button');
  useEffect(() => {
    if (!open) return;
    const field = bodyRef.current?.querySelector<HTMLElement>(
      'input:not([type="hidden"]):not([disabled]):not([readonly]), select:not([disabled]), textarea:not([disabled]):not([readonly])');
    (field ?? cancelButton())?.focus();
  }, [open]);
  // The confirm button was disabled while busy, so focus may have been lost; a refusal puts it on Cancel (never on the
  // destructive button), next to the alert.
  // This relies on the caller setting `error` in the same update that clears `busy`.
  const wasBusy = useRef(false);
  useEffect(() => {
    if (open && wasBusy.current && !busy && error !== null) cancelButton()?.focus();
    wasBusy.current = busy;
  }, [busy, open, error]);
  const bodyId = `${useId()}-body`;
  return (
    <Dialog open={open} kind="confirm" title={title} closeLabel={cancelLabel} onClose={onClose} describedBy={bodyId}
      lead={person === undefined ? undefined : <span className="d-confirm-dialog__avatar" aria-hidden="true"><Avatar name={person} size="confirm" /></span>}
      description={<p id={bodyId} className="d-confirm-dialog__body">{body}</p>}
      footer={<Button variant={tone} type="button" disabled={!canConfirm || busy} onClick={onConfirm}>{busy ? busyLabel : confirmLabel}</Button>}>
      <div ref={bodyRef} className="d-confirm-dialog" hidden={children === undefined && error === null ? true : undefined}>
        {children}
        {error !== null ? <p key={attempt} className="d-confirm-dialog__error" role="alert">{error}</p> : null}
      </div>
    </Dialog>
  );
}
