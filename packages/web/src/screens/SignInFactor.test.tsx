// web3.6 W2: the second factor in the sign-in ceremony (flow 10, states 14 to 16; flow 03 step 2). web3.8 (R6, R7): the
// factor comes after the login and before the key; a backup code waits in the page and goes only with the key.
import { afterEach, describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent, { type UserEvent } from '@testing-library/user-event';
import type { AccountState, BootPhase, Command, FactorsState } from '@dilla/client-core';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient, refusal } from '../test/fake-client.ts';
import { account, INSTANCE } from '../test/fixtures.ts';
import { expectNoAxeViolations } from '../test/setup.ts';
import { en } from '../strings/index.ts';
import { liveRegions, watchRegion } from '../test/live-regions.ts';
import { SignIn } from './SignIn.tsx';

const GROUPED = '7K2M-QX9D-H4TB-R8NW-C3VF-J6PZ-A1GE-Y5KS-M0QT-B7XH-W2DN-F9RC-P4ZA';
const ME = { id: 'bb'.repeat(16), username: 'ada' };
const CEREMONY = 'cd'.repeat(16);
const BACKUP = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ';
const SECRET = 'JBSWY3DPEHPK3PXP';
const OPTIONS = JSON.stringify({ publicKey: { challenge: 'Zm9vYmFyLWNoYWxsZW5nZS1fLQ', rpId: 'dilla.test', userVerification: 'preferred' } });
const NO_FACTORS: FactorsState = { owed: true, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true };

const get = vi.fn<(o: { publicKey: { userVerification: string } }) => Promise<unknown>>();
function webauthn(on: boolean): void {
  if (on) {
    Object.defineProperty(window, 'PublicKeyCredential', { value: class {}, configurable: true });
    Object.defineProperty(navigator, 'credentials', { value: { get }, configurable: true });
  } else {
    Reflect.deleteProperty(window, 'PublicKeyCredential');
    Reflect.deleteProperty(navigator, 'credentials');
  }
}
Object.defineProperty(window, 'isSecureContext', { value: true, configurable: true });
const assertion = () => ({ id: 'AQID', rawId: new Uint8Array([1, 2, 3]).buffer, type: 'public-key', authenticatorAttachment: 'platform',
  response: { clientDataJSON: new Uint8Array([4]).buffer, authenticatorData: new Uint8Array([5]).buffer, signature: new Uint8Array([6]).buffer, userHandle: null },
  getClientExtensionResults: () => ({}) });
afterEach(() => { webauthn(false); get.mockReset(); });

type Script = (c: Command, fake: FakeClient) => Promise<unknown>;
function move(fake: FakeClient, over: Partial<AccountState>): void {
  fake.set('account', { ...fake.get('account')!, ...over });
}
const readyMe = { phase: 'ready', user: ME, deviceId: 'dd'.repeat(16), signIn: null, factorSetup: null } as const;
// web3.8 (R6): a settled login, passkey or code holds the assertion and waits for the key.
const held = { phase: 'signin-key', signIn: { username: 'ada', factor: null } } as const;

function setup(phase: BootPhase, script: Script, over: Partial<AccountState> = {}, opts: { webauthn?: boolean } = {}) {
  const fake = new FakeClient();
  fake.set('account', account({
    phase, user: null, deviceId: null, instance: { ...INSTANCE, passwordSignup: true },
    signIn: { username: null, factor: null }, ...over,
  }));
  fake.handler = c => script(c, fake);
  webauthn(opts.webauthn ?? true);
  const onFinish = vi.fn();
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><SignIn onFinish={onFinish} /></CoreProvider></div>);
  return { fake, onFinish, user, view };
}
const h1 = () => screen.getByRole('heading', { level: 1 });
const button = (name: string) => screen.getByRole('button', { name });
const field = (name: string) => screen.getByRole('textbox', { name });
const password = () => screen.getByLabelText('Password');

async function login(user: UserEvent): Promise<void> {
  await user.type(field('Username'), 'ada');
  await user.type(password(), 'correct horse');
  await user.click(button('Continue'));
}
/** The key step: the key, then Add this browser. */
async function addWithKey(user: UserEvent): Promise<void> {
  expect(h1()).toHaveTextContent('Your recovery key');
  await user.click(field('Recovery key'));
  await user.paste(GROUPED);
  await user.click(button('Add this browser'));
}

/** A worker for an account with an app (needs 1) or a passkey only (needs 2); a passkey may or may not verify the person. */
const worker = (opts: { needs?: 0 | 1 | 2; passkeyNeeds?: 0 | 1 } = {}): Script => (c, fake) => {
  switch (c.m) {
    case 'signInLogin': {
      const needs = opts.needs ?? 1;
      if (needs === 1) move(fake, { phase: 'signin-totp', signIn: { username: 'ada', factor: 'totp' } });
      else if (needs === 2) move(fake, { phase: 'signin-passkey', signIn: { username: 'ada', factor: 'passkey' } });
      else move(fake, held);
      return Promise.resolve({ needs });
    }
    case 'signInPasskeyBegin': return Promise.resolve({ ceremonyId: CEREMONY, options: OPTIONS });
    case 'signInPasskey': {
      const needs = opts.passkeyNeeds ?? 0;
      if (needs === 1) move(fake, { phase: 'signin-totp', signIn: { username: null, factor: 'passkey' } });
      else move(fake, held);
      return Promise.resolve({ needs });
    }
    case 'signInTotp':
      move(fake, held);
      return Promise.resolve(null);
    case 'signInBackupCode': case 'signInKey':
      move(fake, readyMe);
      return Promise.resolve(null);
    case 'signInCancel':
      move(fake, { phase: 'needs-signup', signIn: null });
      return Promise.resolve(null);
    default: return Promise.resolve(null);
  }
};

describe('the code step (flow 10 state 16)', () => {
  it('offers a backup code and the lost-phone line, and no passkey note for an app account', async () => {
    const { user, view } = setup('signin-login', worker());
    await login(user);
    expect(h1()).toHaveTextContent('Your second factor');
    expect(screen.getByText('Sign in · step 2 of 4')).toBeInTheDocument();
    expect(button(en['factorSignin.useBackup'])).toHaveAttribute('type', 'button');
    expect(screen.getByText('Lost your phone? Use a backup code, or ask the operator of dilla.test to reset your second factor.')).toBeInTheDocument();
    expect(screen.queryByText(/did not check that it is you/)).toBeNull();
    expect(field('Code')).toHaveAttribute('autocomplete', 'one-time-code');
    expect(field('Code')).toHaveAttribute('inputmode', 'numeric');
    await expectNoAxeViolations(view.container);
  });

  it('says why a code is asked after a passkey that did not check the person', async () => {
    const { fake, user } = setup('signin-login', worker({ needs: 0, passkeyNeeds: 1 }));
    get.mockResolvedValue(assertion());
    await user.click(button(en['factorSignin.usePasskey']));
    await waitFor(() => { expect(h1()).toHaveTextContent('Your second factor'); });
    expect(screen.getByText('Sign in · step 2 of 4')).toBeInTheDocument();
    expect(screen.getByText('This passkey did not check that it is you, so dilla.test also asks for your code.')).toBeInTheDocument();
    // The code goes alone; the key follows as step 3.
    await user.type(field('Code'), '123456{Enter}');
    expect(fake.callsOf('signInTotp')).toEqual([{ m: 'signInTotp', code: '123456' }]);
    expect(screen.getByText('Sign in · step 3 of 4')).toBeInTheDocument();
    await addWithKey(user);
    expect(fake.callsOf('signInKey')).toEqual([{ m: 'signInKey', recoveryKey: GROUPED }]);
    expect(h1()).toHaveTextContent('You’re in');
  });

  it('signs in with a backup code: the form, the format check, the cleaned code, the codes left', async () => {
    const { fake, user, view } = setup('signin-login', worker());
    await login(user);
    fake.set('factors', { owed: false, totp: { confirmedAt: 1 }, passkeys: [], backupCodesLeft: 9, hasPassword: true });
    await user.click(button(en['factorSignin.useBackup']));
    expect(h1()).toHaveTextContent('Use a backup code');
    expect(screen.getByText('Sign in · step 2 of 4')).toBeInTheDocument();
    expect(document.activeElement).toBe(field('Backup code'));
    expect(screen.queryByRole('textbox', { name: 'Code' })).toBeNull();
    await expectNoAxeViolations(view.container);
    await user.type(field('Backup code'), 'abcd-1{Enter}');
    expect(field('Backup code')).toHaveAccessibleDescription(en['factorSignin.backup.format']);
    expect(fake.callsOf('signInBackupCode')).toEqual([]);
    await user.clear(field('Backup code'));
    await user.type(field('Backup code'), 'abcde fghij-klmno pqrst uvwxy z{Enter}');
    expect(fake.callsOf('signInBackupCode')).toEqual([]);
    expect(screen.getByText('Sign in · step 3 of 4')).toBeInTheDocument();
    await addWithKey(user);
    expect(fake.callsOf('signInBackupCode')).toEqual([{ m: 'signInBackupCode', code: BACKUP, recoveryKey: GROUPED }]);
    expect(h1()).toHaveTextContent('You’re in');
    expect(screen.getByText('You have 9 backup codes left. Make new ones in Settings, under sign-in.')).toBeInTheDocument();
  });

  it('says one code left in the singular, and says nothing after a code from the app', async () => {
    const one = setup('signin-login', worker());
    await login(one.user);
    one.fake.set('factors', { owed: false, totp: { confirmedAt: 1 }, passkeys: [], backupCodesLeft: 1, hasPassword: true });
    await one.user.click(button(en['factorSignin.useBackup']));
    await one.user.type(field('Backup code'), `${BACKUP}{Enter}`);
    await addWithKey(one.user);
    expect(screen.getByText('You have 1 backup code left. Make new ones in Settings, under sign-in.')).toBeInTheDocument();
    one.view.unmount();
    const app = setup('signin-login', worker());
    await login(app.user);
    app.fake.set('factors', { owed: false, totp: { confirmedAt: 1 }, passkeys: [], backupCodesLeft: 9, hasPassword: true });
    await app.user.type(field('Code'), '123456{Enter}');
    await addWithKey(app.user);
    expect(h1()).toHaveTextContent('You’re in');
    expect(screen.queryByText(/backup codes? left/)).toBeNull();
  });

  it('says how many codes are left even when the backup code was spent by a call that failed on the key, and the retry used signInKey', async () => {
    let first = true;
    const script: Script = (c, f) => {
      if (c.m === 'signInBackupCode' && first) {
        first = false;
        move(f, held);
        return Promise.reject(refusal({ code: 'E_RECOVERY_KEY', status: 400 }));
      }
      return worker()(c, f);
    };
    const { fake, user } = setup('signin-login', script);
    await login(user);
    fake.set('factors', { owed: false, totp: { confirmedAt: 1 }, passkeys: [], backupCodesLeft: 8, hasPassword: true });
    await user.click(button(en['factorSignin.useBackup']));
    await user.type(field('Backup code'), `${BACKUP}{Enter}`);
    await addWithKey(user);
    await waitFor(() => { expect(fake.callsOf('signInBackupCode')).toHaveLength(1); });
    await waitFor(() => { expect(h1()).toHaveTextContent('Your recovery key'); });
    await user.click(button('Add this browser'));
    await waitFor(() => { expect(fake.callsOf('signInKey')).toHaveLength(1); });
    expect(h1()).toHaveTextContent('You’re in');
    expect(screen.getByText('You have 8 backup codes left. Make new ones in Settings, under sign-in.')).toBeInTheDocument();
  });

  it('says nothing about backup codes after a refused code sent the person back to the login and a code from the app signed them in', async () => {
    let first = true;
    const script: Script = (c, f) => {
      if (c.m === 'signInBackupCode' && first) {
        first = false;
        move(f, { phase: 'signin-login', signIn: { username: 'ada', factor: null } });
        return Promise.reject(refusal({ code: 'E_UNAUTHENTICATED', status: 401 }));
      }
      return worker()(c, f);
    };
    const { fake, user } = setup('signin-login', script);
    await login(user);
    await user.click(button(en['factorSignin.useBackup']));
    await user.type(field('Backup code'), `${BACKUP}{Enter}`);
    await addWithKey(user);
    await waitFor(() => { expect(h1()).toHaveTextContent('Sign in to dilla.test'); });
    await login(user);
    fake.set('factors', { owed: false, totp: { confirmedAt: 1 }, passkeys: [], backupCodesLeft: 9, hasPassword: true });
    await user.type(field('Code'), '123456{Enter}');
    await addWithKey(user);
    expect(h1()).toHaveTextContent('You’re in');
    expect(screen.queryByText(/backup codes? left/)).toBeNull();
  });

  it('a refused backup code returns to the login with the password cleared, and the next login sends no key', async () => {
    const script: Script = (c, f) => {
      if (c.m === 'signInBackupCode') {
        move(f, { phase: 'signin-login', signIn: { username: 'ada', factor: null }, error: { code: 'E_UNAUTHENTICATED', detail: '', status: 401, retryAfterMs: null } });
        return Promise.reject(refusal({ code: 'E_UNAUTHENTICATED', status: 401 }));
      }
      return worker()(c, f);
    };
    const { fake, user } = setup('signin-login', script);
    await login(user);
    await user.click(button(en['factorSignin.useBackup']));
    await user.type(field('Backup code'), `${BACKUP}{Enter}`);
    await addWithKey(user);
    expect(fake.callsOf('signInBackupCode')).toEqual([{ m: 'signInBackupCode', code: BACKUP, recoveryKey: GROUPED }]);
    await waitFor(() => { expect(h1()).toHaveTextContent('Sign in to dilla.test'); });
    expect(screen.getByRole('alert')).toHaveTextContent(en['factorSignin.backup.failed']);
    expect(password()).toHaveValue('');
    expect(document.activeElement).toBe(password());
    expect(screen.queryByRole('textbox', { name: 'Recovery key' })).toBeNull();
    // Signing in again sends the username and the password only, and asks for a factor again.
    await user.type(password(), 'correct horse');
    await user.click(button('Continue'));
    expect(fake.callsOf('signInLogin').at(-1)).toEqual({ m: 'signInLogin', username: 'ada', password: 'correct horse' });
    expect(h1()).toHaveTextContent('Your second factor');
  });

  // Fix round 1 (review Important 2): on the backup path the code is spent before the key is checked against the account,
  // so a wrong key lands at signin-key; the retry must send signInKey, never the spent code again.
  it('a wrong key after a backup code is retried with signInKey alone', async () => {
    let wrong = true;
    const script: Script = (c, f) => {
      if (c.m === 'signInBackupCode' && wrong) {
        wrong = false;
        move(f, { phase: 'enrolling' });
        move(f, { phase: 'signin-key', signIn: { username: 'ada', factor: null } });
        return Promise.reject(refusal({ code: 'E_RECOVERY_KEY' }));
      }
      return worker()(c, f);
    };
    const { fake, user } = setup('signin-login', script);
    await login(user);
    await user.click(button(en['factorSignin.useBackup']));
    await user.type(field('Backup code'), `${BACKUP}{Enter}`);
    const typo = GROUPED.replace('7K2M', '7K2N');
    await user.click(field('Recovery key'));
    await user.paste(typo);
    await user.click(button('Add this browser'));
    expect(h1()).toHaveTextContent('Your recovery key');
    expect(field('Recovery key')).toHaveValue(typo);
    expect(field('Recovery key')).toHaveAccessibleDescription(expect.stringContaining(en['signin.error.wrongKey']));
    await user.clear(field('Recovery key'));
    await user.paste(GROUPED);
    await user.click(button('Add this browser'));
    expect(fake.calls.filter(c => c.m === 'signInBackupCode' || c.m === 'signInKey')).toEqual([
      { m: 'signInBackupCode', code: BACKUP, recoveryKey: typo },
      { m: 'signInKey', recoveryKey: GROUPED },
    ]);
    expect(h1()).toHaveTextContent('You’re in');
  });

  // Fix round 1 (review Minor 3): a factor step shown before a refusal does not count for the next attempt (R7).
  it('after a refused code, a passkey on the login step makes the key step 2 again', async () => {
    const script: Script = (c, f) => {
      if (c.m === 'signInTotp') {
        move(f, { phase: 'signin-login', signIn: { username: 'ada', factor: null } });
        return Promise.reject(refusal({ code: 'E_UNAUTHENTICATED', status: 401 }));
      }
      return worker({ needs: 1, passkeyNeeds: 0 })(c, f);
    };
    const { user } = setup('signin-login', script);
    await login(user);
    expect(screen.getByText('Sign in · step 2 of 4')).toBeInTheDocument();
    await user.type(field('Code'), '000000{Enter}');
    expect(h1()).toHaveTextContent('Sign in to dilla.test');
    get.mockResolvedValue(assertion());
    await user.click(button(en['factorSignin.usePasskey']));
    await waitFor(() => { expect(h1()).toHaveTextContent('Your recovery key'); });
    expect(screen.getByText('Sign in · step 2 of 3')).toBeInTheDocument();
  });

  it('Back leaves the backup-code form for the code form', async () => {
    const { user } = setup('signin-login', worker());
    await login(user);
    await user.click(button(en['factorSignin.useBackup']));
    await user.click(button('Back'));
    expect(h1()).toHaveTextContent('Your second factor');
    expect(field('Code')).toBeInTheDocument();
  });
});

describe('the passkey (flow 10 states 14 and 15)', () => {
  it('is offered on the login only where passkeys can be used', () => {
    const on = setup('signin-login', worker());
    expect(button(en['factorSignin.usePasskey'])).toHaveAttribute('type', 'button');
    on.view.unmount();
    const off = setup('signin-login', worker(), {}, { webauthn: false });
    expect(screen.queryByRole('button', { name: en['factorSignin.usePasskey'] })).toBeNull();
    off.view.unmount();
    setup('signin-login', worker(), { instance: { ...INSTANCE, passwordSignup: true, passkeys: false } });
    expect(screen.queryByRole('button', { name: en['factorSignin.usePasskey'] })).toBeNull();
  });

  it('signs in with a passkey on its own: the instance options, user verification required, then the key as step 2', async () => {
    const { fake, user } = setup('signin-login', worker({ needs: 0, passkeyNeeds: 0 }));
    get.mockResolvedValue(assertion());
    await user.click(button(en['factorSignin.usePasskey']));
    await waitFor(() => { expect(h1()).toHaveTextContent('Your recovery key'); });
    // The passkey was used on the login step, so no factor step was shown (R7).
    expect(screen.getByText('Sign in · step 2 of 3')).toBeInTheDocument();
    expect(fake.callsOf('signInPasskeyBegin')).toEqual([{ m: 'signInPasskeyBegin' }]);
    expect(get.mock.calls[0]?.[0].publicKey.userVerification).toBe('required');
    const sent = fake.callsOf('signInPasskey');
    expect(sent).toHaveLength(1);
    expect(sent[0]).toMatchObject({ m: 'signInPasskey', ceremonyId: CEREMONY });
    expect(Object.keys(sent[0] ?? {}).sort()).toEqual(['ceremonyId', 'm', 'response']);
    expect(typeof (sent[0] as { response: string }).response).toBe('string');
    expect(fake.callsOf('signInLogin')).toEqual([]);
    await addWithKey(user);
    expect(fake.callsOf('signInKey')).toEqual([{ m: 'signInKey', recoveryKey: GROUPED }]);
    expect(h1()).toHaveTextContent('You’re in');
  });

  it('shows the passkey step for an account that owes only a passkey: no code field, no app wording', async () => {
    const { fake, user, view } = setup('signin-login', worker({ needs: 2 }));
    await login(user);
    expect(h1()).toHaveTextContent('Use your passkey');
    expect(screen.getByText('Sign in · step 2 of 4')).toBeInTheDocument();
    expect(screen.getByText(en['factorSignin.passkey.body'])).toBeInTheDocument();
    expect(screen.queryByRole('textbox', { name: 'Code' })).toBeNull();
    expect(screen.queryByText(/authenticator app/i)).toBeNull();
    expect(button(en['factorSignin.useBackup'])).toBeInTheDocument();
    await expectNoAxeViolations(view.container);
    get.mockResolvedValue(assertion());
    await user.click(button('Use passkey'));
    await waitFor(() => { expect(h1()).toHaveTextContent('Your recovery key'); });
    expect(screen.getByText('Sign in · step 3 of 4')).toBeInTheDocument();
    expect(fake.callsOf('signInPasskey')).toHaveLength(1);
    expect(fake.callsOf('signInPasskey')[0]).not.toHaveProperty('recoveryKey');
  });

  it('a passkey-only account can use a backup code', async () => {
    const { fake, user } = setup('signin-login', worker({ needs: 2 }));
    await login(user);
    await user.click(button(en['factorSignin.useBackup']));
    expect(h1()).toHaveTextContent('Use a backup code');
    await user.type(field('Backup code'), `${BACKUP}{Enter}`);
    expect(fake.callsOf('signInBackupCode')).toEqual([]);
    await addWithKey(user);
    expect(fake.callsOf('signInBackupCode')).toEqual([{ m: 'signInBackupCode', code: BACKUP, recoveryKey: GROUPED }]);
  });

  const refusals: [string, () => void, string][] = [
    ['an unknown passkey, by its code', () => { get.mockResolvedValue(assertion()); }, 'E_PASSKEY_UNKNOWN'],
    ['any other refusal', () => { get.mockResolvedValue(assertion()); }, 'E_UNAUTHENTICATED'],
  ];
  it.each(refusals)('shows the right banner for %s', async (_name, arm, code) => {
    const script: Script = (c, f) => c.m === 'signInPasskey'
      ? Promise.reject(refusal({ code, status: 401, detail: code === 'E_UNAUTHENTICATED' ? 'unknown passkey' : 'x' }))
      : worker()(c, f);
    const { user } = setup('signin-login', script);
    arm();
    await user.click(button(en['factorSignin.usePasskey']));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(code === 'E_PASSKEY_UNKNOWN'
      ? 'dilla.test does not know this passkey. It may belong to another account or another server.'
      : en['factorSignin.passkey.failed']);
    expect(document.activeElement).toBe(button(en['factorSignin.usePasskey']));
  });

  it('a cancelled ceremony shows the failed text and returns focus to the button that started it', async () => {
    const { fake, user } = setup('signin-login', worker());
    get.mockRejectedValue(new DOMException('no', 'NotAllowedError'));
    await user.click(button(en['factorSignin.usePasskey']));
    expect(await screen.findByRole('alert')).toHaveTextContent(en['factorSignin.passkey.failed']);
    expect(document.activeElement).toBe(button(en['factorSignin.usePasskey']));
    expect(fake.callsOf('signInPasskey')).toEqual([]);
  });

  it('on the passkey step a cancelled ceremony keeps the step and focuses its button', async () => {
    const { user } = setup('signin-login', worker({ needs: 2 }));
    await login(user);
    get.mockRejectedValue(new DOMException('no', 'NotAllowedError'));
    await user.click(button('Use passkey'));
    expect(await screen.findByRole('alert')).toHaveTextContent(en['factorSignin.passkey.failed']);
    expect(h1()).toHaveTextContent('Use your passkey');
    expect(document.activeElement).toBe(button('Use passkey'));
  });
});

describe('set-up on a new browser (flow 10 state 1, origin signin)', () => {
  it('shows the set-up as step 3 of 4 with no cancel, then the done step as 4 of 4', async () => {
    const script: Script = (c, f) => {
      switch (c.m) {
        case 'factorTotpBegin': return Promise.resolve({ secret: SECRET, uri: `otpauth://totp/x?secret=${SECRET}` });
        case 'factorTotpConfirm': return Promise.resolve({ backupCodes: [BACKUP] });
        case 'factorSetupDone': move(f, readyMe); return Promise.resolve({ communityId: null, joinError: null });
        default: return Promise.resolve(null);
      }
    };
    const { fake, user } = setup('factor-setup', script, { factorSetup: { origin: 'signin' }, user: ME });
    fake.set('factors', NO_FACTORS);
    expect(h1()).toHaveTextContent('Add a second factor');
    expect(screen.getByText('Sign in · step 3 of 4')).toBeInTheDocument();
    // An allow-list: the choice's Continue and the exit that clears this browser, nothing else.
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual([en['factor.choose.next'], en['devices.forget']]);
    await user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    await user.type(field(en['factor.totp.code']), '123456{Enter}');
    await user.click(await screen.findByRole('checkbox'));
    await user.click(button('Continue'));
    expect(fake.callsOf('factorSetupDone')).toEqual([{ m: 'factorSetupDone', invite: null }]);
    await waitFor(() => { expect(h1()).toHaveTextContent('You’re in'); });
    expect(screen.getByText('Sign in · step 4 of 4')).toBeInTheDocument();
  });

  it('goes back to the choice, with no error line, when the factor is still owed', async () => {
    // web3.6 E3: the worker refuses E_FACTOR_REQUIRED with no HTTP status; the choice's own copy says what is owed.
    const script: Script = c => c.m === 'factorSetupDone'
      ? Promise.reject(refusal({ code: 'E_FACTOR_REQUIRED' }))
      : Promise.resolve({ secret: SECRET, uri: 'otpauth://totp/x', backupCodes: [BACKUP] });
    const { user, fake } = setup('factor-setup', script, { factorSetup: { origin: 'signin' }, user: ME });
    fake.set('factors', NO_FACTORS);
    await user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    await user.type(field(en['factor.totp.code']), '123456{Enter}');
    await user.click(await screen.findByRole('checkbox'));
    await user.click(button('Continue'));
    await waitFor(() => { expect(h1()).toHaveTextContent(en['factor.choose.title']); });
    expect(screen.queryByRole('alert')).toBeNull();
    expect(fake.callsOf('factorSetupDone')).toHaveLength(1);
  });

  it('retries factorSetupDone from the same button after a refusal, once at a time', async () => {
    let n = 0;
    const script: Script = (c, f) => {
      if (c.m === 'factorSetupDone') {
        n += 1;
        if (n === 1) return Promise.reject(refusal({ code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 2000 }));
        move(f, readyMe);
        return Promise.resolve({ communityId: null, joinError: null });
      }
      return Promise.resolve({ secret: SECRET, uri: 'otpauth://totp/x', backupCodes: [BACKUP] });
    };
    const { user, fake } = setup('factor-setup', script, { factorSetup: { origin: 'signin' }, user: ME });
    fake.set('factors', NO_FACTORS);
    await user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    await user.type(field(en['factor.totp.code']), '123456{Enter}');
    await user.click(await screen.findByRole('checkbox'));
    await user.click(button('Continue'));
    expect(await screen.findByRole('alert')).toHaveTextContent('Too many attempts. Try again in 2 s.');
    expect(screen.getAllByRole('listitem').length).toBeGreaterThan(0);
    await user.dblClick(button('Continue'));
    await waitFor(() => { expect(h1()).toHaveTextContent('You’re in'); });
    expect(fake.callsOf('factorSetupDone')).toHaveLength(2);
  });

  it('has one live region, and a copy is spoken once', async () => {
    const script: Script = () => Promise.resolve({ secret: SECRET, uri: `otpauth://totp/x?secret=${SECRET}` });
    const { fake, user, view } = setup('factor-setup', script, { factorSetup: { origin: 'signin' }, user: ME });
    fake.set('factors', NO_FACTORS);
    expect(liveRegions(view.container)).toHaveLength(1);
    await user.click(button('Continue'));
    await screen.findByRole('img', { name: /QR code/ });
    const region = screen.getByTestId('signin-announcer');
    expect(liveRegions(view.container)).toEqual([region]);
    const watch = watchRegion(region);
    await user.click(button('Copy'));
    await waitFor(() => { expect(watch.history).toEqual([en['factor.totp.keyCopied']]); });
    await new Promise(r => setTimeout(r, 450));
    expect(watch.history).toEqual([en['factor.totp.keyCopied']]);
    watch.stop();
  });

  it('offers the exit that clears this browser, and sends nothing else on confirm', async () => {
    const { fake, user } = setup('factor-setup', () => Promise.resolve(null), { factorSetup: { origin: 'signin' }, user: ME });
    fake.set('factors', NO_FACTORS);
    const forget = () => screen.getByRole('button', { name: en['devices.forget'] });
    for (let i = 0; i < 8 && document.activeElement !== forget(); i++) await user.tab();
    expect(document.activeElement).toBe(forget());
    await user.keyboard('{Enter}');
    await user.click(await screen.findByRole('button', { name: en['signin.cancel'] }));
    expect(document.activeElement).toBe(forget());
    expect(fake.callsOf('forgetBrowser')).toEqual([]);
    await user.click(forget());
    await user.click(await screen.findByRole('button', { name: en['devices.confirmForget'] }));
    expect(fake.callsOf('forgetBrowser')).toEqual([{ m: 'forgetBrowser' }]);
    expect(fake.callsOf('factorSetupDone')).toEqual([]);
  });

  it('a 429 or a dropped connection on the last step says what happened', async () => {
    for (const [r, text] of [[{ code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 2000 }, 'Too many attempts. Try again in 2 s.'],
      [{ code: 'E_NETWORK', status: 0 }, en['factor.error.network']]] as const) {
      const script: Script = c => c.m === 'factorSetupDone' ? Promise.reject(refusal(r))
        : Promise.resolve({ secret: SECRET, uri: 'otpauth://totp/x', backupCodes: [BACKUP] });
      const { user, fake, view } = setup('factor-setup', script, { factorSetup: { origin: 'signin' }, user: ME });
      fake.set('factors', NO_FACTORS);
      await user.click(button('Continue'));
      await screen.findByRole('img', { name: /QR code/ });
      await user.type(field(en['factor.totp.code']), '123456{Enter}');
      await user.click(await screen.findByRole('checkbox'));
      await user.click(button('Continue'));
      expect(await screen.findByRole('alert')).toHaveTextContent(text);
      view.unmount();
    }
  });
});

describe('backup codes, tightened', () => {
  it('rejects a 26-character code with a digit the alphabet does not have', async () => {
    const { fake, user } = setup('signin-login', worker());
    await login(user);
    await user.click(button(en['factorSignin.useBackup']));
    // 26 characters, but 0, 1, 8 and 9 are not in the alphabet (A-Z, 2-7).
    for (const bad of ['ABCDEFGHIJKLMNOPQRSTUVWXY0', 'ABCDEFGHIJKLMNOPQRSTUVWXY1', 'ABCDEFGHIJKLMNOPQRSTUVWXY8', 'ABCDEFGHIJKLMNOPQRSTUVWXY9']) {
      await user.clear(field('Backup code'));
      await user.type(field('Backup code'), `${bad}{Enter}`);
      expect(field('Backup code')).toHaveAccessibleDescription(en['factorSignin.backup.format']);
    }
    expect(fake.callsOf('signInBackupCode')).toEqual([]);
    await user.clear(field('Backup code'));
    await user.type(field('Backup code'), 'ABCDEFGHIJKLMNOPQRSTUVWXY7{Enter}');
    expect(fake.callsOf('signInBackupCode')).toEqual([]);
    await addWithKey(user);
    expect(fake.callsOf('signInBackupCode')).toHaveLength(1);
  });

  it('E_NO_ASSERTION on a backup code reads like a refused code', async () => {
    const script: Script = (c, f) => {
      if (c.m === 'signInBackupCode') {
        move(f, { phase: 'signin-login', signIn: { username: 'ada', factor: null } });
        return Promise.reject(refusal({ code: 'E_NO_ASSERTION', status: 401 }));
      }
      return worker()(c, f);
    };
    const { user } = setup('signin-login', script);
    await login(user);
    await user.click(button(en['factorSignin.useBackup']));
    await user.type(field('Backup code'), `${BACKUP}{Enter}`);
    await addWithKey(user);
    expect(await screen.findByRole('alert')).toHaveTextContent(en['factorSignin.backup.failed']);
    expect(document.activeElement).toBe(password());
  });
});

describe('a passkey-only account in a browser without passkeys', () => {
  it('goes straight to the backup-code form: no passkey button that can only fail, and Cancel instead of Back', async () => {
    const { fake, user } = setup('signin-login', worker({ needs: 2 }), {}, { webauthn: false });
    await login(user);
    expect(h1()).toHaveTextContent('Use a backup code');
    expect(screen.queryByRole('button', { name: 'Use passkey' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Back' })).toBeNull();
    expect(screen.getByText(en['factorSignin.passkey.unusable'])).toBeInTheDocument();
    await user.type(field('Backup code'), `${BACKUP}{Enter}`);
    await addWithKey(user);
    expect(fake.callsOf('signInBackupCode')).toHaveLength(1);
    expect(fake.callsOf('signInPasskeyBegin')).toEqual([]);
  });
  it('Cancel leaves the ceremony', async () => {
    const { fake, user } = setup('signin-login', worker({ needs: 2 }), {}, { webauthn: false });
    await login(user);
    await user.click(button('Cancel'));
    expect(fake.callsOf('signInCancel')).toHaveLength(1);
  });
});
