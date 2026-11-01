import { useEffect, useRef, useState } from 'react';
import type { FormEvent, ReactNode } from 'react';
import { profileTextProblem } from '@dilla/client-core';
import { Banner, Button, OnboardingFrame, RecoveryKey, Splash, TextField } from '@dilla/ui';
import { browser } from '../browser.ts';
import { useCore } from '../core/context.tsx';
import { errorOf, type UiError } from '../core/errors.ts';
import { useSlice } from '../core/use-slice.ts';
import { extractInviteCode } from '../invite.ts';
import { useRoute } from '../router.ts';
import { t, type StringKey } from '../strings/index.ts';
import { useAnnouncer } from './announcer.ts';
import { doneProblem, type DoneProblem } from './factor/doneRefusal.ts';
import { FactorSetup } from './factor/FactorSetup.tsx';
import { ForgetBrowser } from './factor/ForgetBrowser.tsx';

/** What the done step hands to the App: the value signupSubmit resolved with and the invite code sent. */
export interface SignupResult { communityId: string | null; joinError: UiError | null; invite: string }

type Step = 'connect' | 'identity' | 'keys' | 'browser';
type Field = 'invite' | 'username' | 'display' | 'password';
type FocusTarget = 'heading' | 'onboarding-invite' | 'onboarding-username' | 'onboarding-display' | 'onboarding-password';
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
const DISPLAY_MAX = 64;
const PASSWORD_MIN = 8;
const COMMUNITY_ID = /^[0-9a-f]{32}$/;
const FIELD_ORDER: readonly Field[] = ['invite', 'username', 'display', 'password'];

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
 * The signup ceremony of L-COPY-01: connect, identity, recovery key, what this browser keeps, done.
 * The account is created only by the fourth step's submit; the done step hands the result to onFinish.
 */
export function Onboarding(props: { onFinish(result: SignupResult): void }): React.JSX.Element {
  const client = useCore();
  const account = useSlice('account');
  const [route, navigate] = useRoute();
  const [step, setStep] = useState<Step>('connect');
  const [invite, setInvite] = useState(() => (route.name === 'welcome' ? route.invite ?? '' : ''));
  const [username, setUsername] = useState('');
  const [display, setDisplay] = useState('');
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
  const [closed, setClosed] = useState(false);
  // Set by the E_NETWORK / 5xx row of a refused signupSubmit: the account may exist already, so the
  // page never sends signupSubmit again and Reload (Signup.resume in the worker) is the only way on.
  const [stalled, setStalled] = useState(false);
  const [result, setResult] = useState<SignupResult | null>(null);
  // web3.6: the account was created and now owes its second factor (step 5); the done step (6) waits for the factor.
  const [viaFactor, setViaFactor] = useState(false);
  const [factorResult, setFactorResult] = useState<SignupResult | null>(null);
  const [factorState, setFactorState] = useState<{ busy: boolean; problem: DoneProblem | null }>({ busy: false, problem: null });
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
  const blocked = busy || beginning || stalled;

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
  const applyRefusal = (rejection: unknown): RefusalView => {
    const view = refusalView(errorOf(rejection), name);
    if (view.closed) setClosed(true);
    if (view.step) setStep(view.step);
    if (view.field && view.fieldKey) setErrors({ [view.field]: view.fieldKey });
    setBanner(view.banner ?? null);
    if (view.focus) requestFocus(view.focus);
    return view;
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
  // Ruling 22: the entry to the sign-in ceremony, on the connect step and the closed frame.
  const beginSignIn = () => {
    if (entering) return;
    setBanner(null);
    dropRace();
    setEntering(true);
    client.call({ m: 'signInBegin' })
      .catch(e => { setBanner({ tone: 'danger', key: 'signin.error.other', vars: { code: errorOf(e).code }, reload: false }); })
      .finally(() => setEntering(false));
  };
  const signInEntry = instance.passwordSignup ? (
    <Button variant="ghost" type="button" onClick={beginSignIn} {...(entering ? { 'aria-disabled': true } : {})}>
      {t('onboarding.connect.signIn')}
    </Button>
  ) : null;
  const identityNext = () => {
    setBanner(null);
    const next: Partial<Record<Field, StringKey>> = {};
    if (!USERNAME.test(username.trim().toLowerCase())) next.username = 'onboarding.error.usernameRule';
    // The server's text rule, so a refusal is named next to the field and not as a banner after the keys are made.
    const displayProblem = profileTextProblem(display, DISPLAY_MAX);
    if (displayProblem === 'long' || codePoints(display.trim()) > DISPLAY_MAX) next.display = 'onboarding.error.displayLength';
    else if (displayProblem === 'chars') next.display = 'profile.error.chars';
    else if (displayProblem === 'blank' && display.trim() !== '') next.display = 'profile.error.blank';
    if (instance.passwordSignup && codePoints(password) < PASSWORD_MIN) next.password = 'onboarding.error.passwordShort';
    if (showErrors(next)) return;
    // Going back never makes new keys: signupBegin runs only while no keys exist.
    if (account.phase !== 'needs-signup') { goTo('keys'); return; }
    setBeginning(true);
    const run = async () => {
      try {
        await client.call({ m: 'signupBegin' });
        setBeginning(false);
        goTo('keys');
      } catch (e) {
        setBeginning(false);
        applyRefusal(e);
      }
    };
    void run();
  };
  const keysNext = () => {
    if (!acknowledged || account.recoveryKey === null) return;
    setBanner(null);
    goTo('browser');
  };
  const submit = () => {
    setBanner(null);
    const code = extractInviteCode(invite);
    setSubmitting(true);
    const run = async () => {
      try {
        const value = await client.call({
          m: 'signupSubmit', invite: code, username: username.trim().toLowerCase(), display: display.trim(),
          password: instance.passwordSignup ? password : null, recoveryKeyAcknowledged: true,
        });
        const parsed = resultOf(value, code);
        if (typeof value === 'object' && value !== null && (value as { factorOwed?: unknown }).factorOwed === true) setViaFactor(true);
        setResult(parsed);
        setSubmitted(true);
        setSubmitting(false);
      } catch (e) {
        setSubmitting(false);
        // Requirement 6, the E_NETWORK row: a second POST /v1/accounts would answer "username taken"
        // for the person's own account, so Create account and Back stay blocked from here on.
        if (applyRefusal(e).banner?.reload === true) setStalled(true);
      }
    };
    void run();
  };
  // The factor is in place: the worker reads the factors again, makes the browser ready and joins the invite's server.
  // Returns the call so FactorSetup knows when it ended: a refusal re-arms its button, which retries.
  const finishFactor = (): Promise<void> => {
    setFactorState({ busy: true, problem: null });
    const code = extractInviteCode(invite);
    return client.call({ m: 'factorSetupDone', invite: code === '' ? null : code }).then(
      (value) => {
        setFactorResult(resultOf(value, code));
        setFactorState({ busy: false, problem: null });
      },
      (e: unknown) => { setFactorState({ busy: false, problem: doneProblem(errorOf(e)) }); throw e; });
  };
  const back = (to: Step) => () => {
    if (blocked) return;
    setBanner(null);
    goTo(to);
  };
  const onSubmit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (blocked) return;
    switch (step) {
      case 'connect': connectNext(); break;
      case 'identity': identityNext(); break;
      case 'keys': keysNext(); break;
      case 'browser': submit(); break;
    }
  };

  // Signal task 10 fix round 1: the prototype's flow label and dots; a new account has six steps.
  const TOTAL = 6;
  const stepLabel = (n: number) => t('onboarding.step', { n, total: TOTAL });
  const progress = (n: number) => ({ n, total: TOTAL });
  const errorText = (field: Field) => {
    const key = errors[field];
    return key === undefined ? undefined : t(key);
  };
  // A blocked button stays focusable (aria-disabled, never the native attribute): the person who
  // pressed it keeps focus there while the step works (pre-flight ruling (c)).
  const blockedProps = blocked ? { 'aria-disabled': true } : {};
  const footer = (backTo: Step | null, next: string, nextProps: { 'aria-disabled'?: boolean; 'aria-describedby'?: string } = {}) => (
    <>
      {backTo === null ? null : <Button variant="ghost" type="button" onClick={back(backTo)} {...blockedProps}>{t('onboarding.back')}</Button>}
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
        <FactorSetup origin="signup" stepLabel={stepLabel(5)} progress={progress(5)} instanceName={name}
          username={account.user?.username ?? username.trim().toLowerCase()} announce={announce} onDone={finishFactor} />
        <ForgetBrowser />
        <p className="d-sr-only" role="status" aria-live="polite" data-testid="onboarding-announcer">{announcement}</p>
      </>
    );
  } else if (shown === 'done' && finished !== null) {
    content = (
      <OnboardingFrame title={t('onboarding.done.title')} stepLabel={stepLabel(6)} progress={progress(6)}
        footer={<Button variant="accent" type="button" onClick={() => props.onFinish(finished)}>{t('onboarding.done.next')}</Button>}>
        <p>{t('onboarding.done.body', { username: account.user?.username ?? username.trim().toLowerCase(), instance: name })}</p>
      </OnboardingFrame>
    );
  } else if (shown === 'closed') {
    content = (
      <OnboardingFrame title={t('onboarding.closed.title', { instance: name })} stepLabel="" footer={null}>
        {bannerEl}
        <p>{t('onboarding.closed.body', { instance: name })}</p>
        {signInEntry}
      </OnboardingFrame>
    );
  } else if (shown === 'connect') {
    const required = instance.registrationMode === 0;
    content = form(t('onboarding.connect.title', { instance: name }), 1, footer(null, t('onboarding.next')), <>
      <p>{t(required ? 'onboarding.connect.body' : 'onboarding.connect.bodyOpen', { instance: name })}</p>
      <TextField id="onboarding-invite" label={t(required ? 'onboarding.connect.invite' : 'onboarding.connect.inviteOptional')}
        value={invite} onChange={change('invite', setInvite)} hint={t('onboarding.connect.inviteHint')} error={errorText('invite')}
        required={required} autoComplete="off" spellCheck={false} />
      {signInEntry}
    </>);
  } else if (shown === 'identity') {
    content = form(t('onboarding.identity.title'), 2, footer('connect', t('onboarding.next')), <>
      <p>{t('onboarding.identity.body', { instance: name })}</p>
      <TextField id="onboarding-username" label={t('onboarding.identity.username')} value={username}
        onChange={change('username', setUsername)} hint={t('onboarding.identity.usernameHint')} error={errorText('username')}
        required autoComplete="username" spellCheck={false} maxLength={32} />
      <TextField id="onboarding-display" label={t('onboarding.identity.display')} value={display}
        onChange={change('display', setDisplay)} hint={t('onboarding.identity.displayHint')} error={errorText('display')}
        autoComplete="nickname" />
      {instance.passwordSignup ? (
        <TextField id="onboarding-password" type="password" label={t('onboarding.identity.password')} value={password}
          onChange={change('password', setPassword)} hint={t('onboarding.identity.passwordHint')} error={errorText('password')}
          required autoComplete="new-password" />
      ) : null}
    </>);
  } else if (shown === 'keys') {
    const key = account.recoveryKey;
    content = form(t('onboarding.keys.title'), 3,
      // Blocked until the tick, but focusable so its reason is heard (Global Constraints line 98); keysNext guards it.
      footer('identity', t('onboarding.next'), {
        ...(!acknowledged || key === null ? { 'aria-disabled': true } : {}),
        'aria-describedby': acknowledged ? undefined : 'onboarding-ack-hint',
      }), <>
        <p>{t('onboarding.keys.body')}</p>
        <p>{t('onboarding.keys.loss')}</p>
        {key === null ? <p role="status">{t('onboarding.keys.preparing')}</p> : (
          <RecoveryKey groups={key} label={t('onboarding.keys.label')} acknowledgeLabel={t('onboarding.keys.acknowledge')}
            acknowledged={acknowledged} onAcknowledge={setAcknowledged} printLabel={t('onboarding.keys.print')}
            onPrint={() => browser.print()} />
        )}
        {acknowledged ? null : <p id="onboarding-ack-hint">{t('onboarding.keys.ackHint')}</p>}
      </>);
  } else {
    content = form(t('onboarding.browser.title'), 4,
      footer('keys', t(busy ? 'onboarding.browser.submitting' : 'onboarding.browser.submit')), <>
        <p>{t('onboarding.browser.device', { instance: name })}</p>
        <p>{t('onboarding.browser.clear')}</p>
        <p>{t('onboarding.browser.oneBrowser')}</p>
        <p>{t('onboarding.browser.private')}</p>
        {busy ? <p role="status">{t('onboarding.browser.registering', { instance: name })}</p> : null}
      </>);
  }
  return <div ref={rootRef}>{content}</div>;
}
