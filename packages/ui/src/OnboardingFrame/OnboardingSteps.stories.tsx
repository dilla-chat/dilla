import type { Meta, StoryObj } from '@storybook/react-vite';
import { useState, type ReactNode } from 'react';
import { OnboardingFrame } from './OnboardingFrame.tsx';
import { TextField } from '../TextField/TextField.tsx';
import { RecoveryKey } from '../RecoveryKey/RecoveryKey.tsx';
import { Banner } from '../Banner/Banner.tsx';
import { Button } from '../Button/Button.tsx';
import { ChoiceCards } from '../ChoiceCards/ChoiceCards.tsx';
import { CodeBoxes } from '../CodeBoxes/CodeBoxes.tsx';
import { CodeList } from '../CodeList/CodeList.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { QrCode } from '../QrCode/QrCode.tsx';
// The shipped copy of @dilla/web, so the F16 screenshots show exactly what the app says.
import { t } from '../../../web/src/strings/index.ts';

// Wave B task 3b: the prototype's six new-account steps, composed exactly as packages/web/src/screens/Onboarding.tsx
// (steps 1-3 and 6) and factor/FactorSetup.tsx with origin "signup" (steps 4 and 5) compose them. webStyles: the app's
// own rules (the setup key's row, the done step's codes) apply as in the app.
const meta = { title: 'Screens/Onboarding', parameters: { layout: 'fullscreen', edgeToEdge: true, webStyles: true } } satisfies Meta;
export default meta;
type Story = StoryObj<typeof meta>;

// The prototype's sample instance, as the sign-in stories use it.
const instance = 'midgard.gg';
const KEY = ['7K2M', 'QX9D', 'H4TB', 'R8NW', 'C3VF', 'J6PZ', 'A1GE', 'Y5KS', 'M0QT', 'B7XH', 'W2DN', 'F9RC', 'P4ZA'];
const SECRET = 'JBSWY3DPEHPK3PXP';
const CODES = ['AAAAA-BBBBB-CCCCC-DDDDD-EEEEE-F', 'GGGGG-HHHHH-IIIII-JJJJJ-KKKKK-L', 'MMMMM-NNNNN-OOOOO-PPPPP-QQQQQ-R',
  'SSSSS-TTTTT-UUUUU-VVVVV-WWWWW-X'];
const noop = () => {};
const blocked = { 'aria-disabled': true } as const;

/** Onboarding.tsx's navRow: Back at the left (steps 2 and 3), the sign-in link at the right (steps 1 to 3). */
function Nav({ back, working }: { back?: boolean; working?: boolean }) {
  return (
    <div className="d-onboarding-frame__nav">
      {back ? <Button variant="ghost" type="button" data-back="" {...(working ? blocked : {})}>{t('onboarding.back')}</Button> : <span />}
      <Button variant="ghost" type="button" {...(working ? blocked : {})}>{t('onboarding.connect.signIn')}</Button>
    </div>
  );
}
function Step({ n, title, footer, children }: { n: number; title: string; footer: ReactNode; children: ReactNode }) {
  return (
    <form noValidate onSubmit={e => e.preventDefault()}>
      <OnboardingFrame title={title} stepLabel={t('onboarding.step', { n, total: 6 })} progress={{ n, total: 6 }} footer={footer}>{children}</OnboardingFrame>
    </form>
  );
}
function Field(props: { id: string; label: string; initial?: string; placeholder?: string; error?: string; type?: 'text' | 'password' }) {
  const [value, setValue] = useState(props.initial ?? '');
  return <TextField id={props.id} label={props.label} value={value} onChange={setValue} placeholder={props.placeholder} error={props.error} type={props.type} />;
}

function Invite({ open, error }: { open?: boolean; error?: string }) {
  return (
    <Step n={1} title={t('onboarding.connect.title')}
      footer={<><Nav /><Button variant="accent" type="submit">{t('onboarding.next')}</Button></>}>
      <p>{open ? t('onboarding.connect.bodyOpen', { instance }) : t('onboarding.connect.body')}</p>
      <Field id="s-invite" label={t(open ? 'onboarding.connect.inviteOptional' : 'onboarding.connect.invite')}
        initial={error ? 'ABCD-EFGH' : ''} placeholder={t('onboarding.connect.invitePlaceholder')} error={error} />
    </Step>
  );
}
function Account({ errors, banner }: { errors?: boolean; banner?: string }) {
  return (
    <Step n={2} title={t('onboarding.identity.title')}
      footer={<><Nav back /><Button variant="accent" type="submit">{t('onboarding.identity.submit')}</Button></>}>
      {banner === undefined ? null : <Banner tone="warn">{banner}</Banner>}
      <p>{t('onboarding.identity.body')}</p>
      <Field id="s-username" label={t('onboarding.identity.username')} initial={errors ? 'ada' : ''}
        error={errors ? t('onboarding.error.usernameTaken') : undefined} />
      <Field id="s-password" type="password" label={t('onboarding.identity.password')} initial={errors ? 'short pw' : ''}
        placeholder={t('onboarding.identity.passwordPlaceholder')} error={errors ? t('onboarding.error.passwordShort') : undefined} />
    </Step>
  );
}
function Keys({ initiallyAcknowledged, registering, network }: { initiallyAcknowledged: boolean; registering?: boolean; network?: boolean }) {
  const [acknowledged, setAcknowledged] = useState(initiallyAcknowledged);
  const working = registering === true || network === true;
  const notReady = !acknowledged || working;
  return (
    <Step n={3} title={t('onboarding.keys.title')}
      footer={<><Nav back working={working} />
        <Button variant="accent" type="submit" {...(notReady ? blocked : {})} aria-describedby={acknowledged ? undefined : 'story-ack-hint'}>
          {t(registering ? 'onboarding.keys.submitting' : 'onboarding.keys.submit')}
        </Button></>}>
      {network ? <Banner tone="danger" action={{ label: t('onboarding.error.reload'), onAction: noop }}>{t('onboarding.error.network')}</Banner> : null}
      <p>{t('onboarding.keys.body')}</p>
      <RecoveryKey groups={KEY} label={t('onboarding.keys.label')} acknowledgeLabel={t('onboarding.keys.acknowledge')}
        acknowledged={acknowledged} onAcknowledge={setAcknowledged} printLabel={t('onboarding.keys.print')} onPrint={noop}
        saveLabel={t('onboarding.keys.save')} onSave={noop} />
      {acknowledged ? null : <p id="story-ack-hint" className="d-sr-only">{t('onboarding.keys.ackHint')}</p>}
      {registering ? <p role="status">{t('onboarding.keys.registering', { instance })}</p> : null}
    </Step>
  );
}
/** FactorSetup's choice, origin "signup": the prototype's subtitle and cards, Continue, no Back. */
function Factor() {
  const [value, setValue] = useState('app');
  return (
    <Step n={4} title={t('factor.choose.title')} footer={<Button variant="accent" type="submit">{t('factor.choose.next')}</Button>}>
      <p>{t('factor.choose.bodySignup')}</p>
      <ChoiceCards name="s-factor-kind" label={t('factor.choose.label')} value={value} onChange={setValue} options={[
        { id: 'app', title: t('factor.choose.app'), hint: t('factor.choose.appHint'), icon: 'smartphone' },
        { id: 'passkey', title: t('factor.choose.passkey'), hint: t('factor.choose.passkeyHint'), icon: 'fingerprint' },
      ]} />
    </Step>
  );
}
/** FactorSetup's app step, origin "signup": the prototype's title and Verify, the QR code, setup key and hint kept. */
function Code({ initial = '' }: { initial?: string }) {
  const [code, setCode] = useState(initial);
  return (
    <Step n={5} title={t('factor.totp.titleSignup')}
      footer={<><div className="d-onboarding-frame__nav"><Button variant="ghost" type="button" data-back="">{t('factor.back')}</Button></div>
        <Button variant="accent" type="submit" {...(code.length === 6 ? {} : blocked)}>{t('factor.totp.verify')}</Button></>}>
      <p>{t('factor.totp.body')}</p>
      <div className="dw-factor-key">
        <QrCode value={`otpauth://totp/${instance}:mira?secret=${SECRET}&issuer=${instance}`} label={t('factor.totp.qr', { username: 'mira', instance })} />
        <div className="dw-factor-key__text">
          <div className="dw-factor-key__field">
            <TextField id="s-factor-key" label={t('factor.totp.key')} value={SECRET} onChange={noop} readOnly spellCheck={false} autoComplete="off" />
            <Button type="button">{t('factor.totp.copyKey')}</Button>
          </div>
        </div>
      </div>
      <p id="s-factor-code-hint" className="dw-factor-code-hint">{t('factor.totp.codeHint')}</p>
      <CodeBoxes id="s-factor-code" label={t('factor.totp.code')} value={code} onChange={setCode} describedBy="s-factor-code-hint" />
    </Step>
  );
}
/** Onboarding.tsx's done step: the prototype's panel, then the backup codes and their tick (accepted deviation). */
function Done({ codes = true, saved = false, panel = true }: { codes?: boolean; saved?: boolean; panel?: boolean }) {
  const [ticked, setTicked] = useState(saved);
  const owed = codes && !ticked;
  return (
    <OnboardingFrame title={t('onboarding.done.title')} stepLabel={t('onboarding.step', { n: 6, total: 6 })} progress={{ n: 6, total: 6 }}
      footer={<Button variant="accent" type="button" {...(owed ? { ...blocked, 'aria-describedby': 's-codes-hint' } : {})}>{t('onboarding.done.next')}</Button>}>
      <p>{t('onboarding.done.body')}</p>
      {panel ? (
        <div className="d-onboarding-frame__ready">
          <span className="d-onboarding-frame__ready-mark"><Icon name="check" size={16} stroke={3} /></span>
          <span>{t('onboarding.done.ready', { server: 'Midgard Crew', channel: 'general' })}</span>
        </div>
      ) : null}
      <p className="d-onboarding-frame__note">{t('onboarding.done.note')}</p>
      {codes ? (
        <div className="dw-signup-codes">
          <p>{t('onboarding.done.codesBody')}</p>
          <CodeList codes={CODES} label={t('factor.codes.label')} labels={{ copy: t('factor.codes.copy'), print: t('factor.codes.print') }}
            onCopy={noop} onPrint={noop} />
          <div className="d-recovery-key__ack">
            <input id="s-codes-ack" type="checkbox" className="d-recovery-key__box" checked={ticked} onChange={e => setTicked(e.currentTarget.checked)} />
            <span className="d-recovery-key__tick" aria-hidden="true"><Icon name="check" size={11} stroke={3} /></span>
            <label htmlFor="s-codes-ack">{t('onboarding.done.codesAck')}</label>
          </div>
          {ticked ? null : <p id="s-codes-hint" className="d-sr-only">{t('onboarding.done.codesHint')}</p>}
        </div>
      ) : null}
    </OnboardingFrame>
  );
}

export const Invite_: Story = { name: 'Invite', render: () => <Invite /> };
export const InviteError: Story = { render: () => <Invite error={t('onboarding.error.inviteInvalid')} /> };
export const InviteOpen: Story = { render: () => <Invite open /> };
// The closed frame is no step of the ceremony: no step line (task 23, pre-flight ruling (b)).
export const Closed: Story = {
  render: () => (
    <OnboardingFrame title={t('onboarding.closed.title', { instance })} stepLabel="" footer={<Nav />}>
      <p>{t('onboarding.closed.body', { instance })}</p>
    </OnboardingFrame>
  ),
};
export const Account_: Story = { name: 'Account', render: () => <Account /> };
export const AccountErrors: Story = { render: () => <Account errors /> };
export const AccountRefused: Story = { render: () => <Account banner={t('onboarding.error.rateLimited', { seconds: 5 })} /> };
export const Keys_: Story = { name: 'Keys', render: () => <Keys initiallyAcknowledged={false} /> };
export const KeysAcknowledged: Story = { render: () => <Keys initiallyAcknowledged /> };
export const Registering: Story = { render: () => <Keys initiallyAcknowledged registering /> };
export const NetworkError: Story = { render: () => <Keys initiallyAcknowledged network /> };
export const Factor_: Story = { name: 'Factor', render: () => <Factor /> };
export const Code_: Story = { name: 'Code', render: () => <Code /> };
export const CodeTyped: Story = { render: () => <Code initial="482" /> };
export const Done_: Story = { name: 'Done', render: () => <Done /> };
export const DoneCodesSaved: Story = { render: () => <Done saved /> };
export const DoneNoServer: Story = { render: () => <Done panel={false} /> };
export const DonePrototype: Story = { render: () => <Done codes={false} /> };
