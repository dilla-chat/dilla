import type { Meta, StoryObj } from '@storybook/react-vite';
import { useState, type ReactNode } from 'react';
import { OnboardingFrame } from './OnboardingFrame.tsx';
import { TextField } from '../TextField/TextField.tsx';
import { RecoveryKey } from '../RecoveryKey/RecoveryKey.tsx';
import { Banner } from '../Banner/Banner.tsx';
import { Button } from '../Button/Button.tsx';
// The shipped copy of @dilla/web, so the F16 screenshots show exactly what the app says.
import { t } from '../../../web/src/strings/index.ts';

const meta = { title: 'Screens/Onboarding', parameters: { layout: 'fullscreen' } } satisfies Meta;
export default meta;
type Story = StoryObj<typeof meta>;

const instance = 'dilla.test';
const KEY = ['7K2M', 'QX9D', 'H4TB', 'R8NW', 'C3VF', 'J6PZ', 'A1GE', 'Y5KS', 'M0QT', 'B7XH', 'W2DN', 'F9RC', 'P4ZA'];
const noop = () => {};

function Field(props: { id: string; label: string; initial?: string; hint?: string; error?: string; type?: 'text' | 'password' }) {
  const [value, setValue] = useState(props.initial ?? '');
  return <TextField id={props.id} label={props.label} value={value} onChange={setValue} hint={props.hint} error={props.error} type={props.type} />;
}
// While the account is being created the app blocks both buttons with aria-disabled, never the native
// attribute, so focus stays on the pressed button (task 23, pre-flight ruling (c)); the story does the same.
// After a lost response (the network banner) both stay blocked: Reload is the only way on (Requirement 6).
function Footer({ back, next, disabled }: { back?: boolean; next: string; disabled?: boolean }) {
  const blocked = disabled ? { 'aria-disabled': true } : {};
  return (
    <>
      {back ? <Button variant="ghost" {...blocked}>{t('onboarding.back')}</Button> : null}
      <Button variant="accent" {...blocked}>{next}</Button>
    </>
  );
}
function Step({ n, title, footer, children }: { n: number; title: string; footer: ReactNode; children: ReactNode }) {
  return <OnboardingFrame title={title} stepLabel={t('onboarding.step', { n, total: 6 })} progress={{ n, total: 6 }} footer={footer}>{children}</OnboardingFrame>;
}
function Connect({ open, error }: { open?: boolean; error?: string }) {
  return (
    <Step n={1} title={t('onboarding.connect.title', { instance })} footer={<Footer next={t('onboarding.next')} />}>
      <p>{t(open ? 'onboarding.connect.bodyOpen' : 'onboarding.connect.body', { instance })}</p>
      <Field id="s-invite" label={t(open ? 'onboarding.connect.inviteOptional' : 'onboarding.connect.invite')}
        initial={error ? 'ABCD-EFGH' : ''} hint={t('onboarding.connect.inviteHint')} error={error} />
    </Step>
  );
}
function Identity({ errors }: { errors?: boolean }) {
  return (
    <Step n={2} title={t('onboarding.identity.title')} footer={<Footer back next={t('onboarding.next')} />}>
      <p>{t('onboarding.identity.body', { instance })}</p>
      <Field id="s-username" label={t('onboarding.identity.username')} initial={errors ? 'ada' : ''}
        hint={t('onboarding.identity.usernameHint')} error={errors ? t('onboarding.error.usernameTaken') : undefined} />
      <Field id="s-display" label={t('onboarding.identity.display')} hint={t('onboarding.identity.displayHint')} />
      {errors ? <Field id="s-password" type="password" label={t('onboarding.identity.password')} initial="1234567"
        hint={t('onboarding.identity.passwordHint')} error={t('onboarding.error.passwordShort')} /> : null}
    </Step>
  );
}
function Keys({ initiallyAcknowledged }: { initiallyAcknowledged: boolean }) {
  const [acknowledged, setAcknowledged] = useState(initiallyAcknowledged);
  return (
    <Step n={3} title={t('onboarding.keys.title')} footer={<><Button variant="ghost">{t('onboarding.back')}</Button><Button variant="accent"
      aria-disabled={acknowledged ? undefined : true} aria-describedby={acknowledged ? undefined : 'story-ack-hint'}>{t('onboarding.next')}</Button></>}>
      <p>{t('onboarding.keys.body')}</p>
      <p>{t('onboarding.keys.loss')}</p>
      <RecoveryKey groups={KEY} label={t('onboarding.keys.label')} acknowledgeLabel={t('onboarding.keys.acknowledge')}
        acknowledged={acknowledged} onAcknowledge={setAcknowledged} printLabel={t('onboarding.keys.print')} onPrint={noop} />
      {acknowledged ? null : <p id="story-ack-hint">{t('onboarding.keys.ackHint')}</p>}
    </Step>
  );
}
function BrowserStep({ registering, network }: { registering?: boolean; network?: boolean }) {
  return (
    <Step n={4} title={t('onboarding.browser.title')}
      footer={<Footer back next={t(registering ? 'onboarding.browser.submitting' : 'onboarding.browser.submit')} disabled={registering || network} />}>
      {network ? <Banner tone="danger" action={{ label: t('onboarding.error.reload'), onAction: noop }}>{t('onboarding.error.network')}</Banner> : null}
      <p>{t('onboarding.browser.device', { instance })}</p>
      <p>{t('onboarding.browser.clear')}</p>
      <p>{t('onboarding.browser.oneBrowser')}</p>
      <p>{t('onboarding.browser.private')}</p>
      {registering ? <p role="status">{t('onboarding.browser.registering', { instance })}</p> : null}
    </Step>
  );
}

export const Connect_: Story = { name: 'Connect', render: () => <Connect /> };
export const ConnectInviteError: Story = { render: () => <Connect error={t('onboarding.error.inviteInvalid')} /> };
export const ConnectOpen: Story = { render: () => <Connect open /> };
// The closed frame is no step of the ceremony: no step line (task 23, pre-flight ruling (b)).
export const Closed: Story = {
  render: () => (
    <OnboardingFrame title={t('onboarding.closed.title', { instance })} stepLabel="" footer={null}>
      <p>{t('onboarding.closed.body', { instance })}</p>
    </OnboardingFrame>
  ),
};
export const Identity_: Story = { name: 'Identity', render: () => <Identity /> };
export const IdentityErrors: Story = { render: () => <Identity errors /> };
export const Keys_: Story = { name: 'Keys', render: () => <Keys initiallyAcknowledged={false} /> };
export const KeysAcknowledged: Story = { render: () => <Keys initiallyAcknowledged /> };
export const Browser: Story = { render: () => <BrowserStep /> };
export const Registering: Story = { render: () => <BrowserStep registering /> };
export const NetworkError: Story = { render: () => <BrowserStep network /> };
export const Done: Story = {
  render: () => (
    <Step n={5} title={t('onboarding.done.title')} footer={<Button variant="accent">{t('onboarding.done.next')}</Button>}>
      <p>{t('onboarding.done.body', { username: 'ada', instance })}</p>
    </Step>
  ),
};
