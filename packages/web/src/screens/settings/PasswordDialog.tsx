// Flow 15: change the password, or set a first one. The old password is the step-up where the account has a password; a first
// password on an account that holds a second factor spends a step-up token the caller got from the step-up dialog. The three
// fields live in this component's state until the dialog closes; the current one is cleared the moment it is sent, and the
// worker keeps none of them (it passes them to the one route and answers with nothing).
import { useEffect, useRef, useState } from 'react';
import { ConfirmDialog, TextField } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import type { UiError } from '../../core/errors.ts';
import { t, type StringKey } from '../../strings/index.ts';
import { useRun } from '../admin/shared.ts';
import { sharedRefusal } from '../factor/FactorSetup.tsx';

/** The same floor as the sign-up password (Onboarding). */
const PASSWORD_MIN = 12;
/** The instance bounds a password only by its request size; this is a clean limit before that. */
const PASSWORD_MAX = 1024;
const OLD = 'password-old';
const NEW = 'password-new';
const AGAIN = 'password-again';

type Field = 'old' | 'next' | 'again';
const IDS: Record<Field, string> = { old: OLD, next: NEW, again: AGAIN };

export interface PasswordDialogProps {
  open: boolean;
  /** True: the dialog asks for the current password (a change). False: a first password. */
  hasPassword: boolean;
  /** The account's username, for the password manager. */
  username: string;
  /** The step-up token for a first password on an account holding a second factor, else null. */
  stepup: string | null;
  /** The page's one live region, shown here while this dialog is the topmost modal (the frame beneath is inert). */
  live?: React.ReactNode;
  /** The password is saved. */
  onDone: () => void;
  /** The step-up token was refused as spent or expired (403 E_STEP_UP): the caller goes back to the step-up dialog. */
  onStepUpLost: () => void;
  /** A first password was refused because the session went stale while the dialog was open. */
  onStale: () => void;
  onClose: () => void;
}

const codePoints = (s: string): number => [...s].length;

export function PasswordDialog({ open, hasPassword, username, stepup, live, onDone, onStepUpLost, onStale, onClose }: PasswordDialogProps): React.JSX.Element {
  const client = useCore();
  const [old, setOld] = useState('');
  const [next, setNext] = useState('');
  const [again, setAgain] = useState('');
  const [errors, setErrors] = useState<Partial<Record<Field, StringKey>>>({});
  const action = useRun();
  const focus = useRef<Field | null>(null);
  const refusedWrong = useRef(false);

  useEffect(() => {
    if (open) return;
    setOld('');
    setNext('');
    setAgain('');
    setErrors({});
    action.reset();
    // action.reset is stable; the effect is for the dialog closing.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  // A field error, or a refused current password, puts focus on that field. This runs after ConfirmDialog's own effect (a
  // child's effects run first), which would put it on Cancel.
  useEffect(() => {
    if (focus.current === null) return;
    document.getElementById(IDS[focus.current])?.focus();
    focus.current = null;
  }, [errors]);
  useEffect(() => {
    if (!refusedWrong.current) return;
    refusedWrong.current = false;
    document.getElementById(OLD)?.focus();
  }, [action.attempt]);

  const edit = (field: Field, set: (v: string) => void) => (value: string) => {
    set(value);
    if (errors[field] !== undefined) setErrors(e => ({ ...e, [field]: undefined }));
  };

  const say = (e: UiError): string => {
    if (e.status === 403 && e.code === 'E_STEP_UP') { onStepUpLost(); return ''; }
    if (e.status === 403 && e.code === 'E_FORBIDDEN') {
      if (hasPassword) { refusedWrong.current = true; return t('settings.password.error.wrong'); }
      onStale();
      return '';
    }
    const shared = sharedRefusal(e);
    return shared !== null ? t(shared.key, shared.vars) : t('settings.password.error.other', { code: e.code });
  };

  const submit = () => {
    const found: Partial<Record<Field, StringKey>> = {};
    if (hasPassword && old === '') found.old = 'settings.password.error.oldRequired';
    if (codePoints(next) < PASSWORD_MIN) found.next = 'settings.password.error.short';
    else if (again !== next) found.again = 'settings.password.error.mismatch';
    const first = (['old', 'next', 'again'] as const).find(f => found[f] !== undefined);
    if (first !== undefined) { focus.current = first; setErrors(found); return; }
    setErrors({});
    const sent = old;
    setOld('');
    void action.run(() => client.call({ m: 'changePassword', old: hasPassword ? sent : null, next, stepup: hasPassword ? null : stepup }), say)
      .then(ok => { if (ok) onDone(); });
  };

  const text = (field: Field): string | undefined => { const key = errors[field]; return key === undefined ? undefined : t(key); };

  return (
    <ConfirmDialog open={open} tone="accent" title={t(hasPassword ? 'settings.password.titleChange' : 'settings.password.titleSet')}
      body={t(hasPassword ? 'settings.password.bodyChange' : 'settings.password.bodySet')}
      confirmLabel={t(hasPassword ? 'settings.password.confirmChange' : 'settings.password.confirmSet')}
      busyLabel={t('settings.password.busy')} cancelLabel={t('settings.password.cancel')}
      busy={action.busy} error={action.error === '' ? null : action.error} attempt={action.attempt}
      onConfirm={submit} onClose={() => { if (!action.busy) onClose(); }}>
      {/* Several fields need a submit button for Enter to submit; the dialog's own confirm sits in its footer, outside the form. */}
      <form noValidate onSubmit={(e) => { e.preventDefault(); submit(); }}>
        <button type="submit" hidden tabIndex={-1} aria-hidden="true" />
        <label className="d-sr-only" htmlFor="password-username">{t('settings.password.username')}</label>
        <input id="password-username" className="d-sr-only" type="text" value={username} readOnly tabIndex={-1} autoComplete="username" />
        {hasPassword ? (
          <TextField id={OLD} type="password" label={t('settings.password.old')} value={old} onChange={edit('old', setOld)}
            error={text('old')} autoComplete="current-password" required />
        ) : null}
        <TextField id={NEW} type="password" label={t('settings.password.new')} value={next} onChange={edit('next', setNext)}
          hint={t('settings.password.newHint')} error={text('next')} autoComplete="new-password" maxLength={PASSWORD_MAX} required />
        <TextField id={AGAIN} type="password" label={t('settings.password.confirm')} value={again} onChange={edit('again', setAgain)}
          error={text('again')} autoComplete="new-password" maxLength={PASSWORD_MAX} required />
      </form>
      {live}
    </ConfirmDialog>
  );
}
