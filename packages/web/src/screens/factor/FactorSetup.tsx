// Flow 10: choosing and setting up a second factor. One component for the three places it is mounted: the last sign-up
// step, a sign-in on a new browser, and the forced screen for an account that owes one. There is no skip and no cancel in
// any origin; Back only returns to the choice. The TOTP secret, the otpauth URI and the backup codes come back from the
// commands and live in this component's state only: never a slice, the URL, storage or a log, and gone when it unmounts.
// No step-up is sent from the three origins: the instance lets an account that holds no factor add its first without one.
// Settings → Sign-in mounts it a fourth way, `embedded`: it renders its own Dialog (a title per step), starts at the
// app or the passkey step, has no choice and no Back, and sends the step-up token its host holds as `stepup`.
import { useEffect, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { Banner, Button, ChoiceCards, CodeList, Dialog, OnboardingFrame, QrCode, TextField } from '@dilla/ui';
import { browser } from '../../browser.ts';
import { useCore } from '../../core/context.tsx';
import { errorOf, type UiError } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { t, type StringKey } from '../../strings/index.ts';
import { PasskeyError, createPasskey, passkeysUsable } from '../../webauthn.ts';

export interface FactorSetupResult { kind: 'totp' | 'passkey'; /** The passkey's name, when one was just added here. */ name?: string }

export interface FactorSetupProps {
  origin: 'signup' | 'signin' | 'required' | 'settings';
  stepLabel: string;
  /** The flow's progress dots (Signal task 10 fix round 1); none in Settings. */
  progress?: { n: number; total: number };
  instanceName: string;
  username: string;
  announce: (text: string) => void;
  /** May return a promise: while it is pending, and once it resolved, a second press does nothing; if it rejects the button
   *  is armed again, so the same press retries. */
  onDone: (result: FactorSetupResult) => void | Promise<void>;
  /** Settings → Sign-in: the add-a-factor dialog. `stepup` is spent once by the begin; a refusal of it (403 E_STEP_UP, from
   *  whichever call) hands the person back to the host with `onStepUpLost`. `onClose` is Cancel, Escape and the close button. */
  embedded?: { stepup: string; start: 'app' | 'passkey'; onClose: () => void;
    /** The token is spent. 'expired': it was refused; 'again': the add ended without adding anything. */
    onStepUpLost: (why: 'expired' | 'again') => void;
    /** The page's one live region, shown in this dialog (the frame beneath is inert while it is open). */
    live?: ReactNode };
}

type Step = 'choose' | 'totp' | 'passkey' | 'codes' | 'done';
type Choice = 'app' | 'passkey';
interface Problem { key: StringKey; vars: Record<string, string | number>; n: number }

const SIX_DIGITS = /^\d{6}$/;
const NAME_MAX = 64;
const FIELD_CODE = 'factor-totp-code';
const FIELD_NAME = 'factor-passkey-name';
const BUTTON_PASSKEY = 'factor-passkey-submit';
const KIND_GROUP = 'factor-kind';

const NAME_MAX_BYTES = 64;
const CONTROL = /\p{Cc}/u;
const UTF8 = new TextEncoder();

/** The worker's rule for a passkey's name (1..64 characters and 64 UTF-8 bytes, no control character), checked before the
 *  authenticator makes a credential the instance would then refuse to register. */
const nameFits = (s: string): boolean => [...s].length <= NAME_MAX && UTF8.encode(s).length <= NAME_MAX_BYTES && !CONTROL.test(s);

/** A finished set-up the worker refused because the account still owes a factor (its read of the factors found none). */
export const stillOwed = (e: unknown): boolean => errorOf(e).code === 'E_FACTOR_REQUIRED';

/** The refusals every request shares, by status and code only (never by detail). Null: the caller's own rows apply. */
export function sharedRefusal(e: UiError): Pick<Problem, 'key' | 'vars'> | null {
  if (e.status === 429) return { key: 'factor.error.tooMany', vars: { seconds: Math.max(1, Math.ceil((e.retryAfterMs ?? 1000) / 1000)) } };
  if (e.status === 0 && (e.code === 'E_NETWORK' || e.code === 'E_UNAVAILABLE')) return { key: 'factor.error.network', vars: {} };
  return null;
}

export function FactorSetup(props: FactorSetupProps): React.JSX.Element {
  const { origin, stepLabel, progress, instanceName, username, announce, onDone, embedded } = props;
  const client = useCore();
  const account = useSlice('account');
  const factors = useSlice('factors');
  const hasPassword = factors?.hasPassword ?? true;
  const usable = passkeysUsable(account?.instance);

  const [step, setStep] = useState<Step>(embedded === undefined ? 'choose' : embedded.start === 'app' ? 'totp' : 'passkey');
  const [choice, setChoice] = useState<Choice>('app');
  const [problem, setProblem] = useState<Problem | null>(null);
  const [busy, setBusy] = useState(false);
  const [key, setKey] = useState<{ secret: string; uri: string } | null>(null);
  const [code, setCode] = useState('');
  const [codeError, setCodeError] = useState<StringKey | null>(null);
  const [name, setName] = useState<string | null>(null);
  const [nameError, setNameError] = useState<StringKey | null>(null);
  const [codes, setCodes] = useState<string[]>([]);
  const [acknowledged, setAcknowledged] = useState(false);
  const [kind, setKind] = useState<'totp' | 'passkey'>('totp');
  const [focusRequest, setFocusRequest] = useState<{ id: string; n: number } | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const mounted = useRef(true);
  // A request made for a screen the person has left (Back, or a newer request) must not write into the new one.
  const generation = useRef(0);
  const completed = useRef(false);
  // Set once this mount has added a factor itself: only a factor found already in place (a reload) ends the set-up unasked.
  const addedHere = useRef(false);

  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);
  const factorsKnown = factors !== undefined;
  useEffect(() => {
    if (!factorsKnown) client.call({ m: 'refreshFactors' }).catch(() => undefined);
  }, [client, factorsKnown]);
  // Embedded: the app's begin is sent once per mount (a second would find the token spent), and each step's title takes focus.
  const started = useRef(false);
  useEffect(() => {
    if (embedded?.start !== 'app' || started.current) return;
    started.current = true;
    beginTotp();
    // beginTotp reads props and refs; this runs once.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  useEffect(() => {
    if (embedded === undefined) return;
    const title = rootRef.current?.querySelector<HTMLElement>('.d-dialog__title');
    title?.setAttribute('tabindex', '-1');
    title?.focus();
    // The title follows the step only.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step]);
  useEffect(() => {
    if (focusRequest !== null) rootRef.current?.querySelector<HTMLElement>(`#${focusRequest.id}`)?.focus();
  }, [focusRequest]);

  // A reload after a factor was confirmed but before the set-up ended: the factor is in place, so it is not offered again.
  // The backup codes shown before the reload are gone; Settings, under sign-in, makes new ones.
  // Only a slice that says the account owes nothing is trusted: one that still says owed (an operator reset, read before
  // the worker's fresh read) shows the choice. When in doubt, the choice.
  const held = factors !== undefined && factors.owed === false && (factors.totp !== null || factors.passkeys.length > 0);
  const heldKind: 'totp' | 'passkey' = factors?.totp != null ? 'totp' : 'passkey';
  useEffect(() => {
    if (step === 'choose' && held && !addedHere.current && !completed.current) {
      addedHere.current = true;
      finish(heldKind);
    }
    // finish reads only props and refs.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, held, heldKind]);
  // Walk I1: the forced screen left open while another browser added the factor. When the window or the tab comes back to
  // the choice, the factors are read once (never polled); a read that finds the account owes nothing ends the set-up above.
  // Focus and visibility often come together: a read still out takes the second.
  const reading = useRef(false);
  useEffect(() => {
    if (origin !== 'required' || step !== 'choose') return;
    const reread = () => {
      if (reading.current || document.visibilityState === 'hidden') return;
      reading.current = true;
      void client.call({ m: 'refreshFactors' }).catch(() => undefined).then(() => { reading.current = false; });
    };
    window.addEventListener('focus', reread);
    document.addEventListener('visibilitychange', reread);
    return () => {
      window.removeEventListener('focus', reread);
      document.removeEventListener('visibilitychange', reread);
    };
  }, [client, origin, step]);

  const requestFocus = (id: string) => setFocusRequest(f => ({ id, n: (f?.n ?? 0) + 1 }));
  /** A refused step-up token (spent, expired, from another session): the host asks for a new one. True when it was one. */
  const stepUpLost = (e: UiError): boolean => {
    if (embedded === undefined || e.status !== 403 || e.code !== 'E_STEP_UP') return false;
    embedded.onStepUpLost('expired');
    return true;
  };
  /** The set-up origins send no step-up, so a 403 E_STEP_UP there means the account now holds a factor (another device
   *  added one). The factors are read again and the choice shows, which ends the set-up as already in place when the
   *  account owes nothing. True when it was one. */
  const factorAppeared = (e: UiError): boolean => {
    if (embedded !== undefined || e.status !== 403 || e.code !== 'E_STEP_UP') return false;
    const g = generation.current;
    void client.call({ m: 'refreshFactors' }).catch(() => undefined).then(() => { if (live(g) && !completed.current) toChoose(); });
    return true;
  };
  /** Embedded: the begin spent the token and the add did not go on, so a retry here would only be refused. */
  const handBack = (): boolean => {
    if (embedded === undefined) return false;
    embedded.onStepUpLost('again');
    return true;
  };
  const addedName = useRef<string | undefined>(undefined);
  /** Embedded: a passkey begin has spent the step-up token. */
  const tokenSpent = useRef(false);
  const fail = (shown: Pick<Problem, 'key' | 'vars'>) => setProblem(p => ({ ...shown, n: (p?.n ?? 0) + 1 }));
  const live = (g: number): boolean => mounted.current && generation.current === g;

  // Without a password the only factor on offer is a passkey.
  const effective: Choice = hasPassword ? choice : 'passkey';
  const passkeyName = name ?? t('factor.passkey.defaultName', { n: (factors?.passkeys.length ?? 0) + 1 });

  const goTo = (next: Step) => {
    generation.current += 1;
    setProblem(null);
    setBusy(false);
    setStep(next);
  };
  const toChoose = () => {
    setKey(null);
    setCode('');
    setCodeError(null);
    setNameError(null);
    goTo('choose');
  };
  /** The factor is in place: show the codes, or skip them when the account already held some. */
  const added = (which: 'totp' | 'passkey', backupCodes: string[]) => {
    addedHere.current = true;
    setKey(null);
    setKind(which);
    setCode('');
    setCodes(backupCodes);
    setBusy(false);
    generation.current += 1;
    if (backupCodes.length > 0) { setStep('codes'); return; }
    finish(which);
  };
  const finish = (which: 'totp' | 'passkey') => {
    setKind(which);
    // Without a done step the codes stay on screen until onDone has succeeded: a refused finish is retried from them.
    if (origin !== 'required') { complete(which); return; }
    setCodes([]);
    setStep('done');
    announce(t('factor.added'));
  };
  /** onDone runs once at a time, however often Enter or the button is pressed, and not again after it succeeded. */
  const complete = (which: 'totp' | 'passkey') => {
    if (completed.current) return;
    completed.current = true;
    let pending: void | Promise<void>;
    try { pending = onDone(which === 'passkey' && addedName.current !== undefined ? { kind: which, name: addedName.current } : { kind: which }); } catch (e) { completed.current = false; throw e; }
    if (pending === undefined) { setCodes([]); return; }
    pending.then(() => { if (mounted.current) setCodes([]); }, (e: unknown) => {
      completed.current = false;
      // The instance still finds the account owing (its factor is gone): back to the choice, which says so.
      if (stillOwed(e) && mounted.current) {
        addedHere.current = false;
        setCodes([]);
        setAcknowledged(false);
        toChoose();
      }
    });
  };

  const beginTotp = () => {
    goTo('totp');
    setBusy(true);
    const g = generation.current;
    client.call({ m: 'factorTotpBegin', stepup: embedded?.stepup ?? null }).then((v) => {
      if (!live(g)) return;
      const r = v as { secret: string; uri: string };
      setKey({ secret: r.secret, uri: r.uri });
      setBusy(false);
    }).catch((e: unknown) => {
      if (!live(g)) return;
      const err = errorOf(e);
      if (stepUpLost(err) || factorAppeared(err) || handBack()) return;
      fail(sharedRefusal(err) ?? { key: 'factor.error.other', vars: { code: err.code } });
      setBusy(false);
    });
  };

  const submitTotp = () => {
    if (busy || key === null) return;
    const digits = code.replace(/\s/g, '');
    if (digits === '') { setCodeError('factor.error.required'); requestFocus(FIELD_CODE); return; }
    if (!SIX_DIGITS.test(digits)) { setCodeError('factor.error.codeFormat'); requestFocus(FIELD_CODE); return; }
    setCodeError(null);
    setProblem(null);
    setBusy(true);
    const g = generation.current;
    client.call({ m: 'factorTotpConfirm', code: digits }).then((v) => {
      if (!live(g)) return;
      added('totp', (v as { backupCodes: string[] }).backupCodes);
    }).catch((e: unknown) => {
      if (!live(g)) return;
      const err = errorOf(e);
      if (stepUpLost(err) || factorAppeared(err)) return;
      setBusy(false);
      if (err.status === 400 && err.code === 'E_INVALID_REQUEST') {
        setCode('');
        setCodeError('factor.error.code');
        requestFocus(FIELD_CODE);
        return;
      }
      fail(sharedRefusal(err) ?? { key: 'factor.error.other', vars: { code: err.code } });
    });
  };

  const submitPasskey = () => {
    if (busy) return;
    // Embedded: an earlier begin spent the token (the browser then ended the ceremony), so this try needs a new one.
    if (tokenSpent.current && handBack()) return;
    const trimmed = passkeyName.trim();
    if (trimmed === '') { setNameError('factor.error.required'); requestFocus(FIELD_NAME); return; }
    if (!nameFits(trimmed)) { setNameError('factor.error.nameLong'); requestFocus(FIELD_NAME); return; }
    setNameError(null);
    setProblem(null);
    setBusy(true);
    addedName.current = trimmed;
    const g = generation.current;
    const run = async () => {
      try {
        tokenSpent.current = embedded !== undefined;
        const begin = await client.call({ m: 'factorPasskeyBegin', stepup: embedded?.stepup ?? null }) as { ceremonyId: string; options: string };
        if (!live(g)) return;
        // The options come from the instance's begin and nowhere else; the page makes no challenge of its own.
        const response = await createPasskey(begin.options);
        if (!live(g)) return;
        const done = await client.call({ m: 'factorPasskeyFinish', ceremonyId: begin.ceremonyId, response, name: trimmed }) as { backupCodes: string[] };
        if (!live(g)) return;
        added('passkey', done.backupCodes);
      } catch (e) {
        if (!live(g)) return;
        if (stepUpLost(errorOf(e)) || factorAppeared(errorOf(e))) return;
        // Walk M7 (flow 10 state 11): the browser ending the ceremony (cancelled, wrong address, already there) is a banner
        // in this dialog with focus on Add passkey; the next press asks for the password. The instance's refusals hand back.
        if (!(e instanceof PasskeyError) && handBack()) return;
        setBusy(false);
        fail(passkeyProblem(e, instanceName));
        requestFocus(BUTTON_PASSKEY);
      }
    };
    void run();
  };

  const copy = (text: string, announced: StringKey) => {
    navigator.clipboard?.writeText(text).then(() => { announce(t(announced)); }, () => undefined);
  };

  const onSubmit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    switch (step) {
      case 'choose':
        if (effective === 'app') beginTotp();
        else if (usable) { goTo('passkey'); }
        break;
      case 'totp': submitTotp(); break;
      case 'passkey': submitPasskey(); break;
      case 'codes': if (acknowledged) finish(kind); break;
      case 'done': complete(kind); break;
    }
  };

  const banner = problem === null ? null : (
    <Banner key={problem.n} tone="danger">{t(problem.key, problem.vars)}</Banner>
  );
  const blocked = busy ? { 'aria-disabled': true as const } : {};
  const frame = (title: string, foot: ReactNode, children: ReactNode) => embedded === undefined ? (
    <form noValidate onSubmit={onSubmit}>
      <OnboardingFrame title={title} stepLabel={stepLabel} progress={progress} footer={foot}>
        {banner}
        {children}
      </OnboardingFrame>
    </form>
  ) : (
    <form noValidate onSubmit={onSubmit}>
      <Dialog open title={title} closeLabel={t(step === 'codes' ? 'dialog.close' : 'factorSettings.cancel')} onClose={embedded.onClose} footer={foot}>
        <div className="dw-factor-embedded">
          {banner}
          {children}
          {embedded.live}
        </div>
      </Dialog>
    </form>
  );
  // Back always returns to the choice, even with a request out: the step's generation moves, so a late answer is dropped.
  const back = embedded !== undefined ? null
    : <Button variant="ghost" type="button" onClick={toChoose}>{t('factor.back')}</Button>;

  let content: React.JSX.Element;
  if (step === 'choose') {
    const options = [
      ...(hasPassword ? [{ id: 'app', title: t('factor.choose.app'), hint: t('factor.choose.appHint') }] : []),
      { id: 'passkey', title: t('factor.choose.passkey'), hint: t('factor.choose.passkeyHint'),
        ...(usable ? {} : { disabled: true, reason: t('factor.choose.passkeyOff') }) },
    ];
    const cannot = effective === 'passkey' && !usable;
    content = frame(t('factor.choose.title'),
      <Button variant="accent" type="submit" {...(cannot ? { 'aria-disabled': true } : {})}>{t('factor.choose.next')}</Button>, <>
        <p>{origin === 'required' ? t('factor.choose.required', { instance: instanceName }) : t('factor.choose.body', { instance: instanceName })}</p>
        {hasPassword ? null : <p>{t('factor.choose.noPassword')}</p>}
        <ChoiceCards name={KIND_GROUP} label={t('factor.choose.label')} options={options} value={effective}
          onChange={(id) => { setChoice(id === 'passkey' ? 'passkey' : 'app'); }} />
      </>);
  } else if (step === 'totp') {
    content = frame(t('factor.totp.title'),
      <>
        {back}
        <Button variant="accent" type="submit" {...(busy || key === null ? { 'aria-disabled': true } : {})}>
          {t(busy && key !== null ? 'factor.totp.working' : 'factor.totp.submit')}
        </Button>
      </>,
      key === null ? (busy ? <p role="status">{t('factor.totp.loading')}</p> : null) : <>
        <p>{t('factor.totp.body')}</p>
        <div className="dw-factor-key">
          <QrCode value={key.uri} label={t('factor.totp.qr', { username, instance: instanceName })} />
          <div className="dw-factor-key__text">
            {/* Focus bubbles from the field; only an input is selected (the copy button is not). */}
            <div className="dw-factor-key__field" onFocus={(e) => { if (e.target instanceof HTMLInputElement) e.target.select(); }}>
              <TextField id="factor-totp-key" label={t('factor.totp.key')} value={key.secret} onChange={() => {}} readOnly
                spellCheck={false} autoComplete="off" />
              <Button type="button" onClick={() => { copy(key.secret, 'factor.totp.keyCopied'); }}>{t('factor.totp.copyKey')}</Button>
            </div>
          </div>
        </div>
        <TextField id={FIELD_CODE} label={t('factor.totp.code')} hint={t('factor.totp.codeHint')} value={code}
          onChange={(v) => { setCode(v); setCodeError(null); }} {...(codeError === null ? {} : { error: t(codeError) })}
          autoComplete="one-time-code" inputMode="numeric" spellCheck={false} required />
      </>);
  } else if (step === 'passkey') {
    content = frame(t('factor.passkey.title'),
      <>
        {back}
        <Button id={BUTTON_PASSKEY} variant="accent" type="submit" {...blocked}>{t(busy ? 'factor.passkey.working' : 'factor.passkey.submit')}</Button>
      </>, <>
        <p>{t('factor.passkey.body')}</p>
        <TextField id={FIELD_NAME} label={t('factor.passkey.name')} hint={t('factor.passkey.nameHint')} value={passkeyName}
          onChange={(v) => { setName(v); setNameError(null); }} {...(nameError === null ? {} : { error: t(nameError) })}
          autoComplete="off" required />
      </>);
  } else if (step === 'codes') {
    content = frame(t('factor.codes.title'),
      <Button variant="accent" type="submit"
        {...(acknowledged ? {} : { 'aria-disabled': true as const, 'aria-describedby': 'factor-ack-hint' })}>{t('factor.codes.next')}</Button>, <>
        <p>{t('factor.codes.body')}</p>
        <p>{t('factor.codes.notKey')}</p>
        <div className="dw-factor-codes">
          <CodeList codes={codes} label={t('factor.codes.label')} labels={{ copy: t('factor.codes.copy'), print: t('factor.codes.print') }}
            onCopy={() => { copy(codes.join('\n'), 'factor.codes.copied'); }} onPrint={() => { browser.print(); }} />
        </div>
        <div className="dw-factor-ack">
          <input id="factor-ack" type="checkbox" checked={acknowledged} onChange={(e) => { setAcknowledged(e.currentTarget.checked); }} />
          {' '}
          <label htmlFor="factor-ack">{t('factor.codes.ack')}</label>
        </div>
        {acknowledged ? null : <p id="factor-ack-hint">{t('factor.codes.ackHint')}</p>}
      </>);
  } else {
    content = frame(t('factor.done.title'), <Button variant="accent" type="submit">{t('factor.done.next')}</Button>,
      <p>{t(kind === 'totp' ? 'factor.done.totp' : 'factor.done.passkey')}</p>);
  }
  return <div ref={rootRef}>{content}</div>;
}

/** What a failed passkey ceremony shows: the browser's own end by kind, the instance's refusals by status and code. */
function passkeyProblem(e: unknown, instanceName: string): Pick<Problem, 'key' | 'vars'> {
  if (e instanceof PasskeyError) {
    switch (e.kind) {
      case 'cancelled': return { key: 'factor.error.cancelled', vars: {} };
      case 'wrongAddress': return { key: 'factor.error.wrongAddress', vars: { instance: instanceName } };
      case 'exists': return { key: 'factor.error.exists', vars: {} };
      default: return { key: 'factor.error.other', vars: { code: 'E_PASSKEY' } };
    }
  }
  const err = errorOf(e);
  const shared = sharedRefusal(err);
  if (shared !== null) return shared;
  if (err.status >= 400 && err.status < 500) return { key: 'factor.error.refused', vars: { instance: instanceName } };
  return { key: 'factor.error.other', vars: { code: err.code } };
}
