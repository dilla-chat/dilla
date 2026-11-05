import { useEffect, useRef, useState } from 'react';
import type { MutableRefObject } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { BACKUP_CODE, normaliseBackupCode, normaliseRecoveryKey } from '@dilla/client-core';
import { Banner, Button, CodeBoxes, Icon, OnboardingFrame, RecoveryKeyField, Splash, TextField, type IconName } from '@dilla/ui';
import { useCore } from '../core/context.tsx';
import { errorOf, type UiError } from '../core/errors.ts';
import { useSlice, useSlices } from '../core/use-slice.ts';
import { orderChannels } from './shell-model.ts';
import { t, type StringKey } from '../strings/index.ts';
import { PasskeyError, conditionalMediationAvailable, desktopAuthKind, getPasskey, passkeysUsable } from '../webauthn.ts';
import { desktopBridge } from '../desktop.ts';
import { mobileBridge } from '../mobile.ts';
import { useAnnouncer } from './announcer.ts';
import { doneProblem, type DoneProblem } from './factor/doneRefusal.ts';
import { FactorSetup } from './factor/FactorSetup.tsx';
import { ForgetBrowser } from './factor/ForgetBrowser.tsx';

const KEY_LENGTH = 52;
type Field = 'username' | 'password' | 'code' | 'key' | 'backup' | 'passkey';
type SignInCommand = 'oidc' | 'login' | 'totp' | 'key' | 'passkey' | 'backup';
type Shown = 'login' | 'totp' | 'key' | 'noBackup' | 'done' | 'passkey' | 'backup' | 'factor';
const FIELD_ID: Readonly<Record<Field, string>> = {
  username: 'signin-username', password: 'signin-password', code: 'signin-code', key: 'signin-key',
  backup: 'signin-backup', passkey: 'signin-passkey',
};
const FIELD_ORDER: readonly Field[] = ['username', 'password', 'code', 'backup', 'key'];
const LENGTH_ID = 'signin-key-length';
const CODE_BODY_ID = 'signin-code-body';
const CODE_ERROR_ID = 'signin-code-error';
const SIX_DIGITS = /^\d{6}$/;
/** The done panel's check (prototype: d-icon check, 16, stroke 3). */
const READY_ICON: IconName = 'check';
const BACKUP_ID = 'signin-key-backup';
/** How long the key field rests before a backup code is named (R44): a key being typed passes 26 characters on the way. */
const BACKUP_NOTE_MS = 600;

interface BannerState { tone: 'warn' | 'danger'; key: StringKey; vars: Record<string, string | number> }
interface RefusalView { field?: Field; fieldKey?: StringKey; focus?: Field; banner?: BannerState; noBackup?: true }

/** What a passkey ceremony that ended in the browser (cancelled, the wrong address, nothing usable) shows. */
const PASSKEY_FAILED: BannerState = { tone: 'danger', key: 'factorSignin.passkey.failed', vars: {} };
const PASSKEY_UNSUPPORTED: BannerState = { tone: 'danger', key: 'factorSignin.passkey.unsupported', vars: {} };
const PASSKEY_RATE_LIMITED: BannerState = { tone: 'danger', key: 'factorSignin.passkey.rateLimited', vars: {} };
const OIDC_UNSUPPORTED: BannerState = { tone: 'danger', key: 'factorSignin.oidc.unsupported', vars: {} };

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
  // Wave B task 3 (Jonas, M2): a passkey of another account than the password login on the code or passkey step; the
  // instance refused it and issued nothing. The step stays, with focus on the passkey control that started it.
  if (e.code === 'E_PASSKEY_OTHER_ACCOUNT' && command === 'passkey') {
    return { banner: { tone: 'danger', key: 'factorSignin.passkey.otherAccount', vars: {} }, focus: 'passkey' };
  }
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

/** Where "Open dilla" lands: the server and channel the done panel names. */
export interface SignInTarget { communityId: string; channelId: string }

/**
 * Wave B task 3 fix round 1 (Jonas): the login step offers passkeys in the username field's own autofill (WebAuthn
 * conditional mediation) in place of a "Sign in with a passkey" button. Mounted only while the login step shows: it asks
 * the instance for a ceremony (signInPasskeyBegin, the same discoverable options the button used) and leaves a
 * conditional request open; the person picking a passkey from the autofill resolves it. Unmounting (the step is left)
 * aborts it, and so does `abortRef` (the password is submitted). A browser without conditional mediation never starts
 * one: the password, then "Use a passkey" on the code step, is its way. Nothing here sends the assertion: the parent's
 * onCredential does, with the same guards as every other passkey ceremony.
 */
/** Re-check I1: a discoverable login ceremony lives 120 s on the instance, and an autofill request has no timeout of its
 *  own, so the request is renewed (aborted and begun again) this long after each begin, while the username field still
 *  has focus. An unfocused field renews nothing: no begin, so no rate-limit cost, while the page sits idle. */
export const CONDITIONAL_RENEW_MS = 110_000;

function ConditionalPasskey(props: {
  begin: () => Promise<{ ceremonyId: string; options: string }>;
  onCredential: (ceremonyId: string, response: string) => void;
  onFailed: () => void;
  abortRef: MutableRefObject<AbortController | null>;
  /** Whether a renewal is still wanted (the username field has focus). */
  stillWanted: () => boolean;
  /** No renewal: the request is left closed until the field is focused again. */
  onIdle: () => void;
}): null {
  const latest = useRef(props);
  useEffect(() => { latest.current = props; });
  useEffect(() => {
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let current: AbortController | null = null;
    const clearRenewal = () => { if (timer !== undefined) { clearTimeout(timer); timer = undefined; } };
    const round = () => {
      const abort = new AbortController();
      current = abort;
      latest.current.abortRef.current = abort;
      const go = async () => {
        if (!(await conditionalMediationAvailable()) || abort.signal.aborted) return;
        let began: { ceremonyId: string; options: string };
        try { began = await latest.current.begin(); } catch { return; }
        if (abort.signal.aborted) return;
        timer = setTimeout(() => {
          timer = undefined;
          // A request the page already ended (the password was sent, the step left) is not renewed.
          if (stopped || abort.signal.aborted) return;
          abort.abort();
          if (latest.current.stillWanted()) round();
          else latest.current.onIdle();
        }, CONDITIONAL_RENEW_MS);
        let response: string;
        try {
          response = await getPasskey(began.options, { signal: abort.signal });
        } catch (e) {
          // An aborted request (renewed, the password sent, the step left) and the browser's own cancel say nothing.
          if (abort.signal.aborted || (e instanceof PasskeyError && e.kind === 'cancelled')) return;
          clearRenewal();
          latest.current.onFailed();
          return;
        }
        clearRenewal();
        if (abort.signal.aborted) return;
        latest.current.onCredential(began.ceremonyId, response);
      };
      void go();
    };
    round();
    return () => {
      stopped = true;
      clearRenewal();
      current?.abort();
      if (latest.current.abortRef.current === current) latest.current.abortRef.current = null;
    };
  }, []);
  return null;
}

/** The four-step sign-in ceremony of L-COPY-02. Rendered by App for the phases signin-login, signin-totp,
 *  signin-passkey, signin-key and enrolling, and kept mounted after the phase turns ready until its done step calls
 *  onFinish. web3.8 (R6): the login first, then the second factor, then the recovery key. The login and the factor
 *  never register this browser: the worker holds the settled assertion, and the key's command registers it after the
 *  key's form check, back to back with the fetch, the enrolment and the list PUT (REGISTRATION-DEVICES-02). A backup
 *  code waits in page memory and goes only with the key (signInBackupCode), so an assertion that ran out meanwhile
 *  spends none. Phase signin-key is the key step, before or after the registration (a wrong key, a reload after it):
 *  its step sends signInKey. Step numbers follow R7. */
export function SignIn(props: { onFinish(target?: SignInTarget): void }): React.JSX.Element {
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
  // Wave B task 3 (Jonas, Q4): the done step's green panel names the account's first server and its first text channel.
  const communities = useSlice('communities');
  const firstServer = communities?.[0] ?? null;
  // Fix round 1: the channel list is read only when there is a server to read it for.
  const [firstChannels] = useSlices(firstServer === null ? [] : [`channels:${firstServer.id}` as const]);
  // The login step's conditional passkey request, aborted when the password is sent (fix round 1).
  const conditionalAbort = useRef<AbortController | null>(null);
  // Commit review fix: the autofill request starts only when the username field gains focus. Each start is a passkey
  // login begin, which costs the instance a stored ceremony (metered on its own passkey_begin bucket and an instance-wide
  // cap), so nothing starts on its own: not on showing the step, not after a refused password or passkey, until the field
  // is focused again.
  const [autofillArmed, setAutofillArmed] = useState(false);
  // Back is on its way to the worker; a second press sends nothing (fix round 1).
  const [goingBack, setGoingBack] = useState(false);
  // Fix round 2 (Jonas): whether this browser offers passkeys in the username autofill; null until known. Without it the
  // login step keeps the "Sign in with a passkey" button, the only way in for an account with no password.
  const [autofill, setAutofill] = useState<boolean | null>(null);
  const passkeysOn = passkeysUsable(account?.instance ?? null);
  useEffect(() => {
    if (!passkeysOn) return undefined;
    let live = true;
    void conditionalMediationAvailable().then((on) => { if (live) setAutofill(on); });
    return () => { live = false; };
  }, [passkeysOn]);
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
  const blocked = busy !== null || enrolling || goingBack;
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

  // The autofill request is armed again only by focusing the username field on a fresh login step.
  useEffect(() => { if (shown !== 'login') setAutofillArmed(false); }, [shown]);
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
  // Wave B task 3: the typed code lives only while its step shows; any other phase drops it (it is never stored).
  useEffect(() => { if (phase !== 'signin-totp' && phase !== 'enrolling') setCode(''); }, [phase]);
  // The prototype's code step opens on its boxes (its focusRef), so a one-time-code autofill offers itself at once; the
  // step's instruction is the field's description.
  useEffect(() => { if (shown === 'totp') setFocusRequest(f => ({ target: 'code', n: f.n + 1 })); }, [shown]);

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
    conditionalAbort.current?.abort();
    begin('login', 'login');
    // web3.8 (R6): the login carries no key; the factor step or the key step follows it.
    // A refused password keeps the login step; its autofill request (aborted above) starts again only when the username
    // field is focused again.
    setAutofillArmed(false);
    const call = client.call({ m: 'signInLogin', username: username.trim().toLowerCase(), password });
    run('login', call, () => {
      setPassword('');
    });
  };
  /** Wave B task 3: the code boxes keep digits only, and the sixth digit sends the code (onComplete) with no press. A
   *  code that is not six digits is never sent: a refused code spends the assertion and the person logs in again. */
  const submitTotp = (value: string = code) => {
    // A11Y-DESIGN-06: an authenticator may copy the code grouped ("123 456"); the spaces are not part of it.
    const trimmed = value.replace(/\s/g, '');
    if (showErrors(trimmed === '' ? { code: 'signin.error.required' } : SIX_DIGITS.test(trimmed) ? {} : { code: 'factor.error.codeFormat' })) return;
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
  const submitOIDC = () => {
    const bridge = mobileBridge() ?? desktopBridge();
    if (blocked || !bridge?.browserAuth) return;
    setBanner(null); setErrors({}); begin('oidc', 'login');
    const go = async () => {
      const result = await bridge.browserAuth!({ kind: 'oidc', options: '' }).catch((e: unknown) => {
        const kind = desktopAuthKind(e);
        if (kind === null) throw e;
        setBanner(kind === 'unsupported' ? OIDC_UNSUPPORTED : PASSKEY_RATE_LIMITED);
        return null;
      });
      if (result === null) return;
      if (!result.assertion || ![0, 1, 2].includes(result.needs)) throw new Error('Invalid browser sign-in');
      await client.call({ m: 'signInOIDC', assertion: result.assertion, needs: result.needs as 0 | 1 | 2 });
    };
    run('oidc', go(), () => { setPassword(''); });
  };
  /** The passkey on its own: the instance's options go to the browser and its answer back; the page makes no challenge. */
  const submitPasskey = (from: Shown) => {
    if (blocked) return;
    // A passkey signs in without the password: none stays in page memory.
    setPassword('');
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
          setBanner(e.kind === 'unsupported' ? PASSKEY_UNSUPPORTED : e.kind === 'rateLimited' ? PASSKEY_RATE_LIMITED : PASSKEY_FAILED);
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
  /** The login step's autofill passkey: sent exactly as the button's was, then the code (needs 1) or the key follows. A
   *  refusal shows its banner on the login and puts focus on the username field; a new request waits for the field to be
   *  focused again. The password typed so far is dropped at once: the passkey signs in without it. */
  const conditionalCredential = (ceremonyId: string, response: string) => {
    if (blocked) return;
    setPassword('');
    setBanner(null);
    setErrors({});
    setAutofillArmed(false);
    begin('passkey', 'login');
    void client.call({ m: 'signInPasskey', ceremonyId, response })
      .then(() => undefined, (e: unknown) => {
        apply(errorOf(e), 'passkey');
        requestFocus('username');
      })
      .finally(() => setBusy(null));
  };
  // A browser that failed the request (not a cancel) is not asked again until the step shows anew: no loop of ceremonies.
  const conditionalFailed = () => { setBanner(PASSKEY_FAILED); };
  // Fix round 1: the typed code belongs to the code form; opening or leaving the backup-code form drops it.
  const openBackup = () => { if (blocked) return; setBanner(null); setErrors({}); setCode(''); setBackupMode(true); requestFocus('backup'); };
  const closeBackup = () => { if (blocked) return; setBanner(null); setErrors({}); setCode(''); setBackupMode(false); setBackup(''); requestFocus('heading'); };
  // The factor was set up here: the worker reads the factors again, then the browser is ready.
  // Returns the call so FactorSetup knows when it ended: a refusal re-arms its button, which retries.
  const finishFactor = (): Promise<void> => {
    setFactorDone({ busy: true, problem: null });
    return client.call({ m: 'factorSetupDone', invite: null }).then(
      () => { setFactorDone({ busy: false, problem: null }); },
      (e: unknown) => { setFactorDone({ busy: false, problem: doneProblem(errorOf(e)) }); throw e; });
  };
  /** The sixth digit landed: the same path as Verify, so a running command (or the press that follows) sends nothing twice. */
  const completeCode = (value: string) => {
    if (blocked || shown !== 'totp') return;
    setBanner(null);
    setErrors({});
    submitTotp(value);
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
  /** Wave B task 3 (Jonas, Q1): Back on the code and key steps. The worker drops the held assertion (signInBack), so nothing
   *  half-signed-in survives; the page drops the typed code, key and any held backup code, and the login asks for the
   *  password again with the username kept. */
  const goBack = () => {
    if (blocked) return;
    setGoingBack(true);
    setPassword('');
    setBanner(null);
    setErrors({});
    setCode('');
    setKey('');
    setBackupHeld(false);
    setBackup('');
    client.call({ m: 'signInBack' })
      .then(() => { requestFocus('password'); }, (e: unknown) => { apply(errorOf(e), 'login'); })
      .finally(() => { setGoingBack(false); });
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
  // The prototype's row under the button on the code and key steps: Back at the left, the new-account link at the right
  // (signInCancel, which lands on the new-account screen, as Cancel did).
  const navRow = (
    <div className="d-onboarding-frame__nav">
      <Button variant="ghost" type="button" data-back="" onClick={goBack} {...blockedProps}>{t('factor.back')}</Button>
      <Button variant="ghost" type="button" onClick={cancel} {...blockedProps}>{t('signin.login.createInstead')}</Button>
    </div>
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
    const channel = firstChannels === undefined ? null : orderChannels(firstChannels).find(c => c.kind === 0 && c.mode === 0) ?? null;
    content = (
      <OnboardingFrame title={t('signin.done.title')} stepLabel={stepLabel(total)} progress={progress(total)}
        // Fix round 1: "Open dilla" lands where the panel says.
        footer={<Button variant="accent" type="button"
          onClick={() => props.onFinish(firstServer === null || channel === null ? undefined : { communityId: firstServer.id, channelId: channel.id })}>
          {t('signin.done.next')}</Button>}>
        <p>{t('signin.done.body', { username: who, instance: name })}</p>
        {firstServer === null || channel === null ? null : (
          <div className="d-onboarding-frame__ready">
            <span className="d-onboarding-frame__ready-mark"><Icon name={READY_ICON} size={16} stroke={3} /></span>
            <span>{t('signin.done.ready', { server: firstServer.name, channel: channel.name })}</span>
          </div>
        )}
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
    // Wave B task 3, the prototype's code step: six boxes over one input, then "Use a passkey" (for an account that just
    // used one, or whose password login said it holds one, in a browser that can run one) and "Use a backup code", which
    // open today's passkey ceremony and backup-code step.
    const passkeyLink = (account.signIn?.factor === 'passkey' || account.signIn?.passkey === true) && passkeysUsable(instance);
    const codeError = errorText('code');
    content = form(t('signin.totp.title'), 2, <>
      {navRow}
      <Button variant="accent" type="submit" {...(blocked || !SIX_DIGITS.test(code) ? { 'aria-disabled': true } : {})}>
        {t(busy === 'totp' ? 'signin.login.working' : 'signin.totp.submit')}
      </Button>
    </>, <>
      {account.signIn?.factor === 'passkey' ? <p>{t('factorSignin.passkey.needsCode', { instance: name })}</p> : null}
      <p id={CODE_BODY_ID}>{t('signin.totp.body')}</p>
      <CodeBoxes id={FIELD_ID.code} label={t('signin.totp.code')} value={code} onChange={change('code', setCode)}
        onComplete={completeCode} invalid={codeError !== undefined}
        describedBy={codeError === undefined ? CODE_BODY_ID : `${CODE_BODY_ID} ${CODE_ERROR_ID}`} />
      {codeError === undefined ? null : <p id={CODE_ERROR_ID} className="d-onboarding-frame__error" role="alert">{codeError}</p>}
      <div className="d-onboarding-frame__links">
        {passkeyLink ? (
          <Button id={FIELD_ID.passkey} variant="ghost" type="button" onClick={() => { submitPasskey('totp'); }} {...blockedProps}>
            {t(busy === 'passkey' ? 'factorSignin.passkey.working' : 'factorSignin.code.usePasskey')}
          </Button>
        ) : null}
        <Button variant="ghost" type="button" onClick={openBackup} {...blockedProps}>{t('factorSignin.useBackup')}</Button>
      </div>
      <p role="status">{enrolling ? t('signin.key.working') : null}</p>
    </>);
  } else if (shown === 'key') {
    const n = normaliseRecoveryKey(key).length;
    const short = n !== KEY_LENGTH;
    const working = busy === 'key' || busy === 'backup' || enrolling;
    // The text cannot change under a running enrolment.
    const onKey = change('key', setKey);
    content = form(t('signin.key.title'), keyStep, <>
      {navRow}
      <Button variant="accent" type="submit" {...(short || blocked ? { 'aria-disabled': true } : {})}
        aria-describedby={short ? LENGTH_ID : undefined}>{t('signin.key.submit')}</Button>
    </>, <>
      <p>{t('signin.key.body')}</p>
      <RecoveryKeyField id={FIELD_ID.key} label={t('signin.key.label')} value={key} placeholder={t('signin.key.placeholder')}
        onChange={value => { if (!blocked) { setNoteSettled(false); onKey(value); } }}
        onBlur={() => { if (looksBackup) setNoteSettled(true); }}
        describedBy={showBackup ? BACKUP_ID : undefined}
        hint={t('signin.key.hint', { n })} error={errorText('key')} />
      {showBackup ? <p id={BACKUP_ID}>{t('signIn.key.isBackupCode')}</p> : null}
      {short ? <p id={LENGTH_ID} className="d-onboarding-frame__note">{t('signin.error.keyLength')}</p> : null}
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
        required autoComplete={passkeysUsable(instance) ? 'username webauthn' : 'username'} spellCheck={false} maxLength={32}
        onFocus={() => { if (!blocked) setAutofillArmed(true); }} />
      <TextField id={FIELD_ID.password} type="password" label={t('signin.login.password')} value={password}
        onChange={change('password', setPassword)} error={errorText('password')} required autoComplete="current-password" />
      {instance?.oidc && (mobileBridge() ?? desktopBridge())?.browserAuth ? <Button variant="ghost" type="button" onClick={submitOIDC} {...blockedProps}>
        {t(busy === 'oidc' ? 'factorSignin.oidc.working' : 'factorSignin.oidc.submit')}
      </Button> : null}
      {/* Fix round 1 (Jonas): passkeys come from the username field's autofill; no button. Fix round 2: a browser without
          conditional mediation keeps the button (an accepted deviation for those browsers only). */}
      {passkeysUsable(instance) && autofillArmed ? (
        <ConditionalPasskey abortRef={conditionalAbort}
          begin={() => client.call({ m: 'signInPasskeyBegin' }) as Promise<{ ceremonyId: string; options: string }>}
          onCredential={conditionalCredential} onFailed={conditionalFailed}
          stillWanted={() => document.activeElement?.id === FIELD_ID.username} onIdle={() => { setAutofillArmed(false); }} />
      ) : null}
      {passkeysUsable(instance) && autofill === false ? (
        <Button id={FIELD_ID.passkey} variant="ghost" type="button" onClick={() => { submitPasskey('login'); }} {...blockedProps}>
          {t(busy === 'passkey' ? 'factorSignin.passkey.working' : 'factorSignin.usePasskey')}
        </Button>
      ) : null}
      <p role="status">{enrolling ? t('signin.key.working') : busy === 'passkey' ? t('factorSignin.passkey.working') : null}</p>
    </>);
  }
  return <div ref={rootRef}>{content}</div>;
}
