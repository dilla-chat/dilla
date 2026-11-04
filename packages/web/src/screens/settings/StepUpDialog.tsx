// Flow 10 state 5: "Confirm it is you". A step-up token is one use and short-lived; this dialog turns the account's password
// (or, with no password, a passkey) into one and hands it to the caller, who spends it on exactly one change. The password
// goes once into `stepUp` and is cleared from the field the moment it is sent; the token is never stored here.
import { useEffect, useRef, useState } from 'react';
import { Banner, ConfirmDialog, TextField } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { errorOf, type UiError } from '../../core/errors.ts';
import { t } from '../../strings/index.ts';
import { PasskeyError, getPasskey } from '../../webauthn.ts';
import { sharedRefusal } from '../factor/FactorSetup.tsx';

export interface StepUpDialogProps {
  open: boolean;
  hasPassword: boolean;
  /** No password and no passkey, but an authenticator app: the field takes its code, or a backup code. */
  code?: boolean;
  /** Why the person is asked again, shown above the field until the next attempt: the last token ran out, or an add that
   *  spent it ended without adding anything. */
  notice: 'expired' | 'again' | null;
  /** The page's one live region, shown here while this dialog is the topmost modal (the frame beneath is inert). */
  live?: React.ReactNode;
  onToken: (token: string) => void;
  onClose: () => void;
}

interface Run { busy: boolean; error: string | null; attempt: number }
const IDLE: Run = { busy: false, error: null, attempt: 0 };
const FIELD = 'stepup-password';

/** A refused step-up, by status and code only. A ceremony the browser ended itself has no code of ours. */
function say(e: UiError, hasPassword: boolean, code: boolean): string {
  if (e.status === 403 && e.code === 'E_STEP_UP') return t(hasPassword ? 'factorSettings.stepUp.failed' : code ? 'factorSettings.stepUp.codeFailed' : 'factorSettings.stepUp.passkeyFailed');
  const shared = sharedRefusal(e);
  if (shared !== null) return t(shared.key, shared.vars);
  return t('factorSettings.error.other', { code: e.code });
}

const SIX_DIGITS = /^\d{6}$/;
const BACKUP_CODE = /^[A-Z2-7]{26}$/;

export function StepUpDialog({ open, hasPassword, code = false, notice: reason, live, onToken, onClose }: StepUpDialogProps): React.JSX.Element {
  const client = useCore();
  const [password, setPassword] = useState('');
  const [run, setRun] = useState<Run>(IDLE);
  const [notice, setNotice] = useState<'expired' | 'again' | null>(null);
  const running = useRef(false);
  // Each press is an attempt. Cancel, Escape or a newer press ends it: a token that arrives for an ended attempt is dropped,
  // never handed on, so closing the dialog grants nothing.
  const attempt = useRef(0);
  useEffect(() => {
    if (open) { setNotice(reason); return; }
    attempt.current += 1;
    running.current = false;
    setPassword('');
    setRun(IDLE);
    setNotice(null);
  }, [open, reason]);

  // Walk M3 (flow 10: a field error focuses the field): a refused password puts focus back in the emptied field. This runs
  // after ConfirmDialog's own effect (a child's effects run first), which puts it on Cancel.
  useEffect(() => {
    if (open && (hasPassword || code) && !run.busy && run.error !== null) document.getElementById(FIELD)?.focus();
    // The attempt count names each refusal, the same text twice included.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [run.attempt]);
  // Contract 1: the refusal and the end of busy are one update, so ConfirmDialog moves focus to Cancel.
  const refused = (message: string) => setRun(r => ({ busy: false, error: message, attempt: r.attempt + 1 }));
  const settle = (g: number, token: string) => {
    if (g !== attempt.current) return;
    running.current = false;
    setRun(r => ({ busy: false, error: null, attempt: r.attempt }));
    onToken(token);
  };
  const fail = (g: number, e: unknown) => {
    if (g !== attempt.current) return;
    running.current = false;
    // The browser ending a ceremony (cancelled, no key) is not an answer of the instance: it has no code of ours.
    refused(e instanceof PasskeyError ? t(e.kind === 'unsupported' ? 'factorSettings.stepUp.passkeyUnsupported' : e.kind === 'rateLimited' ? 'factor.error.rateLimited' : 'factorSettings.stepUp.passkeyFailed') : say(errorOf(e), hasPassword, code));
  };

  const submit = () => {
    if (running.current) return;
    const typed = code ? password.replace(/[-\s]/g, '') : password;
    if ((hasPassword || code) && typed === '') { refused(t('factor.error.required')); return; }
    const method = SIX_DIGITS.test(typed) ? 'totp' : 'backup';
    if (code && method === 'backup' && !BACKUP_CODE.test(typed.toUpperCase())) { refused(t('factorSettings.stepUp.codeFailed')); return; }
    running.current = true;
    setNotice(null);
    const g = ++attempt.current;
    setRun(r => ({ busy: true, error: null, attempt: r.attempt }));
    if (hasPassword || code) {
      const secret = hasPassword ? password : typed;
      setPassword('');
      client.call({ m: 'stepUp', method: hasPassword ? 'password' : method, secret }).then(
        v => { settle(g, (v as { token: string }).token); }, (e: unknown) => { fail(g, e); });
      return;
    }
    const ceremony = async () => {
      const begin = await client.call({ m: 'stepUpPasskeyBegin' }) as { ceremonyId: string; options: string };
      const response = await getPasskey(begin.options);
      const done = await client.call({ m: 'stepUpPasskeyFinish', ceremonyId: begin.ceremonyId, response }) as { token: string };
      settle(g, done.token);
    };
    ceremony().catch((e: unknown) => { fail(g, e); });
  };

  return (
    <ConfirmDialog open={open} tone="accent" title={t('factorSettings.stepUp.title')}
      body={t(hasPassword ? 'factorSettings.stepUp.body' : code ? 'factorSettings.stepUp.codeBody' : 'factorSettings.stepUp.passkeyBody')}
      confirmLabel={t(hasPassword || code ? 'factorSettings.stepUp.confirm' : 'factorSettings.stepUp.usePasskey')}
      busyLabel={t('factorSettings.stepUp.working')} cancelLabel={t('factorSettings.cancel')}
      busy={run.busy} error={run.error} attempt={run.attempt} onConfirm={submit} onClose={onClose}>
      {notice === null ? null : <Banner tone="warn">{t(notice === 'again' ? 'factorSettings.stepUp.again' : 'factorSettings.stepUp.expired')}</Banner>}
      {hasPassword || code ? (
        <form noValidate onSubmit={(e) => { e.preventDefault(); submit(); }}>
          {hasPassword ? (
            <TextField id={FIELD} type="password" label={t('factorSettings.stepUp.password')} value={password}
              onChange={setPassword} autoComplete="current-password" required />
          ) : (
            <TextField id={FIELD} label={t('factorSettings.stepUp.code')} value={password} onChange={setPassword}
              autoComplete="one-time-code" spellCheck={false} required />
          )}
        </form>
      ) : null}
      {live}
    </ConfirmDialog>
  );
}
