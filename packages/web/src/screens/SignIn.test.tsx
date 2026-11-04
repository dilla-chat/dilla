import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { act, fireEvent, render, screen } from '@testing-library/react';
import userEvent, { type UserEvent } from '@testing-library/user-event';
import type { AccountState, BootPhase, Command } from '@dilla/client-core';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient, refusal } from '../test/fake-client.ts';
import { account, INSTANCE } from '../test/fixtures.ts';
import { expectNoAxeViolations } from '../test/setup.ts';
import { en } from '../strings/en.ts';
import { SignIn } from './SignIn.tsx';
import { COMPLETE_DELAY_MS } from '@dilla/ui';
// Wave B task 3 fix round 1: the code boxes send the sixth digit after the prototype's 350 ms.
const settleCode = () => act(async () => { await new Promise<void>((resolve) => { setTimeout(resolve, COMPLETE_DELAY_MS + 20); }); });


const KEY = '7K2MQX9DH4TBR8NWC3VFJ6PZA1GEY5KSM0QTB7XHW2DNF9RCP4ZA';
const GROUPED = '7K2M-QX9D-H4TB-R8NW-C3VF-J6PZ-A1GE-Y5KS-M0QT-B7XH-W2DN-F9RC-P4ZA';
const ME = { id: 'bb'.repeat(16), username: 'ada' };
const LENGTH = 'A recovery key has 52 characters.';
type Script = (c: Command, fake: FakeClient) => Promise<unknown>;
const idle: Script = () => Promise.resolve(null);

function move(fake: FakeClient, over: Partial<AccountState>): void {
  const current = fake.get('account');
  if (current === undefined) throw new Error('no account slice');
  fake.set('account', { ...current, ...over });
}
function setup(phase: BootPhase, script: Script = idle, over: Partial<AccountState> = {}) {
  const fake = new FakeClient();
  fake.set('account', account({
    phase, user: null, deviceId: null, instance: { ...INSTANCE, passwordSignup: true },
    signIn: { username: null, factor: null }, ...over,
  }));
  fake.handler = c => script(c, fake);
  const onFinish = vi.fn();
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><SignIn onFinish={onFinish} /></CoreProvider></div>);
  return { fake, onFinish, user, view };
}
const h1 = () => screen.getByRole('heading', { level: 1 });
const button = (name: string) => screen.getByRole('button', { name });
const field = (name: string) => screen.getByRole('textbox', { name });
const password = () => screen.getByLabelText('Password');


const BACKUP = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ';
const KEY_BODY = 'This browser is new. Paste your recovery key to read your conversations here.';
const EXPIRED = 'Adding this browser took longer than 5 minutes, so the sign-in ran out. Sign in again.';
const EXPIRED_ERROR = { code: 'E_SIGNIN_EXPIRED', detail: '', status: 0, retryAfterMs: null };
const readyMe = { phase: 'ready', user: ME, deviceId: 'dd'.repeat(16), signIn: null } as const;

/** The worker of web3.8 (R6) as the page sees it: the login (and the code) hold the assertion and end at signin-key;
 *  only signInKey, or signInBackupCode with the key, adds this browser. */
const worker = (opts: { totp?: boolean } = {}): Script => (c, fake) => {
  switch (c.m) {
    case 'signInLogin':
      if (opts.totp) move(fake, { phase: 'signin-totp', signIn: { username: c.username, factor: 'totp' } });
      else move(fake, { phase: 'signin-key', signIn: { username: c.username, factor: null } });
      return Promise.resolve({ needs: opts.totp === true ? 1 : 0 });
    case 'signInTotp':
      move(fake, { phase: 'signin-key', signIn: { username: 'ada', factor: null } });
      return Promise.resolve(null);
    case 'signInKey': case 'signInBackupCode':
      move(fake, readyMe);
      return Promise.resolve(null);
    case 'signInCancel':
      move(fake, { phase: 'needs-signup', signIn: null });
      return Promise.resolve(null);
    case 'signInBack':
      move(fake, { phase: 'signin-login', signIn: { username: fake.get('account')!.signIn?.username ?? null, factor: null } });
      return Promise.resolve(null);
    default:
      return Promise.resolve(null);
  }
};
/** Step 1: the username and the password. */
async function login(user: UserEvent): Promise<void> {
  await user.type(field('Username'), ' Ada ');
  await user.type(password(), 'correct horse');
  await user.click(button('Continue'));
}
async function pasteKey(user: UserEvent, text = GROUPED): Promise<void> {
  await user.click(field('Recovery key'));
  await user.paste(text);
}

describe('the ceremony', () => {
  // web3.8 (R6, R7): the login first, then the second factor, then the recovery key. Nothing registers before the key:
  // the worker holds the assertion, and the key's command is the only one that carries the key.
  it('starts at the login, then asks for the key as step 2 when no factor step was shown, then done', async () => {
    const { fake, user, onFinish, view } = setup('signin-login', worker());
    expect(h1()).toHaveTextContent('Sign in to dilla.test');
    expect(document.activeElement).toBe(h1());
    expect(screen.getByText('Sign in · step 1 of 4')).toBeInTheDocument();
    expect(screen.getByText('Use your dilla username and password.')).toBeInTheDocument();
    expect(screen.queryByRole('textbox', { name: 'Recovery key' })).toBeNull();
    expect(button('Continue')).toHaveAttribute('type', 'submit');
    expect(button('Have an invite? Create an account')).toHaveAttribute('type', 'button');
    expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull();
    await expectNoAxeViolations(view.container);
    await login(user);
    expect(fake.callsOf('signInLogin')).toEqual([{ m: 'signInLogin', username: 'ada', password: 'correct horse' }]);
    expect(h1()).toHaveTextContent('Add this browser');
    expect(document.activeElement).toBe(h1());
    expect(screen.getByText('Sign in · step 2 of 3')).toBeInTheDocument();
    expect(screen.getByText(KEY_BODY)).toBeInTheDocument();
    expect(button('Have an invite? Create an account')).toHaveAttribute('type', 'button');
    expect(button('Add browser')).toHaveAttribute('type', 'submit');
    await expectNoAxeViolations(view.container);
    expect(fake.callsOf('signInKey')).toEqual([]);
    await pasteKey(user);
    expect(field('Recovery key')).toHaveAccessibleDescription(expect.stringContaining('52 of 52 characters'));
    await user.click(button('Add browser'));
    expect(fake.callsOf('signInKey')).toEqual([{ m: 'signInKey', recoveryKey: GROUPED }]);
    expect(h1()).toHaveTextContent('You’re in');
    expect(document.activeElement).toBe(h1());
    expect(screen.getByText('Sign in · step 3 of 3')).toBeInTheDocument();
    expect(screen.getByText('This browser is now one of your devices.')).toBeInTheDocument();
    expect(screen.queryByRole('textbox')).toBeNull();
    await expectNoAxeViolations(view.container);
    expect(onFinish).not.toHaveBeenCalled();
    await user.click(button('Open dilla'));
    expect(onFinish).toHaveBeenCalledTimes(1);
  });

  it('counts the second-factor step when the account has one: the code is step 2, the key step 3', async () => {
    const { fake, user, view } = setup('signin-login', worker({ totp: true }));
    await login(user);
    expect(h1()).toHaveTextContent('Enter your code');
    expect(screen.getByText('Sign in · step 2 of 4')).toBeInTheDocument();
    expect(screen.getByText('Six digits from your authenticator app.')).toBeInTheDocument();
    expect(button('Have an invite? Create an account')).toHaveAttribute('type', 'button');
    await expectNoAxeViolations(view.container);
    await user.type(field('Code'), '123456');
    await settleCode();
    expect(fake.callsOf('signInTotp')).toEqual([{ m: 'signInTotp', code: '123456' }]);
    expect(h1()).toHaveTextContent('Add this browser');
    expect(document.activeElement).toBe(h1());
    expect(screen.getByText('Sign in · step 3 of 4')).toBeInTheDocument();
    expect(screen.getByText(KEY_BODY)).toBeInTheDocument();
    await expectNoAxeViolations(view.container);
    await pasteKey(user);
    await user.click(button('Add browser'));
    expect(fake.callsOf('signInKey')).toEqual([{ m: 'signInKey', recoveryKey: GROUPED }]);
    expect(h1()).toHaveTextContent('You’re in');
    expect(screen.getByText('Sign in · step 4 of 4')).toBeInTheDocument();
  });

  it('holds a backup code in the page and sends it only with the key', async () => {
    const { fake, user, view } = setup('signin-login', worker({ totp: true }));
    await login(user);
    fake.set('factors', { owed: false, totp: { confirmedAt: 1 }, passkeys: [], backupCodesLeft: 9, hasPassword: true });
    await user.click(button(en['factorSignin.useBackup']));
    expect(screen.getByText('Sign in · step 2 of 4')).toBeInTheDocument();
    await user.type(field('Backup code'), 'abcde fghij-klmno pqrst uvwxy z{Enter}');
    expect(fake.callsOf('signInBackupCode')).toEqual([]);
    expect(h1()).toHaveTextContent('Add this browser');
    expect(document.activeElement).toBe(h1());
    expect(screen.getByText('Sign in · step 3 of 4')).toBeInTheDocument();
    expect(screen.getByText(KEY_BODY)).toBeInTheDocument();
    await expectNoAxeViolations(view.container);
    await pasteKey(user);
    await user.click(button('Add browser'));
    expect(fake.callsOf('signInBackupCode')).toEqual([{ m: 'signInBackupCode', code: BACKUP, recoveryKey: GROUPED }]);
    expect(fake.callsOf('signInKey')).toEqual([]);
    expect(fake.calls.map(c => c.m)).toEqual(['signInLogin', 'signInBackupCode']);
    expect(h1()).toHaveTextContent('You’re in');
    expect(screen.getByText('You have 9 backup codes left. Make new ones in Settings, under sign-in.')).toBeInTheDocument();
  });

  it('an empty submit shows the required text and sends nothing', async () => {
    const { fake, user } = setup('signin-login', worker());
    await user.click(button('Continue'));
    expect(field('Username')).toHaveAccessibleDescription('Fill in this field.');
    expect(password()).toHaveAccessibleDescription('Fill in this field.');
    expect(document.activeElement).toBe(field('Username'));
    expect(field('Username')).toBeRequired();
    expect(password()).toBeRequired();
    await user.type(field('Username'), 'ada');
    expect(field('Username')).not.toHaveAccessibleDescription(expect.stringContaining('Fill in'));
    await user.click(button('Continue'));
    expect(field('Username')).not.toHaveAccessibleDescription(expect.stringContaining('Fill in'));
    expect(password()).toHaveAccessibleDescription('Fill in this field.');
    expect(document.activeElement).toBe(password());
    expect(fake.callsOf('signInLogin')).toEqual([]);
  });

  it('takes a pasted code its authenticator grouped, without the space (A11Y-DESIGN-06)', async () => {
    const { fake, user } = setup('signin-login', worker({ totp: true }));
    await login(user);
    await user.click(field('Code'));
    // Wave B task 3 (Review Focus 1): the boxes keep the digits, all six fill, and the code goes once with no press.
    await user.paste('123 456');
    await settleCode();
    expect(fake.callsOf('signInTotp')).toEqual([{ m: 'signInTotp', code: '123456' }]);
    await user.keyboard('{Enter}');
    expect(fake.callsOf('signInTotp')).toHaveLength(1);
  });

  it('a complete code submits once however often Enter and Verify follow it, and a short code is never sent', async () => {
    let settle: () => void = () => {};
    const { fake, user } = setup('signin-totp', (c, f) => {
      if (c.m !== 'signInTotp') return worker({ totp: true })(c, f);
      return new Promise(resolve => { settle = () => { move(f, { phase: 'signin-key', signIn: { username: 'ada', factor: null } }); resolve(null); }; });
    }, { signIn: { username: 'ada', factor: 'totp' } });
    await user.type(field('Code'), '12345{Enter}');
    expect(field('Code')).toHaveAccessibleDescription('Six digits from your authenticator app. A code has six digits.');
    expect(fake.callsOf('signInTotp')).toEqual([]);
    expect(button('Verify')).toHaveAttribute('aria-disabled', 'true');
    await user.type(field('Code'), '6');
    await user.keyboard('{Enter}');
    await user.click(button('Checking…'));
    expect(fake.callsOf('signInTotp')).toEqual([{ m: 'signInTotp', code: '123456' }]);
    await act(async () => { settle(); await Promise.resolve(); });
    expect(h1()).toHaveTextContent('Add this browser');
    expect(fake.callsOf('signInTotp')).toHaveLength(1);
  });

  it('a key refused by its form after the login stays on the key step with the text kept and the field in error', async () => {
    const { fake, user } = setup('signin-login', (c, f) => {
      if (c.m !== 'signInKey') return worker()(c, f);
      move(f, { phase: 'signin-key', error: { code: 'E_RECOVERY_KEY', detail: '', status: 0, retryAfterMs: null } });
      return Promise.reject(refusal({ code: 'E_RECOVERY_KEY' }));
    });
    const typo = GROUPED.replace('P4ZA', 'P4ZU');
    await login(user);
    await pasteKey(user, typo);
    await user.click(button('Add browser'));
    expect(fake.callsOf('signInLogin')).toEqual([{ m: 'signInLogin', username: 'ada', password: 'correct horse' }]);
    expect(fake.callsOf('signInKey')).toEqual([{ m: 'signInKey', recoveryKey: typo }]);
    expect(h1()).toHaveTextContent('Add this browser');
    expect(field('Recovery key')).toHaveValue(typo);
    expect(field('Recovery key')).toHaveAccessibleDescription(expect.stringContaining('This is not the recovery key of this account. Check every character.'));
    expect(document.activeElement).toBe(field('Recovery key'));
  });

  it('keeps the status region in place on the key step, empty until the step works (A11Y-DESIGN-04)', () => {
    setup('signin-key');
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });

  it('an empty code shows the required text and sends nothing', async () => {
    const { fake, user } = setup('signin-totp', worker({ totp: true }), { signIn: { username: 'ada', factor: 'totp' } });
    await user.click(button('Verify'));
    expect(field('Code')).toHaveAccessibleDescription('Six digits from your authenticator app. Fill in this field.');
    expect(document.activeElement).toBe(field('Code'));
    expect(fake.callsOf('signInTotp')).toEqual([]);
  });

  it('blocks a second login while the first is in flight, keeping focus on the button', async () => {
    let calls = 0;
    const { user } = setup('signin-login', c => {
      if (c.m === 'signInLogin') calls += 1;
      return new Promise(() => {});
    });
    await login(user);
    expect(button('Checking…')).toHaveAttribute('aria-disabled', 'true');
    expect(document.activeElement).toBe(button('Checking…'));
    expect(button('Have an invite? Create an account')).toHaveAttribute('aria-disabled', 'true');
    await user.click(button('Checking…'));
    await user.click(button('Have an invite? Create an account'));
    expect(calls).toBe(1);
  });

  it('pastes a key in any grouping and case, and counts what the key will be', async () => {
    const { user } = setup('signin-key');
    await pasteKey(user, GROUPED.toLowerCase().replaceAll('-', ' '));
    expect(field('Recovery key')).toHaveValue(GROUPED.toLowerCase().replaceAll('-', ' '));
    expect(field('Recovery key')).toHaveAccessibleDescription(expect.stringContaining('52 of 52 characters'));
    await user.clear(field('Recovery key'));
    await user.type(field('Recovery key'), 'il1o0');
    expect(field('Recovery key')).toHaveAccessibleDescription(expect.stringContaining('5 of 52 characters'));
  });

  it('blocks Add this browser until the key has 52 characters, and says why', async () => {
    const { fake, user } = setup('signin-key', worker());
    await user.type(field('Recovery key'), KEY.slice(0, 51));
    expect(field('Recovery key')).toHaveAccessibleDescription(expect.stringContaining('51 of 52 characters'));
    expect(button('Add browser')).toHaveAttribute('aria-disabled', 'true');
    expect(button('Add browser')).toHaveAccessibleDescription(LENGTH);
    await user.keyboard('{Enter}');
    expect(fake.callsOf('signInKey')).toEqual([]);
    expect(document.activeElement).toBe(field('Recovery key'));
    expect(field('Recovery key')).toHaveAccessibleDescription(expect.stringContaining(LENGTH));
    await user.click(button('Add browser'));
    expect(fake.callsOf('signInKey')).toEqual([]);
    await user.type(field('Recovery key'), KEY.slice(51));
    expect(button('Add browser')).not.toHaveAttribute('aria-disabled');
    expect(screen.queryByText(LENGTH)).toBeNull();
    await user.click(button('Add browser'));
    expect(fake.callsOf('signInKey')).toEqual([{ m: 'signInKey', recoveryKey: KEY }]);
  });

  it('shows the working state while this browser is added, with nothing to press', async () => {
    const { fake, user } = setup('signin-key', (c, f) => {
      if (c.m === 'signInKey') move(f, { phase: 'enrolling' });
      return new Promise(() => {});
    });
    await pasteKey(user);
    await user.click(button('Add browser'));
    expect(h1()).toHaveTextContent('Add this browser');
    expect(screen.getByRole('status')).toHaveTextContent('Adding this browser to your account…');
    expect(button('Add browser')).toHaveAttribute('aria-disabled', 'true');
    expect(button('Have an invite? Create an account')).toHaveAttribute('aria-disabled', 'true');
    expect(document.activeElement).toBe(button('Add browser'));
    await user.click(button('Add browser'));
    await user.click(button('Have an invite? Create an account'));
    expect(fake.callsOf('signInKey')).toHaveLength(1);
    expect(fake.callsOf('signInCancel')).toEqual([]);
  });

  it('keeps its place after a reload into a registered enrolment, as step 2', () => {
    setup('signin-key');
    expect(h1()).toHaveTextContent('Add this browser');
    expect(screen.getByText('Sign in · step 2 of 3')).toBeInTheDocument();
    expect(field('Recovery key')).toHaveValue('');
    expect(button('Have an invite? Create an account')).toHaveAttribute('type', 'button');
  });

  it('shows the working state when it mounts in enrolling', () => {
    const { fake } = setup('enrolling');
    expect(h1()).toHaveTextContent('Add this browser');
    expect(screen.getByRole('status')).toHaveTextContent('Adding this browser to your account…');
    expect(fake.calls).toEqual([]);
  });

  it('cancels from every step', async () => {
    const first = setup('signin-login', worker());
    await first.user.click(button('Have an invite? Create an account'));
    expect(first.fake.callsOf('signInCancel')).toEqual([{ m: 'signInCancel' }]);
    first.view.unmount();
    const second = setup('signin-totp', worker({ totp: true }), { signIn: { username: 'ada', factor: 'totp' } });
    await second.user.click(button('Have an invite? Create an account'));
    expect(second.fake.callsOf('signInCancel')).toEqual([{ m: 'signInCancel' }]);
    second.view.unmount();
    const third = setup('signin-key', worker());
    await third.user.click(button('Have an invite? Create an account'));
    expect(third.fake.callsOf('signInCancel')).toEqual([{ m: 'signInCancel' }]);
    expect(third.fake.callsOf('signInKey')).toEqual([]);
  });

  it('wave B task 3: Back on the code step returns to the login with the code dropped and the password asked again', async () => {
    const { fake, user } = setup('signin-login', worker({ totp: true }));
    await login(user);
    expect(h1()).toHaveTextContent('Enter your code');
    await user.type(field('Code'), '123');
    await user.click(button('Back'));
    expect(fake.callsOf('signInBack')).toEqual([{ m: 'signInBack' }]);
    expect(fake.callsOf('signInCancel')).toEqual([]);
    expect(fake.callsOf('signInTotp')).toEqual([]);
    expect(h1()).toHaveTextContent('Sign in to dilla.test');
    expect(field('Username')).toHaveValue(' Ada ');
    expect(password()).toHaveValue('');
    expect(document.activeElement).toBe(password());
    // A second login shows an empty code: the three digits typed before Back are gone.
    await user.type(password(), 'correct horse');
    await user.click(button('Continue'));
    expect(field('Code')).toHaveValue('');
  });

  it('wave B task 3: Back on the key step drops the typed key and sends no key', async () => {
    const { fake, user } = setup('signin-login', worker());
    await login(user);
    await pasteKey(user);
    await user.click(button('Back'));
    expect(fake.callsOf('signInBack')).toEqual([{ m: 'signInBack' }]);
    expect(fake.callsOf('signInKey')).toEqual([]);
    expect(h1()).toHaveTextContent('Sign in to dilla.test');
  });

  it('wave B task 3: Back and the new-account link sit under the button on the code and key steps, blocked while a step works', async () => {
    const { user } = setup('signin-key', (c, f) => {
      if (c.m === 'signInKey') move(f, { phase: 'enrolling' });
      return new Promise(() => {});
    });
    expect(button('Back')).toHaveAttribute('type', 'button');
    expect(button('Back').closest('.d-onboarding-frame__nav')).not.toBeNull();
    await pasteKey(user);
    await user.click(button('Add browser'));
    expect(button('Back')).toHaveAttribute('aria-disabled', 'true');
  });

  it('wave B task 3: the done step names the first server and its first text channel in the green panel', () => {
    const { fake, view } = setup('ready', idle, { user: ME, deviceId: 'dd'.repeat(16), signIn: null });
    expect(view.container.querySelector('.d-onboarding-frame__ready')).toBeNull();
    const cid = 'c1'.repeat(16);
    const row = (id: string, kind: 0 | 1 | 2, name: string, position: number, parentId: string | null = null) => ({
      id, communityId: cid, kind, mode: 0 as const, name, topic: '', parentId, position, slowmode: 0, announcement: false, group: 'active' as const });
    act(() => {
      fake.set('communities', [{ id: cid, name: 'Midgard Crew', owner: 'aa'.repeat(16) }]);
      fake.set(`channels:${cid}`, [row('v1'.repeat(16), 1, 'harbour', 0), row('t2'.repeat(16), 0, 'random', 2), row('t1'.repeat(16), 0, 'general', 1)]);
    });
    expect(screen.getByText('Midgard Crew is waiting in #general.')).toBeInTheDocument();
  });

  it('fix round 1: "Open dilla" lands on the server and channel the panel names; without a panel, on the default', async () => {
    const named = setup('ready', idle, { user: ME, deviceId: 'dd'.repeat(16), signIn: null });
    const cid = 'c1'.repeat(16);
    act(() => {
      named.fake.set('communities', [{ id: cid, name: 'Midgard Crew', owner: 'aa'.repeat(16) }]);
      named.fake.set(`channels:${cid}`, [{ id: 't1'.repeat(16), communityId: cid, kind: 0, mode: 0, name: 'general', topic: '', parentId: null,
        position: 0, slowmode: 0, announcement: false, group: 'active' as const }]);
    });
    await named.user.click(button('Open dilla'));
    expect(named.onFinish).toHaveBeenCalledWith({ communityId: cid, channelId: 't1'.repeat(16) });
    named.view.unmount();
    const plain = setup('ready', idle, { user: ME, deviceId: 'dd'.repeat(16), signIn: null });
    await plain.user.click(button('Open dilla'));
    expect(plain.onFinish).toHaveBeenCalledWith(undefined);
  });

  it('wave B task 3: no panel when the first server has no text channel', () => {
    const { fake, view } = setup('ready', idle, { user: ME, deviceId: 'dd'.repeat(16), signIn: null });
    const cid = 'c1'.repeat(16);
    act(() => {
      fake.set('communities', [{ id: cid, name: 'Midgard Crew', owner: 'aa'.repeat(16) }]);
      fake.set(`channels:${cid}`, [{ id: 'v1'.repeat(16), communityId: cid, kind: 1, mode: 0, name: 'harbour', topic: '', parentId: null,
        position: 0, slowmode: 0, announcement: false, group: 'active' as const }]);
    });
    expect(view.container.querySelector('.d-onboarding-frame__ready')).toBeNull();
    expect(h1()).toHaveTextContent('You’re in');
  });

  it('fix round 1: the typed code is dropped by "Use a backup code" and stays dropped after Back', async () => {
    const { user } = setup('signin-login', worker({ totp: true }));
    await login(user);
    await user.type(field('Code'), '123');
    await user.click(button('Use a backup code'));
    expect(h1()).toHaveTextContent('Use a backup code');
    await user.click(button('Back'));
    expect(h1()).toHaveTextContent('Enter your code');
    expect(field('Code')).toHaveValue('');
  });

  it('fix round 1: a second Back while the first is on its way sends nothing', async () => {
    let release!: () => void;
    const { fake, user } = setup('signin-login', (c, f) => {
      if (c.m === 'signInBack') return new Promise(resolve => { release = () => { void worker()(c, f); resolve(null); }; });
      return worker({ totp: true })(c, f);
    });
    await login(user);
    await user.click(button('Back'));
    expect(button('Back')).toHaveAttribute('aria-disabled', 'true');
    await user.click(button('Back'));
    await user.click(button('Have an invite? Create an account'));
    expect(fake.callsOf('signInBack')).toHaveLength(1);
    expect(fake.callsOf('signInCancel')).toEqual([]);
    await act(async () => { release(); await Promise.resolve(); });
  });

  it('shows the loading splash without an instance', () => {
    setup('signin-login', idle, { instance: null });
    expect(screen.getByRole('status')).toHaveTextContent('Starting dilla');
  });
});

describe('refusals', () => {
  // E_ENROL_RATE is withdrawn (head ruling 38 as amended): registration answers no per-user 429. Every 429 of the
  // ceremony (the login route, the second-factor route, the registration meter) is E_RATE_LIMITED and shows the one
  // short-wait string signin.error.tooMany as flow 03 froze it (task 15).
  const LOGIN: [string, Parameters<typeof refusal>[0], string][] = [
    ['the login rate', { code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 4_200 }, 'Too many sign-in attempts. Try again in 5 s.'],
    ['the login rate without a wait', { code: 'E_RATE_LIMITED', status: 429 }, 'Too many attempts from this network. Wait a few minutes, then try again.'],
    ['a withdrawn code', { code: 'E_ENROL_RATE', status: 429, retryAfterMs: 1_740_001 }, 'Signing in did not work (E_ENROL_RATE). Try again.'],
    ['a dropped connection', { code: 'E_NETWORK' }, 'The connection dropped. Check it and try again.'],
    ['a server failure', { code: 'E_INTERNAL', status: 503 }, 'The connection dropped. Check it and try again.'],
    ['anything else', { code: 'E_SESSION_SCOPE' }, 'Signing in did not work (E_SESSION_SCOPE). Try again.'],
  ];
  it.each(LOGIN)('a refused login shows %s and stays', async (_, init, text) => {
    const { user } = setup('signin-login', c => (c.m === 'signInLogin' ? Promise.reject(refusal(init)) : Promise.resolve(null)));
    await login(user);
    expect(h1()).toHaveTextContent('Sign in to dilla.test');
    expect(screen.getByRole('alert')).toHaveTextContent(text);
    expect(button('Continue')).not.toHaveAttribute('aria-disabled');
    expect(document.activeElement).toBe(button('Continue'));
  });

  it('a wrong username or password is told on the password field', async () => {
    const { user } = setup('signin-login', c => (c.m === 'signInLogin'
      ? Promise.reject(refusal({ code: 'E_UNAUTHENTICATED', status: 401 })) : Promise.resolve(null)));
    await login(user);
    expect(password()).toHaveAccessibleDescription('That username and password did not work.');
    expect(document.activeElement).toBe(password());
    expect(field('Username')).toHaveValue(' Ada ');
    await user.type(password(), 'x');
    expect(password()).not.toHaveAccessibleDescription(expect.stringContaining('did not work'));
  });

  const TOTP: [string, Parameters<typeof refusal>[0]][] = [
    ['a refused code', { code: 'E_UNAUTHENTICATED', status: 401 }],
    ['a spent sign-in', { code: 'E_NO_ASSERTION' }],
  ];
  it.each(TOTP)('%s returns to the login step with the username kept and asks to sign in again', async (_, init) => {
    const { fake, user } = setup('signin-login', (c, f) => {
      if (c.m !== 'signInTotp') return worker({ totp: true })(c, f);
      move(f, { phase: 'signin-login', signIn: { username: 'ada', factor: 'totp' } });
      return Promise.reject(refusal(init));
    });
    await login(user);
    expect(screen.getByText('Sign in · step 2 of 4')).toBeInTheDocument();
    await user.type(field('Code'), '000000');
    await settleCode();
    expect(fake.callsOf('signInTotp')).toEqual([{ m: 'signInTotp', code: '000000' }]);
    expect(h1()).toHaveTextContent('Sign in to dilla.test');
    expect(screen.getByText('Sign in · step 1 of 4')).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent('That code did not work. Sign in again with a fresh code.');
    expect(screen.queryByRole('textbox', { name: 'Code' })).toBeNull();
    expect(field('Username')).toHaveValue(' Ada ');
    expect(password()).toHaveValue('');
    expect(document.activeElement).toBe(password());
  });

  // REGISTRATION-DEVICES-02 (R6): the registration runs inside signInKey; its refusals return the phase to signin-login
  // and show on the login step.
  const AFTER_KEY: [string, Parameters<typeof refusal>[0], string][] = [
    ['the device cap', { code: 'E_FORBIDDEN', status: 403 }, 'This account already has as many devices as dilla.test allows. Remove one in Settings on another device first.'],
    ['the registration meter', { code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 600 }, 'Too many sign-in attempts. Try again in 1 s.'],
    ['a replaced row', { code: 'E_SIGNIN_EVICTED' }, 'Someone else is signing in to this account. Change your password from a device you still have, or ask the operator.'],
  ];
  it.each(AFTER_KEY)('a registration refused after the key shows %s on the login step', async (_, init, text) => {
    const { user, view } = setup('signin-login', (c, f) => {
      if (c.m !== 'signInKey') return worker()(c, f);
      move(f, { phase: 'enrolling' });
      move(f, { phase: 'signin-login', signIn: { username: 'ada', factor: null } });
      return Promise.reject(refusal(init));
    });
    await login(user);
    await pasteKey(user);
    await user.click(button('Add browser'));
    expect(h1()).toHaveTextContent('Sign in to dilla.test');
    expect(screen.getByText('Sign in · step 1 of 4')).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent(text);
    expect(field('Username')).toHaveValue(' Ada ');
    await expectNoAxeViolations(view.container);
  });

  // R6 (pre-flight C-15): the assertion lives five minutes; a key read off paper for longer costs a new login.
  it('an assertion that ran out during the key says enrol.expired on the login step, the password emptied and focused', async () => {
    const { fake, user, view } = setup('signin-login', (c, f) => {
      if (c.m !== 'signInKey') return worker({ totp: true })(c, f);
      move(f, { phase: 'enrolling' });
      move(f, { phase: 'signin-login', signIn: { username: 'ada', factor: null }, error: EXPIRED_ERROR });
      return Promise.reject(refusal({ code: 'E_SIGNIN_EXPIRED' }));
    });
    await login(user);
    await user.type(field('Code'), '123456');
    await settleCode();
    await pasteKey(user);
    await user.click(button('Add browser'));
    expect(fake.callsOf('signInKey')).toEqual([{ m: 'signInKey', recoveryKey: GROUPED }]);
    expect(h1()).toHaveTextContent('Sign in to dilla.test');
    expect(screen.getByText('Sign in · step 1 of 4')).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent(EXPIRED);
    expect(screen.getByRole('alert').closest('.d-banner')).toHaveAttribute('data-tone', 'warn');
    expect(screen.queryByText(/E_SIGNIN_EXPIRED/)).toBeNull();
    expect(field('Username')).toHaveValue(' Ada ');
    expect(password()).toHaveValue('');
    expect(document.activeElement).toBe(password());
    await expectNoAxeViolations(view.container);
  });

  // Pre-flight C-16: on the backup-code path the expiry is a 401 from enrol.backupCode, which the worker reports as
  // E_SIGNIN_EXPIRED too; the page shows the banner, not a raw error, and no code was spent.
  it('the same expiry on the backup-code path shows the banner, not a raw error', async () => {
    const { fake, user } = setup('signin-login', (c, f) => {
      if (c.m !== 'signInBackupCode') return worker({ totp: true })(c, f);
      move(f, { phase: 'signin-login', signIn: { username: 'ada', factor: null }, error: EXPIRED_ERROR });
      return Promise.reject(refusal({ code: 'E_SIGNIN_EXPIRED' }));
    });
    await login(user);
    await user.click(button(en['factorSignin.useBackup']));
    await user.type(field('Backup code'), `${BACKUP}{Enter}`);
    await pasteKey(user);
    await user.click(button('Add browser'));
    expect(fake.callsOf('signInBackupCode')).toEqual([{ m: 'signInBackupCode', code: BACKUP, recoveryKey: GROUPED }]);
    expect(h1()).toHaveTextContent('Sign in to dilla.test');
    expect(screen.getByRole('alert')).toHaveTextContent(EXPIRED);
    expect(screen.queryByText(/E_SIGNIN_EXPIRED/)).toBeNull();
    expect(screen.queryByText(en['factorSignin.backup.failed'])).toBeNull();
    expect(password()).toHaveValue('');
    expect(document.activeElement).toBe(password());
    // Signing in again starts at the code: the held code went with the refusal.
    await user.type(password(), 'correct horse');
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent('Enter your code');
    expect(fake.callsOf('signInLogin').at(-1)).toEqual({ m: 'signInLogin', username: 'ada', password: 'correct horse' });
  });

  // Fix round 1 (review Important 1): the key typed for one person never reaches the next person's key step on this tab.
  it('a second person on the same tab meets an empty key field after the first one\'s sign-in ran out', async () => {
    let first = true;
    const { fake, user } = setup('signin-login', (c, f) => {
      if (c.m === 'signInKey' && first) {
        first = false;
        move(f, { phase: 'enrolling' });
        move(f, { phase: 'signin-login', signIn: { username: 'ada', factor: null }, error: EXPIRED_ERROR });
        return Promise.reject(refusal({ code: 'E_SIGNIN_EXPIRED' }));
      }
      return worker()(c, f);
    });
    await login(user);
    await pasteKey(user);
    await user.click(button('Add browser'));
    expect(screen.getByRole('alert')).toHaveTextContent(EXPIRED);
    await user.clear(field('Username'));
    await user.type(field('Username'), 'bob');
    await user.type(password(), 'battery staple');
    await user.click(button('Continue'));
    expect(fake.callsOf('signInLogin').at(-1)).toEqual({ m: 'signInLogin', username: 'bob', password: 'battery staple' });
    expect(h1()).toHaveTextContent('Add this browser');
    expect(field('Recovery key')).toHaveValue('');
    expect(field('Recovery key')).toHaveAccessibleDescription(expect.stringContaining('0 of 52 characters'));
  });

  it('back at the login, the key field starts empty even for the same username', async () => {
    let first = true;
    const { user } = setup('signin-login', (c, f) => {
      if (c.m === 'signInKey' && first) {
        first = false;
        move(f, { phase: 'enrolling' });
        move(f, { phase: 'signin-login', signIn: { username: 'ada', factor: null }, error: EXPIRED_ERROR });
        return Promise.reject(refusal({ code: 'E_SIGNIN_EXPIRED' }));
      }
      return worker()(c, f);
    });
    await login(user);
    await pasteKey(user);
    await user.click(button('Add browser'));
    expect(field('Username')).toHaveValue(' Ada ');
    await user.type(password(), 'correct horse');
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent('Add this browser');
    expect(field('Recovery key')).toHaveValue('');
  });

  it('an account with nothing to recover, found by the key step, offers only Cancel', async () => {
    const { user, fake, view } = setup('signin-key', (c, f) => {
      if (c.m !== 'signInKey') return Promise.resolve(null);
      move(f, { phase: 'enrolling' });
      move(f, { phase: 'signin-key', error: { code: 'E_NO_BACKUP', detail: '', status: 0, retryAfterMs: null } });
      return Promise.reject(refusal({ code: 'E_NO_BACKUP' }));
    });
    await pasteKey(user);
    await user.click(button('Add browser'));
    expect(h1()).toHaveTextContent('Add this browser');
    expect(screen.getByRole('alert')).toHaveTextContent('This account has no backup to recover from on dilla.test.');
    expect(screen.queryByRole('textbox')).toBeNull();
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual(['Cancel']);
    await expectNoAxeViolations(view.container);
    await user.click(button('Cancel'));
    expect(fake.callsOf('signInCancel')).toEqual([{ m: 'signInCancel' }]);
  });

  // Flow 03 (task 15, D5): E_NO_BACKUP from signInKey is a missing or unreadable backup state, its own string.
  it('a key refused for a missing backup state offers only Cancel', async () => {
    const { user, fake } = setup('signin-key', (c, f) => {
      if (c.m !== 'signInKey') return Promise.resolve(null);
      move(f, { phase: 'enrolling' });
      move(f, { phase: 'signin-key', error: { code: 'E_NO_BACKUP', detail: '', status: 0, retryAfterMs: null } });
      return Promise.reject(refusal({ code: 'E_NO_BACKUP' }));
    });
    await pasteKey(user);
    await user.click(button('Add browser'));
    expect(h1()).toHaveTextContent('Add this browser');
    // BACKUPS-RECOVERY-03: the copy names the action that repairs the backup.
    expect(screen.getByRole('alert')).toHaveTextContent('This account has no backup to recover from on dilla.test. Sign in on a device that still holds this account and open dilla there; it repairs the backup. Then try again.');
    expect(screen.queryByRole('textbox')).toBeNull();
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual(['Cancel']);
    await user.click(button('Cancel'));
    expect(fake.callsOf('signInCancel')).toEqual([{ m: 'signInCancel' }]);
  });

  it('a reload into an account with nothing to recover shows the same', () => {
    setup('signin-key', idle, { error: { code: 'E_NO_BACKUP', detail: '', status: 0, retryAfterMs: null } });
    expect(screen.getByRole('alert')).toHaveTextContent('This account has no recovery data on dilla.test.');
    expect(screen.queryByRole('textbox')).toBeNull();
  });

  it('reads no other code from the account slice', () => {
    setup('signin-key', idle, { error: { code: 'E_RECOVERY_KEY', detail: '', status: 0, retryAfterMs: null } });
    expect(screen.queryByRole('alert')).toBeNull();
    expect(field('Recovery key')).toBeInTheDocument();
  });

  it('a wrong key keeps the text, puts focus on the field, and the right key works after it', async () => {
    let attempt = 0;
    const { fake, user } = setup('signin-key', (c, f) => {
      if (c.m !== 'signInKey') return Promise.resolve(null);
      attempt += 1;
      if (attempt === 1) {
        move(f, { phase: 'enrolling' });
        move(f, { phase: 'signin-key' });
        return Promise.reject(refusal({ code: 'E_RECOVERY_KEY' }));
      }
      move(f, { phase: 'ready', user: ME, signIn: null });
      return Promise.resolve(null);
    });
    await pasteKey(user, GROUPED.replace('7K2M', '7K2N'));
    await user.click(button('Add browser'));
    expect(field('Recovery key')).toHaveValue(GROUPED.replace('7K2M', '7K2N'));
    expect(field('Recovery key')).toHaveAccessibleDescription(expect.stringContaining('This is not the recovery key of this account. Check every character.'));
    expect(document.activeElement).toBe(field('Recovery key'));
    await user.clear(field('Recovery key'));
    await pasteKey(user);
    await user.click(button('Add browser'));
    expect(fake.callsOf('signInKey')).toEqual([
      { m: 'signInKey', recoveryKey: GROUPED.replace('7K2M', '7K2N') }, { m: 'signInKey', recoveryKey: GROUPED },
    ]);
    expect(h1()).toHaveTextContent('You’re in');
  });

  it('a refused detail is never read', async () => {
    const { user } = setup('signin-key', c => (c.m === 'signInKey'
      ? Promise.reject(refusal({ code: 'E_TIER_MISMATCH', detail: 'E_RECOVERY_KEY wrong recovery key', status: 400 })) : Promise.resolve(null)));
    await pasteKey(user);
    await user.click(button('Add browser'));
    expect(screen.getByRole('alert')).toHaveTextContent('Signing in did not work (E_TIER_MISMATCH). Try again.');
    expect(screen.queryByText(/not the recovery key/)).toBeNull();
  });
});


describe('the key step recognises a backup code (R44)', () => {
  const BACKUP = 'abcd-efgh-ijkl-mnop-qrst-uvwxyz';
  const OUTSIDE = '0189-0189-0189-0189-0189-018901';
  const LONG = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ23456723456723456723456723';
  const NOTE = en['signIn.key.isBackupCode'];
  const key = () => field('Recovery key');
  const note = () => document.getElementById('signin-key-backup');
  const typeInto = (text: string) => { act(() => { fireEvent.change(key(), { target: { value: text } }); }); };
  const wait = (ms: number) => { act(() => { vi.advanceTimersByTime(ms); }); };

  beforeEach(() => { vi.useFakeTimers(); });
  afterEach(() => { vi.useRealTimers(); });

  it('names a backup code in the words of web3.8', () => {
    setup('signin-key');
    typeInto(BACKUP);
    wait(600);
    expect(note()).toHaveTextContent('That looks like a backup code, not the recovery key. This step needs the 52-character recovery key you saved when you created the account.');
  });

  it('shows the note after 600 ms of rest, describes the field with it and keeps the submit disabled', async () => {
    setup('signin-key');
    typeInto(BACKUP);
    wait(599);
    expect(note()).toBeNull();
    wait(1);
    expect(note()).toHaveTextContent(NOTE);
    expect(note()).not.toHaveAttribute('role');
    expect(key().getAttribute('aria-describedby')).toContain('signin-key-backup');
    expect(button('Add browser')).toHaveAttribute('aria-disabled', 'true');
    vi.useRealTimers(); // axe schedules with timers
    await expectNoAxeViolations(document.body);
  });

  it('shows the note at once on blur', () => {
    setup('signin-key');
    typeInto(BACKUP);
    expect(note()).toBeNull();
    act(() => { fireEvent.blur(key()); });
    expect(note()).toHaveTextContent(NOTE);
  });

  it('restarts the wait on every change', () => {
    setup('signin-key');
    typeInto(BACKUP);
    wait(400);
    typeInto(BACKUP.toUpperCase());
    wait(400);
    expect(note()).toBeNull();
    wait(200);
    expect(note()).toHaveTextContent(NOTE);
  });

  it('never shows the note while a 52-character key is typed one character at a time', () => {
    setup('signin-key');
    for (let i = 1; i <= LONG.length; i += 1) {
      typeInto(LONG.slice(0, i));
      expect(note()).toBeNull();
      wait(50);
      expect(note()).toBeNull();
    }
    wait(2000);
    expect(note()).toBeNull();
  });

  it('hides the note when the entry grows into a 52-character key', () => {
    setup('signin-key');
    typeInto(BACKUP);
    wait(600);
    expect(note()).not.toBeNull();
    typeInto(LONG);
    expect(note()).toBeNull();
    expect(key().getAttribute('aria-describedby') ?? '').not.toContain('signin-key-backup');
  });

  it('shows nothing for 26 characters outside the backup alphabet', () => {
    setup('signin-key');
    typeInto(OUTSIDE);
    wait(2000);
    act(() => { fireEvent.blur(key()); });
    expect(note()).toBeNull();
  });

  it('announces the note once, politely, when it turns on and not on every keystroke', () => {
    setup('signin-key');
    const region = () => screen.getByTestId('signin-announcer');
    typeInto(BACKUP);
    wait(300);
    typeInto(BACKUP.toUpperCase());
    wait(300);
    expect(region()).toBeEmptyDOMElement();
    wait(600);
    wait(1); // the announcer speaks a tick after it is asked
    expect(region()).toHaveTextContent(NOTE);
    expect(region()).toHaveAttribute('aria-live', 'polite');
    wait(2000);
    expect(region().textContent).toBe(NOTE);
  });

  it('clears its timer when the step unmounts', () => {
    const { view } = setup('signin-key');
    const before = vi.getTimerCount();
    typeInto(BACKUP);
    expect(vi.getTimerCount()).toBe(before + 1);
    view.unmount();
    expect(vi.getTimerCount()).toBe(before);
  });

  it('ignores a change while the step is blocked', () => {
    setup('enrolling');
    typeInto(BACKUP);
    wait(2000);
    expect(key()).toHaveValue('');
    expect(note()).toBeNull();
  });

  it('has no axe violations without the note', async () => {
    setup('signin-key');
    typeInto(LONG);
    vi.useRealTimers();
    await expectNoAxeViolations(document.body);
  });
});
