import type { Meta, StoryObj } from '@storybook/react-vite';
import { useState, type ReactNode } from 'react';
import { OnboardingFrame } from './OnboardingFrame.tsx';
import { TextField } from '../TextField/TextField.tsx';
import { RecoveryKeyField } from '../RecoveryKeyField/RecoveryKeyField.tsx';
import { Banner } from '../Banner/Banner.tsx';
import { Button } from '../Button/Button.tsx';
// The shipped copy of @dilla/web, so the F16 screenshots show exactly what the app says.
import { t } from '../../../web/src/strings/index.ts';

const meta = { title: 'Screens/SignIn', parameters: { layout: 'fullscreen', edgeToEdge: true } } satisfies Meta;
export default meta;
type Story = StoryObj<typeof meta>;

// Task 11: the prototype's sample instance and the sign-in it starts with (fUser mira, a typed password).
const instance = 'midgard.gg';
const GROUPED = '7K2M-QX9D-H4TB-R8NW-C3VF-J6PZ-A1GE-Y5KS-M0QT-B7XH-W2DN-F9RC-P4ZA';
const noop = () => {};
const blocked = { 'aria-disabled': true } as const;

function Step({ n, title, footer, children }: { n: number; title: string; footer: ReactNode; children: ReactNode }) {
  return (
    <form noValidate onSubmit={e => e.preventDefault()}>
      <OnboardingFrame title={title} stepLabel={t('signin.step', { n, total: 4 })} progress={{ n, total: 4 }} footer={footer}>{children}</OnboardingFrame>
    </form>
  );
}
function Login({ refused }: { refused?: boolean }) {
  const [username, setUsername] = useState(refused ? 'ada' : 'mira');
  const [password, setPassword] = useState(refused ? 'hunter22' : 'two-ropes');
  return (
    <Step n={1} title={t('signin.login.title', { instance })}
      footer={<><Button variant="ghost" type="button">{t('signin.login.createInstead')}</Button><Button variant="accent" type="submit">{t('signin.login.submit')}</Button></>}>
      <p>{t('signin.login.body', { instance })}</p>
      <TextField id="s-username" label={t('signin.login.username')} value={username} onChange={setUsername} autoComplete="username" spellCheck={false} />
      <TextField id="s-password" type="password" label={t('signin.login.password')} value={password} onChange={setPassword}
        autoComplete="current-password" error={refused ? t('signin.error.loginFailed') : undefined} />
    </Step>
  );
}
function Key({ value, error, working }: { value: string; error?: string; working?: boolean }) {
  const [text, setText] = useState(value);
  const n = text.replace(/[\s\-–—]/g, '').length;
  const short = n !== 52;
  return (
    <Step n={3} title={t('signin.key.title')}
      footer={<><Button variant="ghost" type="button" {...(working ? blocked : {})}>{t('signin.cancel')}</Button>
        <Button variant="accent" type="submit" {...(short || working ? blocked : {})} aria-describedby={short ? 's-key-length' : undefined}>{t('signin.key.submit')}</Button></>}>
      <p>{t('signin.key.body')}</p>
      <RecoveryKeyField id="s-key" label={t('signin.key.label')} value={text} onChange={setText} hint={t('signin.key.hint', { n })} error={error} />
      {short ? <p id="s-key-length">{t('signin.error.keyLength')}</p> : null}
      {working ? <p role="status">{t('signin.key.working')}</p> : null}
    </Step>
  );
}

export const Login_: Story = { name: 'Login', render: () => <Login /> };
export const LoginRefused: Story = { render: () => <Login refused /> };
export const SecondFactor: Story = {
  render: () => (
    <Step n={2} title={t('signin.totp.title')}
      footer={<><Button variant="ghost" type="button">{t('signin.cancel')}</Button><Button variant="accent" type="submit">{t('signin.login.submit')}</Button></>}>
      <p>{t('signin.totp.body')}</p>
      <TextField id="s-code" label={t('signin.totp.code')} value="" onChange={noop} inputMode="numeric" autoComplete="one-time-code" maxLength={6} />
    </Step>
  ),
};
export const RecoveryKey: Story = { render: () => <Key value={GROUPED.slice(0, 29)} /> };
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
    </OnboardingFrame>
  ),
};
