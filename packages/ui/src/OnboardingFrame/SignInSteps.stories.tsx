import type { Meta, StoryObj } from '@storybook/react-vite';
import { useEffect, useState, type ReactNode } from 'react';
import { OnboardingFrame } from './OnboardingFrame.tsx';
import { TextField } from '../TextField/TextField.tsx';
import { RecoveryKeyField } from '../RecoveryKeyField/RecoveryKeyField.tsx';
import { Banner } from '../Banner/Banner.tsx';
import { Button } from '../Button/Button.tsx';
import { CodeBoxes } from '../CodeBoxes/CodeBoxes.tsx';
import { Icon } from '../Icon/Icon.tsx';
// The shipped copy of @dilla/web, so the F16 screenshots show exactly what the app says.
import { t } from '../../../web/src/strings/index.ts';

const meta = { title: 'Screens/SignIn', parameters: { layout: 'fullscreen', edgeToEdge: true } } satisfies Meta;
export default meta;
type Story = StoryObj<typeof meta>;

// Task 11: the prototype's sample instance and the sign-in it starts with (fUser mira, a typed password).
const instance = 'midgard.gg';
const GROUPED = '7K2M-QX9D-H4TB-R8NW-C3VF-J6PZ-A1GE-Y5KS-M0QT-B7XH-W2DN-F9RC-P4ZA';
const blocked = { 'aria-disabled': true } as const;
// Wave B task 3 continuation: the row under the button on the code and key steps, as SignIn.tsx's navRow draws it.
function Nav({ working }: { working?: boolean }) {
  return (
    <div className="d-onboarding-frame__nav">
      <Button variant="ghost" type="button" data-back="" {...(working ? blocked : {})}>{t('factor.back')}</Button>
      <Button variant="ghost" type="button" {...(working ? blocked : {})}>{t('signin.login.createInstead')}</Button>
    </div>
  );
}

function Step({ n, title, footer, children }: { n: number; title: string; footer: ReactNode; children: ReactNode }) {
  return (
    <form noValidate onSubmit={e => e.preventDefault()}>
      <OnboardingFrame title={title} stepLabel={t('signin.step', { n, total: 4 })} progress={{ n, total: 4 }} footer={footer}>{children}</OnboardingFrame>
    </form>
  );
}
function Login({ refused, passkeyButton }: { refused?: boolean; passkeyButton?: boolean }) {
  const [username, setUsername] = useState(refused ? 'ada' : 'mira');
  const [password, setPassword] = useState(refused ? 'hunter22' : 'two-ropes');
  return (
    <Step n={1} title={t('signin.login.title', { instance })}
      footer={<><Button variant="ghost" type="button">{t('signin.login.createInstead')}</Button><Button variant="accent" type="submit">{t('signin.login.submit')}</Button></>}>
      <p>{t('signin.login.body', { instance })}</p>
      <TextField id="s-username" label={t('signin.login.username')} value={username} onChange={setUsername} autoComplete="username" spellCheck={false} />
      <TextField id="s-password" type="password" label={t('signin.login.password')} value={password} onChange={setPassword}
        autoComplete="current-password" error={refused ? t('signin.error.loginFailed') : undefined} />
      {passkeyButton ? <Button variant="ghost" type="button">{t('factorSignin.usePasskey')}</Button> : null}
    </Step>
  );
}
function Key({ value, error, working }: { value: string; error?: string; working?: boolean }) {
  const [text, setText] = useState(value);
  const n = text.replace(/[\s\-–—]/g, '').length;
  const short = n !== 52;
  return (
    <Step n={3} title={t('signin.key.title')}
      footer={<><Nav working={working} />
        <Button variant="accent" type="submit" {...(short || working ? blocked : {})} aria-describedby={short ? 's-key-length' : undefined}>{t('signin.key.submit')}</Button></>}>
      <p>{t('signin.key.body')}</p>
      <RecoveryKeyField id="s-key" label={t('signin.key.label')} value={text} onChange={setText} hint={t('signin.key.hint', { n })} error={error}
        placeholder={t('signin.key.placeholder')} />
      {short ? <p id="s-key-length" className="d-onboarding-frame__note">{t('signin.error.keyLength')}</p> : null}
      {/* As SignIn.tsx: the status region is always there, and the page's announcer after it. */}
      <p role="status">{working ? t('signin.key.working') : null}</p>
      <p className="d-sr-only" aria-live="polite" />
    </Step>
  );
}

export const Login_: Story = { name: 'Login', render: () => <Login /> };
export const LoginRefused: Story = { render: () => <Login refused /> };
// Fix round 2 (Jonas): in a browser without conditional mediation the login keeps "Sign in with a passkey" (SignIn.tsx,
// autofill === false); an accepted deviation for those browsers only.
export const LoginNoAutofill: Story = { render: () => <Login passkeyButton /> };
// Wave B task 3: the code step exactly as packages/web/src/screens/SignIn.tsx composes it (shown === 'totp'): the body as the
// code's description, the six boxes with focus (SignIn's focus request), the links row, the empty status region, Verify
// (blocked until six digits) and the Back / new-account row. "Use a passkey" shows after a passkey that needs the code
// as well (with its note) and after a password login that said the account holds a passkey (`withPasskey`, no note).
function Code({ initial, afterPasskey, withPasskey }: { initial?: string; afterPasskey?: boolean; withPasskey?: boolean }) {
  const [code, setCode] = useState(initial ?? '');
  useEffect(() => { document.getElementById('s-code')?.focus(); }, []);
  return (
    <Step n={2} title={t('signin.totp.title')}
      footer={<><Nav />
        <Button variant="accent" type="submit" {...(/^\d{6}$/.test(code) ? {} : blocked)}>{t('signin.totp.submit')}</Button></>}>
      {afterPasskey ? <p>{t('factorSignin.passkey.needsCode', { instance })}</p> : null}
      <p id="s-code-body">{t('signin.totp.body')}</p>
      <CodeBoxes id="s-code" label={t('signin.totp.code')} value={code} onChange={setCode} describedBy="s-code-body" />
      <div className="d-onboarding-frame__links">
        {afterPasskey || withPasskey ? <Button variant="ghost" type="button">{t('factorSignin.code.usePasskey')}</Button> : null}
        <Button variant="ghost" type="button">{t('factorSignin.useBackup')}</Button>
      </div>
      <p role="status" />
    </Step>
  );
}
export const SecondFactor: Story = { render: () => <Code /> };
export const SecondFactorTyped: Story = { render: () => <Code initial="482" /> };
export const SecondFactorAfterPasskey: Story = { render: () => <Code afterPasskey /> };
export const SecondFactorWithPasskey: Story = { render: () => <Code withPasskey /> };
export const SecondFactorWithPasskeyTyped: Story = { render: () => <Code withPasskey initial="482" /> };
export const RecoveryKey: Story = { render: () => <Key value={GROUPED.slice(0, 29)} /> };
export const RecoveryKeyEmpty: Story = { render: () => <Key value="" /> };
export const RecoveryKeyWrong: Story = { render: () => <Key value={GROUPED.replace('7K2M', '7K2N')} error={t('signin.error.wrongKey')} /> };
export const NoBackup: Story = {
  render: () => (
    <OnboardingFrame title={t('signin.key.title')} stepLabel={t('signin.step', { n: 3, total: 4 })} progress={{ n: 3, total: 4 }} footer={<Button variant="ghost" type="button">{t('signin.cancel')}</Button>}>
      <Banner tone="danger">{t('signin.error.noBackup', { instance })}</Banner>
    </OnboardingFrame>
  ),
};
export const Adding: Story = { render: () => <Key value={GROUPED} working /> };
export const Done: Story = {
  render: () => (
    <OnboardingFrame title={t('signin.done.title')} stepLabel={t('signin.step', { n: 4, total: 4 })} progress={{ n: 4, total: 4 }}
      footer={<Button variant="accent" type="button">{t('signin.done.next')}</Button>}>
      <p>{t('signin.done.body', { username: 'ada', instance })}</p>
      <div className="d-onboarding-frame__ready">
        <span className="d-onboarding-frame__ready-mark"><Icon name="check" size={16} stroke={3} /></span>
        <span>{t('signin.done.ready', { server: 'Midgard Crew', channel: 'general' })}</span>
      </div>
    </OnboardingFrame>
  ),
};
