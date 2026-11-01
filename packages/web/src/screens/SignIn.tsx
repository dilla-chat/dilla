import { useEffect, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { BACKUP_CODE, normaliseBackupCode, normaliseRecoveryKey } from '@dilla/client-core';
import { Banner, Button, OnboardingFrame, RecoveryKeyField, Splash, TextField } from '@dilla/ui';
import { useCore } from '../core/context.tsx';
import { errorOf, type UiError } from '../core/errors.ts';
import { useSlice } from '../core/use-slice.ts';
import { t, type StringKey } from '../strings/index.ts';
import { PasskeyError, getPasskey, passkeysUsable } from '../webauthn.ts';
import { useAnnouncer } from './announcer.ts';
import { doneProblem, type DoneProblem } from './factor/doneRefusal.ts';
import { FactorSetup } from './factor/FactorSetup.tsx';
import { ForgetBrowser } from './factor/ForgetBrowser.tsx';

const KEY_LENGTH = 52;
type Field = 'username' | 'password' | 'code' | 'key' | 'backup' | 'passkey';
type SignInCommand = 'login' | 'totp' | 'key' | 'passkey' | 'backup';
type Shown = 'login' | 'totp' | 'key' | 'noBackup' | 'done' | 'passkey' | 'backup' | 'factor';
const FIELD_ID: Readonly<Record<Field, string>> = {
  username: 'signin-username', password: 'signin-password', code: 'signin-code', key: 'signin-key',
  backup: 'signin-backup', passkey: 'signin-passkey',
};
const FIELD_ORDER: readonly Field[] = ['username', 'password', 'code', 'backup', 'key'];
const LENGTH_ID = 'signin-key-length';
const BACKUP_ID = 'signin-key-backup';
/** How long the key field rests before a backup code is named (R44): a key being typed passes 26 characters on the way. */
const BACKUP_NOTE_MS = 600;

interface BannerState { tone: 'warn' | 'danger'; key: StringKey; vars: Record<string, string | number> }
interface RefusalView { field?: Field; fieldKey?: StringKey; focus?: Field; banner?: BannerState; noBackup?: true }

/** What a passkey ceremony that ended in the browser (cancelled, the wrong address, nothing usable) shows. */
const PASSKEY_FAILED: BannerState = { tone: 'danger', key: 'factorSignin.passkey.failed', vars: {} };

/**
 * The refusal table of flow 03 (L-COPY-02 as task 15 froze it), first match wins. It reads the code, the HTTP
 * status and the wait, and never the server's detail text (protocol/02: a client must not parse it). The step a
 * refusal shows on is the one `account.phase` names afterwards, never the command's (head ruling 39).
 */
function signInRefusal(e: UiError, command: SignInCommand, instance: string): RefusalView {
  if (e.code === 'E_NO_BACKUP') {
    // From the fetch after the login the account has no recovery data; from signInKey its backup state is
    // missing or unreadable (task 14's ruling (e)).
    const key: StringKey = command === 'key' ? 'signin.error.noBackupState' : 'signin.error.noBackup';
    return { noBackup: true, banner: { tone: 'danger', key, vars: { instance } } };
  }
  if (e.code === 'E_RECOVERY_KEY') return { field: 'key', fieldKey: 'signin.error.wrongKey' };
  // web3.8 (R6, pre-flight C-15/C-16): the held assertion ran out during the key, from the registration or from the
  // backup code on its path; nothing was spent and the worker is back at signin-login. The person logs in again.
  if (e.code === 'E_SIGNIN_EXPIRED') return { banner: { tone: 'warn', key: 'enrol.expired', vars: {} }, focus: 'password' };
  // web3.6: the instance gives an unknown passkey its own code (never read from the detail text).
  if (e.code === 'E_PASSKEY_UNKNOWN' && command === 'passkey') {
    return { banner: { tone: 'danger', key: 'factorSignin.passkey.unknown', vars: { instance } }, focus: 'passkey' };
  }
  if ((e.code === 'E_UNAUTHENTICATED' || e.code === 'E_NO_ASSERTION') && command === 'backup') {
    // The worker moved the phase to signin-login: the assertion is spent. The password and the typed key are cleared.
    return { banner: { tone: 'warn', key: 'factorSignin.backup.failed', vars: {} }, focus: 'password' };
  }
  if ((e.code === 'E_UNAUTHENTICATED' || e.code === 'E_NO_ASSERTION') && command === 'totp') {
    // The worker moved the phase to signin-login: the assertion is spent and the person logs in again.
    return { banner: { tone: 'warn', key: 'signin.error.totpFailed', vars: {} }, focus: 'password' };
  }
  if (e.code === 'E_UNAUTHENTICATED' && command === 'login') {
    return { field: 'password', fieldKey: 'signin.error.loginFailed', focus: 'password' };
  }
  // REGISTRATION-DEVICES-02: the registration runs inside signInKey, after the key's form passed; its 403 is the cap.
  if (e.code === 'E_FORBIDDEN') {
    return { banner: { tone: 'danger', key: 'signin.error.deviceCap', vars: { instance } } };
  }
  // The row this browser registered was replaced before its list PUT; the worker dropped the enrolment (step 1).
  if (e.code === 'E_SIGNIN_EVICTED') return { banner: { tone: 'danger', key: 'signin.error.evicted', vars: {} } };
  if (e.code === 'E_RATE_LIMITED') {
    return {
      banner: e.retryAfterMs !== null
        ? { tone: 'warn', key: 'signin.error.tooMany', vars: { seconds: Math.ceil(e.retryAfterMs / 1000) } }
        : { tone: 'warn', key: 'onboarding.error.rateLimitedNoWait', vars: {} },
    };
  }
  if (e.code === 'E_NETWORK' || e.status >= 500) return { banner: { tone: 'danger', key: 'signin.error.network', vars: {} } };
  if (command === 'passkey') return { banner: { tone: 'danger', key: 'factorSignin.passkey.failed', vars: {} }, focus: 'passkey' };
  return { banner: { tone: 'danger', key: 'signin.error.other', vars: { code: e.code } } };
}

/** The four-step sign-in ceremony of L-COPY-02. Rendered by App for the phases signin-login, signin-totp,
 *  signin-passkey, signin-key and enrolling, and kept mounted after the phase turns ready until its done step calls
 *  onFinish. web3.8 (R6): the login first, then the second factor, then the recovery key. The login and the factor
 *  never register this browser: the worker holds the settled assertion, and the key's command registers it after the
 *  key's form check, back to back with the fetch, the enrolment and the list PUT (REGISTRATION-DEVICES-02). A backup
 *  code waits in page memory and goes only with the key (signInBackupCode), so an assertion that ran out meanwhile
 *  spends none. Phase signin-key is the key step, before or after the registration (a wrong key, a reload after it):
 *  its step sends signInKey. Step numbers follow R7. */
export function SignIn(props: { onFinish(): void }): React.JSX.Element {
  const client = useCore();
  const account = useSlice('account');
  const [username, setUsername] = useState(() => account?.signIn?.username ?? '');
  // The password stays in page memory no longer than the call (Global Constraints "Never written").
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');
  const [key, setKey] = useState('');
  // The backup-code form passed its check and the code waits in page memory for the key step; never stored.
  const [backupHeld, setBackupHeld] = useState(false);
  // R7: a factor step (code, passkey or backup code) was shown in this ceremony, so the key is step 3, not 2.
  const [factorShown, setFactorShown] = useState(false);
  // Signal task 10 fix round 3: this ceremony reached a factor set-up (login, key, set-up, done: four dots).
  const [setupSeen, setSetupSeen] = useState(false);
  const [errors, setErrors] = useState<Partial<Record<Field, StringKey>>>({});
  const [banner, setBanner] = useState<BannerState | null>(null);
  // The no-backup view's banner after a refusal of this mount; null otherwise.
  const [noBackup, setNoBackup] = useState<StringKey | null>(null);
  const [busy, setBusy] = useState<SignInCommand | null>(null);
  // The step whose button started the running command, so the working state shows where the person pressed.
  const [busyStep, setBusyStep] = useState<Shown | null>(null);
  const [backupMode, setBackupMode] = useState(false);
  const [backup, setBackup] = useState('');
  // A backup code was sent with the key: the done step says how many remain. Back at the login the code was refused or
  // the ceremony began again, so the flag clears.
  const [usedBackup, setUsedBackup] = useState(false);
  const [factorDone, setFactorDone] = useState<{ busy: boolean; problem: DoneProblem | null }>({ busy: false, problem: null });
  const { text: announcement, announce } = useAnnouncer();
  const factors = useSlice('factors');
  const [focusRequest, setFocusRequest] = useState<{ target: 'heading' | Field; n: number }>({ target: 'heading', n: 0 });
  const rootRef = useRef<HTMLDivElement>(null);

  // R44: the note shows only once a backup-code-shaped entry has rested, or the field is left while it holds.
  const [noteSettled, setNoteSettled] = useState(false);
  const looksBackup = BACKUP_CODE.test(normaliseBackupCode(key));
  useEffect(() => {
    setNoteSettled(false);
    if (!looksBackup) return undefined;
    const timer = setTimeout(() => { setNoteSettled(true); }, BACKUP_NOTE_MS);
    return () => { clearTimeout(timer); };
  }, [key, looksBackup]);
  const showBackup = looksBackup && noteSettled;
  // aria-describedby is read only on focus; the person is in the field, so the note is also spoken once as it appears.
  useEffect(() => { if (showBackup) announce(t('signIn.key.isBackupCode')); }, [showBackup, announce]);

  const phase = account?.phase;
  const instance = account?.instance ?? null;
  const enrolling = phase === 'enrolling';
  const blocked = busy !== null || enrolling;
  // Requirement 10: a reload in the ceremony meets E_NO_BACKUP only through the slice. No other code is read there.
  const noBackupKey: StringKey | null = phase !== 'signin-key' ? null
    : noBackup ?? (busy === null && account?.error?.code === 'E_NO_BACKUP' ? 'signin.error.noBackup' : null);
  // A passkey-only account in a browser that cannot run a passkey ceremony gets no screen whose button can only fail:
  // the backup-code form opens directly and has no Back (there is nothing to go back to).
  const noPasskeys = phase === 'signin-passkey' && !passkeysUsable(instance);
  const shown: Shown = phase === 'ready' ? 'done'
    : phase === 'factor-setup' ? 'factor'
      : noBackupKey !== null ? 'noBackup'
        : phase === 'signin-totp' ? (backupHeld ? 'key' : backupMode ? 'backup' : 'totp')
          : phase === 'signin-passkey' ? (backupHeld ? 'key' : backupMode || noPasskeys ? 'backup' : 'passkey')
            : phase === 'signin-key' ? 'key'
              : enrolling ? busyStep ?? 'key'
                : 'login';

  // A cancel and a fresh ceremony start clean.
  useEffect(() => { if (phase !== 'signin-key') setNoBackup(null); }, [phase]);
  // The backup-code form and its held code belong to the code and passkey steps; a refused code, an expiry or a cancel
  // leaves them, and so does signin-key (the code was spent by the registration that reached it).
  useEffect(() => {
    if (phase !== 'signin-totp' && phase !== 'signin-passkey' && phase !== 'enrolling') {
      setBackupMode(false); setBackup(''); setBackupHeld(false);
    }
  }, [phase]);
  // R7: the factor step counts from when it shows until the ceremony returns to the login.
  useEffect(() => {
    if (phase === 'signin-totp' || phase === 'signin-passkey') setFactorShown(true);
    else if (phase === 'signin-login' || phase === 'needs-signup') { setFactorShown(false); setSetupSeen(false); setUsedBackup(false); }
    if (phase === 'factor-setup') setSetupSeen(true);
  }, [phase]);
  // The typed key belongs to one person's attempt: back at the login, whoever signs in next starts with an empty field.
  // A refusal at the key step itself (a wrong key) keeps the text for the same person's retry.
  useEffect(() => { if (phase === 'signin-login') setKey(''); }, [phase]);

  // OnboardingFrame focuses its heading when the title changes; this effect runs after the frame's (child effects
  // first), so a field request after a step change lands on the field.
  useEffect(() => {
    const root = rootRef.current;
    if (!root) return;
    if (focusRequest.target === 'heading') root.querySelector('h1')?.focus();
    else root.querySelector<HTMLElement>(`#${FIELD_ID[focusRequest.target]}`)?.focus();
  }, [shown, focusRequest]);

  if (account === undefined || instance === null) return <Splash status={t('boot.loading.status')} />;
  const name = instance.name;

  const requestFocus = (target: 'heading' | Field) => setFocusRequest(f => ({ target, n: f.n + 1 }));
  const change = (field: Field, set: (value: string) => void) => (value: string) => {
    set(value);
    setErrors(e => {
      if (e[field] === undefined) return e;
      const rest = { ...e };
      delete rest[field];
      return rest;
    });
  };
  const showErrors = (next: Partial<Record<Field, StringKey>>): boolean => {
    setErrors(next);
    const first = FIELD_ORDER.find(f => next[f] !== undefined);
    if (first === undefined) return false;
    requestFocus(first);
    return true;
  };
  const apply = (e: UiError, command: SignInCommand) => {
    const view = signInRefusal(e, command, name);
    if (view.noBackup && view.banner) setNoBackup(view.banner.key);
    if (view.field && view.fieldKey) {
      // A key refused by its form or by the account stays on the key step, its text kept.
      setErrors({ [view.field]: view.fieldKey });
      requestFocus(view.field);
    }
    if (view.focus) {
      if (view.focus === 'password') setPassword('');
      requestFocus(view.focus);
    } else if (command === 'passkey') {
      // After a refused or cancelled ceremony focus goes back to the button that started it.
      requestFocus('passkey');
    }
    setBanner(view.noBackup ? null : view.banner ?? null);
  };
  const begin = (command: SignInCommand, step: Shown) => { setBusy(command); setBusyStep(step); };
  const run = (command: SignInCommand, call: Promise<unknown>, done: (value: unknown) => void) => {
    void call
      .then(done, (e: unknown) => { apply(errorOf(e), command); })
      .finally(() => setBusy(null));
  };

  const submitLogin = () => {
    const next: Partial<Record<Field, StringKey>> = {};
    if (username.trim() === '') next.username = 'signin.error.required';
    if (password === '') next.password = 'signin.error.required';
    if (showErrors(next)) return;
    begin('login', 'login');
    // web3.8 (R6): the login carries no key; the factor step or the key step follows it.
    run('login', client.call({ m: 'signInLogin', username: username.trim().toLowerCase(), password }), () => {
      setPassword('');
    });
  };
  const submitTotp = () => {
    // A11Y-DESIGN-06: an authenticator may copy the code grouped ("123 456"); the spaces are not part of it.
    const trimmed = code.replace(/\s/g, '');
    if (showErrors(trimmed === '' ? { code: 'signin.error.required' } : {})) return;
    begin('totp', 'totp');
    run('totp', client.call({ m: 'signInTotp', code: trimmed }), () => setCode(''));
  };
  const submitKey = () => {
    // The page counts only; Rust normalises and judges the raw text (L-CORE-26 step 1).
    if (normaliseRecoveryKey(key).length !== KEY_LENGTH) {
      showErrors({ key: 'signin.error.keyLength' });
      return;
    }
    // R6: a held backup code goes now, with the key; the instance checks the assertion before the code.
    if (backupHeld) {
      begin('backup', 'key');
      // The code is spent from the moment it is sent: a call that then fails on the key (a retry goes through signInKey)
      // has still used one, so the done step says how many remain whichever call finishes the sign-in.
      setUsedBackup(true);
      run('backup', client.call({ m: 'signInBackupCode', code: normaliseBackupCode(backup), recoveryKey: key }), () => {
        setKey('');
        setBackup('');
      });
      return;
    }
    begin('key', 'key');
    run('key', client.call({ m: 'signInKey', recoveryKey: key }), () => setKey(''));
  };
  /** The backup code is checked for its form and held in page memory; the key step sends it (R6). */
  const submitBackup = () => {
    if (!BACKUP_CODE.test(normaliseBackupCode(backup))) { showErrors({ backup: 'factorSignin.backup.format' }); return; }
    setBackupHeld(true);
    requestFocus('heading');
  };
  /** The passkey on its own: the instance's options go to the browser and its answer back; the page makes no challenge. */
  const submitPasskey = (from: Shown) => {
    if (blocked) return;
    setBanner(null);
    setErrors({});
    begin('passkey', from);
    const go = async () => {
      try {
        const began = await client.call({ m: 'signInPasskeyBegin' }) as { ceremonyId: string; options: string };
        const response = await getPasskey(began.options);
        // web3.8 (R6): the passkey carries no key; a code (needs 1) or the key step follows it.
        await client.call({ m: 'signInPasskey', ceremonyId: began.ceremonyId, response });
      } catch (e) {
        if (e instanceof PasskeyError) {
          setBanner(PASSKEY_FAILED);
          requestFocus('passkey');
        } else {
          apply(errorOf(e), 'passkey');
        }
      } finally {
        setBusy(null);
      }
    };
    void go();
  };
  const openBackup = () => { if (blocked) return; setBanner(null); setErrors({}); setBackupMode(true); requestFocus('backup'); };
  const closeBackup = () => { if (blocked) return; setBanner(null); setErrors({}); setBackupMode(false); setBackup(''); requestFocus('heading'); };
  // The factor was set up here: the worker reads the factors again, then the browser is ready.
  // Returns the call so FactorSetup knows when it ended: a refusal re-arms its button, which retries.
  const finishFactor = (): Promise<void> => {
    setFactorDone({ busy: true, problem: null });
    return client.call({ m: 'factorSetupDone', invite: null }).then(
      () => { setFactorDone({ busy: false, problem: null }); },
      (e: unknown) => { setFactorDone({ busy: false, problem: doneProblem(errorOf(e)) }); throw e; });
  };
  const onSubmit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (blocked) return;
    setBanner(null);
    setErrors({});
    switch (shown) {
      case 'login': submitLogin(); break;
      case 'totp': submitTotp(); break;
      case 'key': submitKey(); break;
      case 'passkey': submitPasskey('passkey'); break;
      case 'backup': submitBackup(); break;
      default: break;
    }
  };
  const cancel = () => {
    if (blocked) return;
    setBackupHeld(false);
    setBackup('');
    setKey('');
    client.call({ m: 'signInCancel' }).catch((e: unknown) => { apply(errorOf(e), 'login'); });
  };

  // R7: login 1, the second factor 2, the key 3 (or 2 when no factor step was shown), done last. Signal task 10 fix round 3:
  // the prototype's "Sign in · step 1 of 4" and its dots count only the steps this ceremony shows, so no dot is skipped: 4 with
  // a factor step or a factor set-up (login, key, set-up, done), 3 without one. The login step cannot know yet and says 4.
  const keyStep = factorShown ? 3 : 2;
  const total = shown === 'login' || factorShown || setupSeen || shown === 'factor' ? 4 : 3;
  const stepLabel = (n: number) => t('signin.step', { n, total });
  const progress = (n: number) => ({ n, total });
  const errorText = (field: Field) => {
    const k = errors[field];
    return k === undefined ? undefined : t(k);
  };
  // A blocked button stays focusable (aria-disabled, never the native attribute): the person who pressed it
  // keeps focus there while the step works.
  const blockedProps = blocked ? { 'aria-disabled': true } : {};
  const cancelButton = (label: StringKey) => (
    <Button variant="ghost" type="button" onClick={cancel} {...blockedProps}>{t(label)}</Button>
  );
  const bannerEl = banner === null ? null : <Banner tone={banner.tone}>{t(banner.key, banner.vars)}</Banner>;
  const form = (title: string, n: number, foot: ReactNode, children: ReactNode) => (
    <form noValidate onSubmit={onSubmit}>
      <OnboardingFrame title={title} stepLabel={stepLabel(n)} progress={progress(n)} footer={foot}>
        {bannerEl}
        {children}
      </OnboardingFrame>
    </form>
  );

  let content: React.JSX.Element;
  if (shown === 'done') {
    const who = account.user?.username ?? account.signIn?.username ?? username.trim().toLowerCase();
    const left = usedBackup && factors !== undefined ? factors.backupCodesLeft : null;
    content = (
      <OnboardingFrame title={t('signin.done.title')} stepLabel={stepLabel(total)} progress={progress(total)}
        footer={<Button variant="accent" type="button" onClick={() => props.onFinish()}>{t('signin.done.next')}</Button>}>
        <p>{t('signin.done.body', { username: who, instance: name })}</p>
        {left === null ? null : <p>{t(left === 1 ? 'factorSignin.left.one' : 'factorSignin.left.other', { count: left })}</p>}
      </OnboardingFrame>
    );
  } else if (shown === 'factor') {
    // Flow 10: an account found owing at sign-in sets up its factor here, as step 3; there is no skip.
    content = (
      <>
        {factorDone.problem === null ? null : <Banner tone="danger">{t(factorDone.problem.key, factorDone.problem.vars)}</Banner>}
        <FactorSetup origin="signin" stepLabel={stepLabel(3)} progress={progress(3)} instanceName={name}
          username={account.user?.username ?? account.signIn?.username ?? username.trim().toLowerCase()}
          announce={announce} onDone={finishFactor} />
        <ForgetBrowser />
        <p className="d-sr-only" role="status" aria-live="polite" data-testid="signin-announcer">{announcement}</p>
      </>
    );
  } else if (shown === 'passkey') {
    const working = busy === 'passkey' || enrolling;
    content = form(t('factorSignin.passkey.title'), 2, <>
      {cancelButton('signin.cancel')}
      <Button id={FIELD_ID.passkey} variant="accent" type="submit" {...blockedProps}>
        {t(working ? 'factorSignin.passkey.working' : 'factorSignin.passkey.submit')}
      </Button>
    </>, <>
      <p>{t('factorSignin.passkey.body')}</p>
      <Button variant="ghost" type="button" onClick={openBackup} {...blockedProps}>{t('factorSignin.useBackup')}</Button>
      <p role="status">{enrolling ? t('signin.key.working') : null}</p>
    </>);
  } else if (shown === 'backup') {
    content = form(t('factorSignin.backup.title'), 2, <>
      {noPasskeys ? cancelButton('signin.cancel')
        : <Button variant="ghost" type="button" onClick={closeBackup} {...blockedProps}>{t('factor.back')}</Button>}
      <Button variant="accent" type="submit" {...blockedProps}>{t('signin.login.submit')}</Button>
    </>, <>
      {noPasskeys ? <p>{t('factorSignin.passkey.unusable')}</p> : null}
      <p>{t('factorSignin.backup.body')}</p>
      <TextField id={FIELD_ID.backup} label={t('factorSignin.backup.label')} value={backup} onChange={change('backup', setBackup)}
        error={errorText('backup')} required autoComplete="off" spellCheck={false} />
      <p role="status">{enrolling ? t('signin.key.working') : null}</p>
    </>);
  } else if (shown === 'noBackup' && noBackupKey !== null) {
    content = (
      <OnboardingFrame title={t('signin.key.title')} stepLabel={stepLabel(keyStep)} progress={progress(keyStep)} footer={cancelButton('signin.cancel')}>
        <Banner tone="danger">{t(noBackupKey, { instance: name })}</Banner>
      </OnboardingFrame>
    );
  } else if (shown === 'totp') {
    content = form(t('signin.totp.title'), 2, <>
      {cancelButton('signin.cancel')}
      <Button variant="accent" type="submit" {...blockedProps}>{t(busy === 'totp' ? 'signin.login.working' : 'signin.login.submit')}</Button>
    </>, <>
      {account.signIn?.factor === 'passkey' ? <p>{t('factorSignin.passkey.needsCode', { instance: name })}</p> : null}
      <p>{t('signin.totp.body')}</p>
      <TextField id={FIELD_ID.code} label={t('signin.totp.code')} value={code} onChange={change('code', setCode)}
        error={errorText('code')} required inputMode="numeric" autoComplete="one-time-code" spellCheck={false} />
      <Button variant="ghost" type="button" onClick={openBackup} {...blockedProps}>{t('factorSignin.useBackup')}</Button>
      <p>{t('factorSignin.lost', { instance: name })}</p>
      <p role="status">{enrolling ? t('signin.key.working') : null}</p>
    </>);
  } else if (shown === 'key') {
    const n = normaliseRecoveryKey(key).length;
    const short = n !== KEY_LENGTH;
    const working = busy === 'key' || busy === 'backup' || enrolling;
    // The text cannot change under a running enrolment.
    const onKey = change('key', setKey);
    content = form(t('signin.key.title'), keyStep, <>
      {cancelButton('signin.cancel')}
      <Button variant="accent" type="submit" {...(short || blocked ? { 'aria-disabled': true } : {})}
        aria-describedby={short ? LENGTH_ID : undefined}>{t('signin.key.submit')}</Button>
    </>, <>
      <p>{t('signin.key.body')}</p>
      <RecoveryKeyField id={FIELD_ID.key} label={t('signin.key.label')} value={key}
        onChange={value => { if (!blocked) { setNoteSettled(false); onKey(value); } }}
        onBlur={() => { if (looksBackup) setNoteSettled(true); }}
        describedBy={showBackup ? BACKUP_ID : undefined}
        hint={t('signin.key.hint', { n })} error={errorText('key')} />
      {showBackup ? <p id={BACKUP_ID}>{t('signIn.key.isBackupCode')}</p> : null}
      {short ? <p id={LENGTH_ID}>{t('signin.error.keyLength')}</p> : null}
      {/* A11Y-DESIGN-04: the live region is there before its text, so the text is announced when it arrives. */}
      <p role="status">{working ? t('signin.key.working') : null}</p>
      <p className="d-sr-only" aria-live="polite" data-testid="signin-announcer">{announcement}</p>
    </>);
  } else {
    // Step 1: the way out before anything is held is a new account (flow 03).
    content = form(t('signin.login.title', { instance: name }), 1, <>
      {cancelButton('signin.login.createInstead')}
      <Button variant="accent" type="submit" {...blockedProps}>{t(busy === 'login' ? 'signin.login.working' : 'signin.login.submit')}</Button>
    </>, <>
      <p>{t('signin.login.body', { instance: name })}</p>
      <TextField id={FIELD_ID.username} label={t('signin.login.username')} value={username}
        onChange={change('username', (value) => { setUsername(value); setKey(''); })} error={errorText('username')}
        required autoComplete="username" spellCheck={false} maxLength={32} />
      <TextField id={FIELD_ID.password} type="password" label={t('signin.login.password')} value={password}
        onChange={change('password', setPassword)} error={errorText('password')} required autoComplete="current-password" />
      {passkeysUsable(instance) ? (
        <Button id={FIELD_ID.passkey} variant="ghost" type="button" onClick={() => { submitPasskey('login'); }} {...blockedProps}>
          {t(busy === 'passkey' ? 'factorSignin.passkey.working' : 'factorSignin.usePasskey')}
        </Button>
      ) : null}
      <p role="status">{enrolling ? t('signin.key.working') : null}</p>
    </>);
  }
  return <div ref={rootRef}>{content}</div>;
}
