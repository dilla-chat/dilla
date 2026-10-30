import { useEffect, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { Banner, Button, CodeList, Icon, OnboardingFrame, RecoveryKey, Splash, TextField, type IconName } from '@dilla/ui';
import { browser } from '../browser.ts';
import { useCore } from '../core/context.tsx';
import { errorOf, type UiError } from '../core/errors.ts';
import { useSlice, useSlices } from '../core/use-slice.ts';
import { extractInviteCode } from '../invite.ts';
import { useRoute } from '../router.ts';
import { t, type StringKey } from '../strings/index.ts';
import { useAnnouncer } from './announcer.ts';
import { orderChannels } from './shell-model.ts';
import { doneProblem, type DoneProblem } from './factor/doneRefusal.ts';
import { FactorSetup, type FactorSetupResult } from './factor/FactorSetup.tsx';
import { ForgetBrowser } from './factor/ForgetBrowser.tsx';

/** What the done step hands to the App: the value signupSubmit resolved with and the invite code sent. */
export interface SignupResult { communityId: string | null; joinError: UiError | null; invite: string }

type Step = 'connect' | 'identity' | 'keys';
type Field = 'invite' | 'username' | 'password';
type FocusTarget = 'heading' | 'onboarding-invite' | 'onboarding-username' | 'onboarding-password';
interface BannerState { tone: 'warn' | 'danger'; key: StringKey; vars: Record<string, string | number>; reload: boolean }
interface RefusalView {
  step?: 'connect' | 'identity';
  field?: 'invite' | 'username';
  fieldKey?: StringKey;
  closed?: true;
  banner?: BannerState;
  focus?: 'heading' | 'onboarding-invite' | 'onboarding-username';
}

// internal/auth/handle.go: 3..32 of a-z 0-9 . _ - after case mapping, no leading or trailing dot.
const USERNAME = /^(?!\.)[a-z0-9._-]{3,32}(?<!\.)$/;
// Jonas, 2026-10-30: a new password has at least 12 characters (code points); the instance refuses fewer (protocol/09).
const PASSWORD_MIN = 12;
const COMMUNITY_ID = /^[0-9a-f]{32}$/;
const FIELD_ORDER: readonly Field[] = ['invite', 'username', 'password'];
const TOTAL = 6;
const READY_ICON: IconName = 'check';

const codePoints = (s: string): number => [...s].length;

// L-TS-27, head ruling 26: after a list race the wipe reloads the page to /welcome?signin=race, and the
// query is the only place the message survives that reload.
const RACE_BANNER: BannerState = { tone: 'warn', key: 'signin.error.listRace', vars: {}, reload: false };
// REGISTRATION-DEVICES-02: the sign-in's row was replaced after the enrolment was written; the wipe reloads to
// /welcome?signin=evicted.
const EVICTED_BANNER: BannerState = { tone: 'danger', key: 'signin.error.evicted', vars: {}, reload: false };

/**
 * L-COPY-01's refusal table, in its order. It reads the code, the HTTP status and the wait, and
 * never the server's detail text (protocol/02: a client must not parse it).
 */
function refusalView(e: UiError, instanceName: string): RefusalView {
  if (e.code === 'E_INVITE_INVALID') {
    return { step: 'connect', field: 'invite', fieldKey: 'onboarding.error.inviteInvalid', focus: 'onboarding-invite' };
  }
  if (e.code === 'E_INVALID_REQUEST' && e.status === 409) {
    return { step: 'identity', field: 'username', fieldKey: 'onboarding.error.usernameTaken', focus: 'onboarding-username' };
  }
  if (e.code === 'E_INVALID_REQUEST') {
    return {
      step: 'identity', focus: 'heading',
      banner: { tone: 'danger', key: 'onboarding.error.detailsRefused', vars: { instance: instanceName }, reload: false },
    };
  }
  if (e.code === 'E_FORBIDDEN') return { closed: true };
  if (e.code === 'E_RATE_LIMITED') {
    return {
      banner: {
        tone: 'warn',
        key: e.retryAfterMs !== null ? 'onboarding.error.rateLimited' : 'onboarding.error.rateLimitedNoWait',
        vars: e.retryAfterMs !== null ? { seconds: Math.ceil(e.retryAfterMs / 1000) } : {},
        reload: false,
      },
    };
  }
  if (e.code === 'E_NETWORK' || e.status >= 500) {
    return { banner: { tone: 'danger', key: 'onboarding.error.network', vars: {}, reload: true } };
  }
  return { banner: { tone: 'danger', key: 'onboarding.error.other', vars: { code: e.code }, reload: false } };
}

/** The value signupSubmit resolved with, as a SignupResult; anything malformed reads as no server and no error. */
function resultOf(value: unknown, invite: string): SignupResult {
  const v = typeof value === 'object' && value !== null ? (value as Record<string, unknown>) : {};
  const communityId = typeof v.communityId === 'string' && COMMUNITY_ID.test(v.communityId) ? v.communityId : null;
  const joinError = v.joinError === null || v.joinError === undefined ? null : errorOf(v.joinError);
  return { communityId, joinError, invite };
}

/**
 * The signup ceremony (wave B task 3b, the prototype's six steps): 1 invite, 2 account (Create account makes the keys),
 * 3 recovery key (I saved it, once ticked, creates the account on the instance), 4 the second factor's choice, 5 its
 * code, 6 done (the backup codes, ticked before Open dilla). The account is created only after the key is confirmed, so
 * a page closed at any step before that leaves no account (the reload resets the pending keys), and one closed after it
 * reopens on the forced second-factor screen, as before. A shown key is never offered again: Back from the key step and
 * a refused creation discard it (signupReset), and the next Create account makes a new one.
 */
export function Onboarding(props: { onFinish(result: SignupResult): void }): React.JSX.Element {
  const client = useCore();
  const account = useSlice('account');
  const communities = useSlice('communities');
  const [route, navigate] = useRoute();
  const [step, setStep] = useState<Step>('connect');
  const [invite, setInvite] = useState(() => (route.name === 'welcome' ? route.invite ?? '' : ''));
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [errors, setErrors] = useState<Partial<Record<Field, StringKey>>>({});
  const [banner, setBanner] = useState<BannerState | null>(
    () => (route.name === 'welcome' && route.signin === 'race' ? RACE_BANNER
      : route.name === 'welcome' && route.signin === 'evicted' ? EVICTED_BANNER : null));
  const [acknowledged, setAcknowledged] = useState(false);
  const [beginning, setBeginning] = useState(false);
  const [entering, setEntering] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [submitted, setSubmitted] = useState(false);
  // A shown key is being discarded (Back from the key step, or a refused creation).
  const [discarding, setDiscarding] = useState(false);
  const [closed, setClosed] = useState(false);
  // Set by the E_NETWORK / 5xx row of a refused signupSubmit: the account may exist already, so the
  // page never sends signupSubmit again and Reload (Signup.resume in the worker) is the only way on.
  const [stalled, setStalled] = useState(false);
  const [result, setResult] = useState<SignupResult | null>(null);
  // web3.6: the account was created and now owes its second factor (steps 4 and 5); done (6) waits for the factor.
  const [viaFactor, setViaFactor] = useState(false);
  const [factorResult, setFactorResult] = useState<SignupResult | null>(null);
  const [factorState, setFactorState] = useState<{ busy: boolean; problem: DoneProblem | null }>({ busy: false, problem: null });
  // Task 3b: the backup codes the factor's add returned, shown on the done step. They live in this component's state only
  // (never a slice, the URL, storage or a log) and are gone when it unmounts.
  const [codes, setCodes] = useState<string[]>([]);
  const [codesSaved, setCodesSaved] = useState(false);
  const { text: announcement, announce } = useAnnouncer();
  const [focusRequest, setFocusRequest] = useState<{ target: FocusTarget; n: number }>({ target: 'heading', n: 0 });
  const rootRef = useRef<HTMLDivElement>(null);

  const instance = account?.instance ?? null;
  const ready = account?.phase === 'ready';
  const inFactor = account?.phase === 'factor-setup' && account.factorSetup?.origin === 'signup';
  useEffect(() => { if (inFactor) setViaFactor(true); }, [inFactor]);
  const factorFlow = viaFactor || inFactor;
  // A page reloaded during registration mounts this screen with nothing submitted; the worker's
  // resume then finishes the account, and the ceremony ends at done (pre-flight ruling (f)).
  const finished: SignupResult | null = factorFlow ? factorResult
    : result ?? (ready && !submitting && !submitted ? { communityId: null, joinError: null, invite: extractInviteCode(invite) } : null);
  const shown = factorFlow && finished === null && (inFactor || ready) ? 'factor'
    : ready && finished !== null ? 'done'
      : (closed || instance?.registrationMode === 2) ? 'closed' : step;
  const busy = submitting || (submitted && !ready);
  const blocked = busy || beginning || stalled || discarding;
  // The done step's panel (the prototype's): the server the invite joined and its first text channel, read only when
  // there is one.
  const joinedId = shown === 'done' ? finished?.communityId ?? null : null;
  const joined = joinedId === null ? null : communities?.find(c => c.id === joinedId) ?? null;
  const [joinedChannels] = useSlices(joined === null ? [] : [`channels:${joined.id}` as const]);

  // OnboardingFrame focuses its heading when the title changes; this effect runs after the frame's
  // (child effects first), so a field request after a step change lands on the field.
  useEffect(() => {
    const root = rootRef.current;
    if (!root) return;
    if (focusRequest.target === 'heading') root.querySelector('h1')?.focus();
    else root.querySelector<HTMLElement>(`#${focusRequest.target}`)?.focus();
  }, [shown, focusRequest]);

  if (account === undefined || instance === null) return <Splash status={t('boot.loading.status')} />;
  const name = instance.name;

  const requestFocus = (target: FocusTarget) => setFocusRequest(f => ({ target, n: f.n + 1 }));
  const goTo = (next: Step) => { setStep(next); requestFocus('heading'); };
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
    requestFocus(`onboarding-${first}`);
    return true;
  };
  const applyRefusal = (view: RefusalView) => {
    if (view.closed) setClosed(true);
    if (view.step) setStep(view.step);
    if (view.field && view.fieldKey) setErrors({ [view.field]: view.fieldKey });
    setBanner(view.banner ?? null);
    if (view.focus) requestFocus(view.focus);
  };
  /** The shown key is never offered again: the worker forgets the pending keys (signupReset) and the tick goes with them.
   *  A failed reset leaves the keys pending; the next Create account resets them before it makes new ones. */
  const discardKeys = async () => {
    setAcknowledged(false);
    try { await client.call({ m: 'signupReset' }); } catch { /* the next Create account resets first */ }
  };

  // The first navigation away from the connect step writes the route without the race query.
  const dropRace = () => {
    if (route.name === 'welcome' && route.signin !== null) void navigate({ name: 'welcome', invite: route.invite, signin: null }, true);
  };
  const connectNext = () => {
    setBanner(null);
    const code = extractInviteCode(invite);
    if (showErrors(instance.registrationMode === 0 && code === '' ? { invite: 'onboarding.error.inviteRequired' } : {})) return;
    dropRace();
    goTo('identity');
  };
  // Ruling 22: the entry to the sign-in ceremony, the prototype's "Already have an account? Sign in". On the key step it
  // discards the shown key first: a sign-in starts only from a browser that holds no pending keys.
  const beginSignIn = () => {
    if (entering || blocked) return;
    setBanner(null);
    dropRace();
    setEntering(true);
    const run = async () => {
      if (shown === 'keys') await discardKeys();
      await client.call({ m: 'signInBegin' });
    };
    run()
      .catch((e: unknown) => { setBanner({ tone: 'danger', key: 'signin.error.other', vars: { code: errorOf(e).code }, reload: false }); })
      .finally(() => setEntering(false));
  };
  // Step 2's Create account makes the keys, a new key each time: keys an earlier attempt left pending are reset first, so a
  // key that was shown is never shown again. The account itself is created on step 3.
  const identityNext = () => {
    setBanner(null);
    const next: Partial<Record<Field, StringKey>> = {};
    if (!USERNAME.test(username.trim().toLowerCase())) next.username = 'onboarding.error.usernameRule';
    if (instance.passwordSignup && codePoints(password) < PASSWORD_MIN) next.password = 'onboarding.error.passwordShort';
    if (showErrors(next)) return;
    setBeginning(true);
    const run = async () => {
      try {
        if (account.phase === 'signup-keys') await client.call({ m: 'signupReset' });
        await client.call({ m: 'signupBegin' });
        setBeginning(false);
        setAcknowledged(false);
        goTo('keys');
      } catch (e) {
        setBeginning(false);
        applyRefusal(refusalView(errorOf(e), name));
      }
    };
    void run();
  };
  // Step 3's "I saved it": only once the key is ticked, and only then is the account created on the instance.
  const keysNext = () => {
    if (!acknowledged || account.recoveryKey === null) return;
    setBanner(null);
    const code = extractInviteCode(invite);
    setSubmitting(true);
    const run = async () => {
      try {
        // Jonas (task 3b): no display name at sign-up. The instance stores none and the username is shown until the
        // person sets one in Settings → Profile.
        const value = await client.call({
          m: 'signupSubmit', invite: code, username: username.trim().toLowerCase(), display: '',
          password: instance.passwordSignup ? password : null, recoveryKeyAcknowledged: true,
        });
        const parsed = resultOf(value, code);
        if (typeof value === 'object' && value !== null && (value as { factorOwed?: unknown }).factorOwed === true) setViaFactor(true);
        setResult(parsed);
        setSubmitted(true);
        setSubmitting(false);
      } catch (e) {
        const view = refusalView(errorOf(e), name);
        // Requirement 6, the E_NETWORK row: the account may exist and a second POST /v1/accounts would answer "username
        // taken" for the person's own account, so the page stays on this step with I saved it and Back blocked; Reload
        // (Signup.resume) is the only way on.
        if (view.banner?.reload === true) {
          setSubmitting(false);
          setStalled(true);
          applyRefusal(view);
          return;
        }
        // Any other refusal created nothing. The key of this attempt is discarded, and the person is back at the account
        // step (or the invite step, or the closed frame) with the reason; the next Create account makes a new key.
        setDiscarding(true);
        await discardKeys();
        setDiscarding(false);
        setSubmitting(false);
        applyRefusal({ ...view, step: view.step ?? 'identity', focus: view.focus ?? 'heading' });
      }
    };
    void run();
  };
  // The factor is in place: the worker reads the factors again, makes the browser ready and joins the invite's server.
  // Returns the call so FactorSetup knows when it ended: a refusal re-arms its button, which retries.
  const finishFactor = (added: FactorSetupResult): Promise<void> => {
    setFactorState({ busy: true, problem: null });
    const code = extractInviteCode(invite);
    return client.call({ m: 'factorSetupDone', invite: code === '' ? null : code }).then(
      (value) => {
        setCodes(added.backupCodes ?? []);
        setCodesSaved(false);
        setFactorResult(resultOf(value, code));
        setFactorState({ busy: false, problem: null });
      },
      (e: unknown) => { setFactorState({ busy: false, problem: doneProblem(errorOf(e)) }); throw e; });
  };
  const back = (to: Step) => () => {
    if (blocked) return;
    setBanner(null);
    if (step !== 'keys') { goTo(to); return; }
    setDiscarding(true);
    void discardKeys().finally(() => { setDiscarding(false); goTo(to); });
  };
  const onSubmit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (blocked) return;
    switch (step) {
      case 'connect': connectNext(); break;
      case 'identity': identityNext(); break;
      case 'keys': keysNext(); break;
    }
  };
  const open = () => {
    if (finished === null || (codes.length > 0 && !codesSaved)) return;
    setCodes([]);
    props.onFinish(finished);
  };

  // Signal task 10 fix round 1: the prototype's flow label and dots; a new account has six steps.
  const stepLabel = (n: number) => t('onboarding.step', { n, total: TOTAL });
  const progress = (n: number) => ({ n, total: TOTAL });
  const errorText = (field: Field) => {
    const key = errors[field];
    return key === undefined ? undefined : t(key);
  };
  // A blocked button stays focusable (aria-disabled, never the native attribute): the person who
  // pressed it keeps focus there while the step works (pre-flight ruling (c)).
  const blockedProps = blocked ? { 'aria-disabled': true } : {};
  // The prototype's row under the button: Back at the left (steps 2 and 3), "Already have an account? Sign in" at the
  // right (steps 1 to 3 and the closed frame, where the instance takes passwords).
  const navRow = (backTo: Step | null) => {
    const signIn = instance.passwordSignup;
    if (backTo === null && !signIn) return null;
    return (
      <div className="d-onboarding-frame__nav">
        {backTo === null ? <span /> : (
          <Button variant="ghost" type="button" data-back="" onClick={back(backTo)} {...blockedProps}>{t('onboarding.back')}</Button>
        )}
        {signIn ? (
          <Button variant="ghost" type="button" onClick={beginSignIn} {...(entering || blocked ? { 'aria-disabled': true } : {})}>
            {t('onboarding.connect.signIn')}
          </Button>
        ) : null}
      </div>
    );
  };
  const footer = (backTo: Step | null, next: string, nextProps: { 'aria-disabled'?: boolean; 'aria-describedby'?: string } = {}) => (
    <>
      {navRow(backTo)}
      <Button variant="accent" type="submit" {...blockedProps} {...nextProps}>{next}</Button>
    </>
  );
  const bannerEl = banner === null ? null : (
    <Banner tone={banner.tone} action={banner.reload ? { label: t('onboarding.error.reload'), onAction: () => browser.reload() } : undefined}>
      {t(banner.key, banner.vars)}
    </Banner>
  );
  const form = (title: string, n: number, foot: ReactNode, children: ReactNode) => (
    <form noValidate onSubmit={onSubmit}>
      <OnboardingFrame title={title} stepLabel={stepLabel(n)} progress={progress(n)} footer={foot}>
        {bannerEl}
        {children}
      </OnboardingFrame>
    </form>
  );

  let content: React.JSX.Element;
  if (shown === 'factor') {
    content = (
      <>
        {factorState.problem === null ? null : <Banner tone="danger">{t(factorState.problem.key, factorState.problem.vars)}</Banner>}
        <FactorSetup origin="signup" stepLabel={stepLabel(4)} progress={progress(4)}
          laterStep={{ stepLabel: stepLabel(5), progress: progress(5) }} instanceName={name}
          username={account.user?.username ?? username.trim().toLowerCase()} announce={announce} onDone={finishFactor} />
        <ForgetBrowser />
        <p className="d-sr-only" role="status" aria-live="polite" data-testid="onboarding-announcer">{announcement}</p>
      </>
    );
  } else if (shown === 'done' && finished !== null) {
    const channel = joinedChannels === undefined ? null : orderChannels(joinedChannels).find(c => c.kind === 0 && c.mode === 0) ?? null;
    const owed = codes.length > 0 && !codesSaved;
    content = (
      <OnboardingFrame title={t('onboarding.done.title')} stepLabel={stepLabel(6)} progress={progress(6)}
        footer={<Button variant="accent" type="button" onClick={open}
          {...(owed ? { 'aria-disabled': true, 'aria-describedby': 'onboarding-codes-hint' } : {})}>{t('onboarding.done.next')}</Button>}>
        <p>{t('onboarding.done.body')}</p>
        {joined === null || channel === null ? null : (
          <div className="d-onboarding-frame__ready">
            <span className="d-onboarding-frame__ready-mark"><Icon name={READY_ICON} size={16} stroke={3} /></span>
            <span>{t('onboarding.done.ready', { server: joined.name, channel: channel.name })}</span>
          </div>
        )}
        {/* Task 3b (Jonas): the backup codes, which the prototype does not show, sit on its done step (accepted deviation),
            and Open dilla waits for their tick as I saved it waits for the key's. */}
        {codes.length === 0 ? null : (
          <div className="dw-signup-codes">
            <p>{t('onboarding.done.codesBody')}</p>
            <CodeList codes={codes} label={t('factor.codes.label')} labels={{ copy: t('factor.codes.copy'), print: t('factor.codes.print') }}
              onCopy={() => { navigator.clipboard?.writeText(codes.join('\n')).then(() => { announce(t('factor.codes.copied')); }, () => undefined); }}
              onPrint={() => { browser.print(); }} />
            <div className="dw-signup-codes__ack">
              <input id="onboarding-codes-ack" type="checkbox" checked={codesSaved} onChange={(e) => { setCodesSaved(e.currentTarget.checked); }} />
              <label htmlFor="onboarding-codes-ack">{t('onboarding.done.codesAck')}</label>
            </div>
            {codesSaved ? null : <p id="onboarding-codes-hint" className="d-sr-only">{t('onboarding.done.codesHint')}</p>}
          </div>
        )}
        <p className="d-sr-only" role="status" aria-live="polite" data-testid="onboarding-announcer">{announcement}</p>
      </OnboardingFrame>
    );
  } else if (shown === 'closed') {
    content = (
      <OnboardingFrame title={t('onboarding.closed.title', { instance: name })} stepLabel="" footer={navRow(null)}>
        {bannerEl}
        <p>{t('onboarding.closed.body', { instance: name })}</p>
      </OnboardingFrame>
    );
  } else if (shown === 'connect') {
    const required = instance.registrationMode === 0;
    content = form(t('onboarding.connect.title'), 1, footer(null, t('onboarding.next')), <>
      <p>{required ? t('onboarding.connect.body') : t('onboarding.connect.bodyOpen', { instance: name })}</p>
      <TextField id="onboarding-invite" label={t(required ? 'onboarding.connect.invite' : 'onboarding.connect.inviteOptional')}
        value={invite} onChange={change('invite', setInvite)} placeholder={t('onboarding.connect.invitePlaceholder')}
        error={errorText('invite')} required={required} autoComplete="off" spellCheck={false} />
    </>);
  } else if (shown === 'identity') {
    content = form(t('onboarding.identity.title'), 2, footer('connect', t('onboarding.identity.submit')), <>
      <p>{t('onboarding.identity.body')}</p>
      <TextField id="onboarding-username" label={t('onboarding.identity.username')} value={username}
        onChange={change('username', setUsername)} error={errorText('username')}
        required autoComplete="username" spellCheck={false} maxLength={32} />
      {instance.passwordSignup ? (
        <TextField id="onboarding-password" type="password" label={t('onboarding.identity.password')} value={password}
          onChange={change('password', setPassword)} placeholder={t('onboarding.identity.passwordPlaceholder')}
          error={errorText('password')} required autoComplete="new-password" />
      ) : null}
    </>);
  } else {
    const key = account.recoveryKey;
    content = form(t('onboarding.keys.title'), 3,
      // Blocked until the tick, but focusable so its reason is heard (Global Constraints line 98); keysNext guards it.
      footer('identity', t(busy ? 'onboarding.keys.submitting' : 'onboarding.keys.submit'), {
        ...(!acknowledged || key === null ? { 'aria-disabled': true } : {}),
        'aria-describedby': acknowledged ? undefined : 'onboarding-ack-hint',
      }), <>
        <p>{t('onboarding.keys.body')}</p>
        {key === null ? <p role="status">{t('onboarding.keys.preparing')}</p> : (
          <RecoveryKey groups={key} label={t('onboarding.keys.label')} acknowledgeLabel={t('onboarding.keys.acknowledge')}
            acknowledged={acknowledged} onAcknowledge={(v) => { if (!blocked) setAcknowledged(v); }}
            printLabel={t('onboarding.keys.print')} onPrint={() => browser.print()} />
        )}
        {/* The prototype says this with a toast on the press; here it is the blocked button's description. */}
        {acknowledged ? null : <p id="onboarding-ack-hint" className="d-sr-only">{t('onboarding.keys.ackHint')}</p>}
        {busy ? <p role="status">{t('onboarding.keys.registering', { instance: name })}</p> : null}
      </>);
  }
  return <div ref={rootRef}>{content}</div>;
}
