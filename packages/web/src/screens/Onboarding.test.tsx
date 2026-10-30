// Flow 01, wave B task 3b: the prototype's six steps. 1 invite, 2 account (Create account makes the keys), 3 recovery key
// (I saved it creates the account), 4 and 5 the second factor (OnboardingFactor.test.tsx), 6 done.
import { describe, it, expect, vi } from 'vitest';
import { act, render, screen, within } from '@testing-library/react';
import userEvent, { type UserEvent } from '@testing-library/user-event';
import type { AccountState } from '@dilla/client-core';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient, refusal } from '../test/fake-client.ts';
import { account, INSTANCE } from '../test/fixtures.ts';
import { expectNoAxeViolations } from '../test/setup.ts';
import { browser } from '../browser.ts';
import { Onboarding } from './Onboarding.tsx';

const KEY = ['7K2M', 'QX9D', 'H4TB', 'R8NW', 'C3VF', 'J6PZ', 'A1GE', 'Y5KS', 'M0QT', 'B7XH', 'W2DN', 'F9RC', 'P4ZA'];
const KEY2 = ['N3VA', 'G8TQ', 'K1PZ', 'W7MD', 'R2XE', 'B9HC', 'F4JS', 'Y6LU', 'Q0NG', 'T5KW', 'D8PV', 'H3ZM', 'C7RA'];
const ACK = 'I saved my recovery key somewhere safe';
const RULE = 'Use 3 to 32 characters: a–z, 0–9, dot, underscore or hyphen, with no dot at the start or end.';
const PASSWORD_SIGNUP = { instance: { ...INSTANCE, passwordSignup: true } };
// INSTANCE.name of task 22's fixture is 'dilla.test'; every expected text below writes it out.

/** A worker double: each signupBegin makes the next key (KEY, then KEY2, …); signupReset forgets the pending one. */
function setup(over: Partial<AccountState> = {}, path = '/') {
  window.history.replaceState(null, '', path);
  const fake = new FakeClient();
  fake.set('account', account({ phase: 'needs-signup', user: null, deviceId: null, ...over }));
  let begun = 0;
  fake.handler = c => {
    if (c.m === 'signupBegin') {
      begun += 1;
      fake.set('account', { ...fake.get('account')!, phase: 'signup-keys', recoveryKey: begun % 2 === 1 ? KEY : KEY2 });
    }
    if (c.m === 'signupReset') fake.set('account', { ...fake.get('account')!, phase: 'needs-signup', recoveryKey: null });
    if (c.m === 'signupSubmit') return Promise.resolve({ communityId: null, joinError: null });
    return Promise.resolve(null);
  };
  const onFinish = vi.fn();
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><Onboarding onFinish={onFinish} /></CoreProvider></div>);
  return { fake, onFinish, user, view };
}
const h1 = () => screen.getByRole('heading', { level: 1 });
const button = (name: string) => screen.getByRole('button', { name });
const field = (name: string) => screen.getByRole('textbox', { name });

async function toAccount(user: UserEvent, invite = 'ABCD-EFGH') {
  await user.type(field('Invite link'), invite);
  await user.click(button('Continue'));
}
async function toKeys(user: UserEvent, password?: string) {
  await toAccount(user);
  await user.type(field('Username'), ' Ada ');
  if (password !== undefined) await user.type(screen.getByLabelText('Password'), password);
  await user.click(button('Create account'));
}
async function ticked(user: UserEvent) {
  await toKeys(user);
  await user.click(screen.getByRole('checkbox', { name: ACK }));
}
/** A refusal of the worker's signupSubmit; every other command as the setup's double. */
function refuseSubmit(fake: FakeClient, r: Parameters<typeof refusal>[0]) {
  const base = fake.handler;
  fake.handler = c => (c.m === 'signupSubmit' ? Promise.reject(refusal(r)) : base(c));
}

describe('step 1: invite', () => {
  it('starts at the invite, prefilled from the link, with focus on the title and the prototype copy', async () => {
    const { view } = setup({}, '/welcome?invite=ABCD-EFGH');
    expect(h1()).toHaveTextContent('Join with an invite');
    expect(document.activeElement).toBe(h1());
    expect(screen.getByText('New account · step 1 of 6')).toBeInTheDocument();
    expect(screen.getByText('Paste the link someone sent you.')).toBeInTheDocument();
    expect(field('Invite link')).toHaveValue('ABCD-EFGH');
    expect(field('Invite link')).toHaveAttribute('placeholder', 'https://');
    expect(screen.queryByRole('button', { name: 'Back' })).toBeNull();
    await expectNoAxeViolations(view.container);
  });
  it('requires an invite on an invite-only instance', async () => {
    const { user, fake } = setup();
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent('Join with an invite');
    expect(field('Invite link')).toHaveAccessibleDescription(expect.stringContaining('Paste the invite you were given.'));
    expect(screen.getByRole('alert')).toHaveTextContent('Paste the invite you were given.');
    expect(fake.calls.filter(c => c.m !== 'start')).toEqual([]);
  });
  it('makes the invite optional on an open instance', async () => {
    const { user } = setup({ instance: { ...INSTANCE, registrationMode: 1 } });
    expect(screen.getByText('dilla.test is open to new accounts. Paste an invite if you were given one.')).toBeInTheDocument();
    expect(field('Invite link (optional)')).toHaveValue('');
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent('Create your account');
    expect(document.activeElement).toBe(h1());
  });
  it('says when the instance is closed and offers nothing to submit', async () => {
    const { view } = setup({ instance: { ...INSTANCE, registrationMode: 2 } });
    expect(h1()).toHaveTextContent('dilla.test is not taking new accounts');
    expect(screen.getByText('Ask the host of dilla.test when sign-ups open again.')).toBeInTheDocument();
    expect(screen.queryByRole('textbox')).toBeNull();
    expect(screen.queryByRole('button')).toBeNull();
    await expectNoAxeViolations(view.container);
  });
  // Pre-flight ruling (b): the closed frame is no step of the ceremony, so it has no step line.
  it('gives the closed frame no step line', () => {
    const { view } = setup({ instance: { ...INSTANCE, registrationMode: 2 } });
    expect(document.activeElement).toBe(h1());
    expect(view.container.querySelector('.d-onboarding-frame__step')).toBeNull();
  });
});

describe('step 2: account', () => {
  it('asks for a username and a password only, in the prototype copy, with no display name', async () => {
    const { user, view } = setup(PASSWORD_SIGNUP);
    await toAccount(user);
    expect(h1()).toHaveTextContent('Create your account');
    expect(screen.getByText('New account · step 2 of 6')).toBeInTheDocument();
    expect(screen.getByText('Pick a username and a password. Your username is how people mention you.')).toBeInTheDocument();
    expect(screen.getAllByRole('textbox').map(b => b.getAttribute('id'))).toEqual(['onboarding-username']);
    expect(screen.queryByLabelText(/Display name/)).toBeNull();
    expect(screen.getByLabelText('Password')).toHaveAttribute('placeholder', 'at least 12 characters');
    expect(screen.getByLabelText('Password')).toHaveAttribute('autocomplete', 'new-password');
    expect(button('Create account')).toHaveAttribute('type', 'submit');
    await expectNoAxeViolations(view.container);
  });
  it('checks every field before making keys', async () => {
    const { user, fake } = setup();
    await toAccount(user);
    await user.type(field('Username'), '.ada');
    await user.click(button('Create account'));
    expect(field('Username')).toHaveAccessibleDescription(expect.stringContaining(RULE));
    expect(fake.callsOf('signupBegin')).toHaveLength(0);
    await user.clear(field('Username'));
    await user.type(field('Username'), 'ab');
    await user.click(button('Create account'));
    expect(field('Username')).toHaveAccessibleDescription(expect.stringContaining(RULE));
    await user.clear(field('Username'));
    await user.type(field('Username'), 'Ada.L');
    await user.click(button('Create account'));
    expect(fake.callsOf('signupBegin')).toEqual([{ m: 'signupBegin' }]);
    expect(fake.callsOf('signupSubmit')).toEqual([]);
    expect(h1()).toHaveTextContent('Save your recovery key');
  });
  // Jonas, 2026-10-30: 12 characters, counted in code points (an emoji is one).
  it('refuses a password of 11 characters and takes 12, before making keys', async () => {
    const { user, fake } = setup(PASSWORD_SIGNUP);
    await toAccount(user);
    await user.type(field('Username'), 'ada');
    await user.type(screen.getByLabelText('Password'), '🔑'.repeat(11));
    await user.click(button('Create account'));
    expect(screen.getByLabelText('Password')).toHaveAccessibleDescription(expect.stringContaining('Use at least 12 characters.'));
    expect(document.activeElement).toBe(screen.getByLabelText('Password'));
    expect(fake.callsOf('signupBegin')).toHaveLength(0);
    await user.type(screen.getByLabelText('Password'), '🔑');
    await user.click(button('Create account'));
    expect(fake.callsOf('signupBegin')).toHaveLength(1);
    expect(h1()).toHaveTextContent('Save your recovery key');
  });
  it('Enter in the username field creates the keys', async () => {
    const { user, fake } = setup();
    await toAccount(user);
    await user.type(field('Username'), 'ada{Enter}');
    expect(fake.callsOf('signupBegin')).toEqual([{ m: 'signupBegin' }]);
    expect(h1()).toHaveTextContent('Save your recovery key');
    expect(document.activeElement).toBe(h1());
  });
  it('focus goes to the first field in error', async () => {
    const { user, fake } = setup(PASSWORD_SIGNUP);
    await toAccount(user);
    await user.type(field('Username'), 'A');
    await user.click(button('Create account'));
    expect(field('Username')).toHaveAccessibleDescription(expect.stringContaining(RULE));
    expect(screen.getByLabelText('Password')).toHaveAccessibleDescription(expect.stringContaining('Use at least 12 characters.'));
    expect(document.activeElement).toBe(field('Username'));
    expect(fake.callsOf('signupBegin')).toHaveLength(0);
  });
  it('Back never submits the step', async () => {
    const { user, fake } = setup();
    await toAccount(user);
    expect(button('Back')).toHaveAttribute('type', 'button');
    await user.click(button('Back'));
    expect(h1()).toHaveTextContent('Join with an invite');
    expect(fake.callsOf('signupBegin')).toHaveLength(0);
  });
  it('asks for a password only when the instance offers one', async () => {
    const { user } = setup();
    await toAccount(user);
    expect(screen.queryByLabelText('Password')).toBeNull();
  });
  it('shows a failed key generation and stays', async () => {
    const { user, fake } = setup();
    fake.handler = () => Promise.reject(refusal({ code: 'E_CORE_STORAGE', detail: 'disk' }));
    await toAccount(user);
    await user.type(field('Username'), 'ada');
    await user.click(button('Create account'));
    expect(await screen.findByRole('alert')).toHaveTextContent('The account could not be created (E_CORE_STORAGE). Try again.');
    expect(h1()).toHaveTextContent('Create your account');
  });
  it('resets keys an earlier attempt left pending before it makes new ones', async () => {
    const { user, fake } = setup({ phase: 'signup-keys', recoveryKey: KEY });
    await toAccount(user);
    await user.type(field('Username'), 'ada');
    await user.click(button('Create account'));
    expect(fake.calls.filter(c => c.m === 'signupReset' || c.m === 'signupBegin').map(c => c.m)).toEqual(['signupReset', 'signupBegin']);
  });
});

describe('step 3: recovery key', () => {
  it('shows the key once ready, in the prototype copy, and gates I saved it on the tick', async () => {
    const print = vi.spyOn(browser, 'print').mockImplementation(() => {});
    const { user, view, fake } = setup();
    await toKeys(user);
    expect(screen.getByText('New account · step 3 of 6')).toBeInTheDocument();
    expect(document.activeElement).toBe(h1());
    expect(h1()).toHaveTextContent('Save your recovery key');
    expect(screen.getByText('Shown once. You need it to add another browser.')).toBeInTheDocument();
    for (const group of KEY) expect(screen.getByText(group)).toBeInTheDocument();
    // protocol/03: no copy control on the key's screen.
    expect(screen.queryByRole('button', { name: /copy/i })).toBeNull();
    // Blocked for a reason the person must hear, so focusable (Global Constraints line 98, A11Y-DESIGN-03).
    expect(button('I saved it')).toHaveAttribute('aria-disabled', 'true');
    expect(button('I saved it')).toHaveAccessibleDescription('Tick the box to continue.');
    await user.click(button('I saved it'));
    expect(fake.callsOf('signupSubmit')).toEqual([]);
    expect(h1()).toHaveTextContent('Save your recovery key');
    await user.click(screen.getByRole('checkbox', { name: ACK }));
    expect(button('I saved it')).not.toHaveAttribute('aria-disabled');
    expect(button('I saved it')).not.toHaveAttribute('aria-describedby');
    await user.click(screen.getByRole('checkbox', { name: ACK }));
    expect(button('I saved it')).toHaveAttribute('aria-disabled', 'true');
    await user.click(button('Print'));
    expect(print).toHaveBeenCalledTimes(1);
    await expectNoAxeViolations(view.container);
    print.mockRestore();
  });
  // Jonas (task 3b): Save as file, beside Print. The file carries the key exactly once, with a short explanation; nothing
  // else of it is stored; Copy stays forbidden (protocol/03).
  it('saves the key as a text file that carries it exactly once, and offers no copy', async () => {
    const download = vi.spyOn(browser, 'download').mockImplementation(() => {});
    const setItem = vi.spyOn(Storage.prototype, 'setItem');
    const { user, fake } = setup();
    await toKeys(user);
    const save = button('Save as file');
    expect(save.closest('.d-recovery-key__actions')).toBe(button('Print').closest('.d-recovery-key__actions'));
    await user.click(save);
    expect(download).toHaveBeenCalledTimes(1);
    const [name, text] = download.mock.calls[0];
    expect(name).toBe('dilla-recovery-key-dilla.test.txt');
    const grouped = KEY.join('-');
    expect(text.split(grouped)).toHaveLength(2);
    expect(text).toContain('Your dilla recovery key for dilla.test.');
    expect(screen.queryByRole('button', { name: /copy/i })).toBeNull();
    // Security review LOW-3: the printed sheet names itself and the instance (print-only, so the screen keeps parity).
    expect(screen.getByText('dilla recovery key for dilla.test. You need it to add another browser to your account.'))
      .toHaveClass('d-onboarding-frame__print-only');
    expect(JSON.stringify(setItem.mock.calls)).not.toContain(KEY[0]);
    expect(window.location.href).not.toContain(KEY[0]);
    expect(fake.callsOf('signupSubmit')).toEqual([]);
    download.mockRestore();
    setItem.mockRestore();
  });
  it('waits for the key before showing it', async () => {
    const { user, fake } = setup();
    fake.handler = () => Promise.resolve(null);
    await toKeys(user);
    expect(h1()).toHaveTextContent('Save your recovery key');
    expect(screen.getByRole('status')).toHaveTextContent('Making your keys');
    expect(screen.queryByRole('checkbox')).toBeNull();
    expect(button('I saved it')).toHaveAttribute('aria-disabled', 'true');
    act(() => fake.set('account', account({ phase: 'signup-keys', user: null, recoveryKey: KEY })));
    expect(screen.getByRole('checkbox', { name: ACK })).not.toBeChecked();
  });
  // Jonas (task 3b): a shown key is never offered again. Back discards it, and the next Create account makes a new one.
  it('Back discards the shown key; Create account then makes and shows a new one, unticked', async () => {
    const { user, fake } = setup();
    await ticked(user);
    await user.click(button('Back'));
    expect(h1()).toHaveTextContent('Create your account');
    expect(fake.callsOf('signupReset')).toEqual([{ m: 'signupReset' }]);
    expect(field('Username')).toHaveValue(' Ada ');
    await user.click(button('Create account'));
    expect(h1()).toHaveTextContent('Save your recovery key');
    for (const group of KEY2) expect(screen.getByText(group)).toBeInTheDocument();
    expect(screen.queryByText(KEY[0])).toBeNull();
    expect(screen.getByRole('checkbox', { name: ACK })).not.toBeChecked();
    expect(fake.callsOf('signupBegin')).toHaveLength(2);
    expect(fake.callsOf('signupSubmit')).toEqual([]);
  });
});

describe('creating the account (step 3, I saved it)', () => {
  it('submits exactly what was entered, with no display name, only after the tick', async () => {
    const { user, fake, view } = setup();
    await ticked(user);
    fake.handler = () => new Promise(() => {});
    await user.click(button('I saved it'));
    expect(fake.callsOf('signupSubmit')).toEqual([{
      m: 'signupSubmit', invite: 'ABCD-EFGH', username: 'ada', display: '', password: null, recoveryKeyAcknowledged: true,
    }]);
    // Pre-flight ruling (c): while busy the buttons are blocked with aria-disabled, so focus stays on the pressed one.
    expect(button('Creating account…')).toHaveAttribute('aria-disabled', 'true');
    expect(button('Back')).toHaveAttribute('aria-disabled', 'true');
    expect(document.activeElement).toBe(button('Creating account…'));
    expect(screen.getByRole('status')).toHaveTextContent('Creating your account on dilla.test…');
    // The tick cannot be taken back while the account is created.
    await user.click(screen.getByRole('checkbox', { name: ACK }));
    expect(screen.getByRole('checkbox', { name: ACK })).toBeChecked();
    await expectNoAxeViolations(view.container);
  });
  it('sends nothing more and goes nowhere while the account is being created', async () => {
    const { user, fake } = setup();
    await ticked(user);
    fake.handler = () => new Promise(() => {});
    await user.click(button('I saved it'));
    await user.click(button('Creating account…'));
    await user.keyboard('{Enter}');
    await user.click(button('Back'));
    expect(fake.callsOf('signupSubmit')).toHaveLength(1);
    expect(fake.callsOf('signupReset')).toEqual([]);
    expect(h1()).toHaveTextContent('Save your recovery key');
  });
  it('sends the code of a pasted link and the password when asked for', async () => {
    const { user, fake } = setup(PASSWORD_SIGNUP);
    await toAccount(user, 'https://dilla.test/i/WXYZ-1234');
    await user.type(field('Username'), 'ada');
    await user.type(screen.getByLabelText('Password'), 'correct horse');
    await user.click(button('Create account'));
    await user.click(screen.getByRole('checkbox', { name: ACK }));
    await user.click(button('I saved it'));
    expect(fake.callsOf('signupSubmit')[0]).toEqual({
      m: 'signupSubmit', invite: 'WXYZ-1234', username: 'ada', display: '', password: 'correct horse', recoveryKeyAcknowledged: true,
    });
  });
  it('finishes only once the account is ready, at step 6 in the prototype copy', async () => {
    const { user, fake, onFinish } = setup();
    await ticked(user);
    await user.click(button('I saved it'));
    act(() => fake.set('account', account({ phase: 'registering', user: null, recoveryKey: KEY })));
    expect(h1()).toHaveTextContent('Save your recovery key');
    act(() => fake.set('account', account({ phase: 'ready' })));
    expect(h1()).toHaveTextContent('You’re in');
    expect(document.activeElement).toBe(h1());
    expect(screen.getByText('New account · step 6 of 6')).toBeInTheDocument();
    expect(screen.getByText('Your account is ready.')).toBeInTheDocument();
    // Jonas (task 3b): the removed step's note, as small print.
    expect(screen.getByText('Messages sent before another browser joins are not shown there.')).toHaveClass('d-onboarding-frame__note');
    await user.click(button('Open dilla'));
    expect(onFinish).toHaveBeenCalledWith({ communityId: null, joinError: null, invite: 'ABCD-EFGH' });
  });
  it('hands the joined server and a refused join to onFinish', async () => {
    const C = 'c4'.repeat(16);
    const joined = setup();
    await ticked(joined.user);
    joined.fake.handler = c => Promise.resolve(c.m === 'signupSubmit' ? { communityId: C, joinError: null } : null);
    await joined.user.click(button('I saved it'));
    act(() => joined.fake.set('account', account({ phase: 'ready' })));
    await joined.user.click(button('Open dilla'));
    expect(joined.onFinish).toHaveBeenCalledWith({ communityId: C, joinError: null, invite: 'ABCD-EFGH' });
    joined.view.unmount();
    const refused = setup();
    await ticked(refused.user);
    refused.fake.handler = c => Promise.resolve(c.m === 'signupSubmit'
      ? { communityId: null, joinError: { code: 'E_INVITE_INVALID', detail: 'spent', status: 410, retryAfterMs: null } } : null);
    await refused.user.click(button('I saved it'));
    act(() => refused.fake.set('account', account({ phase: 'ready' })));
    await refused.user.click(button('Open dilla'));
    expect(refused.onFinish).toHaveBeenCalledWith({
      communityId: null, joinError: { code: 'E_INVITE_INVALID', detail: 'spent', status: 410, retryAfterMs: null }, invite: 'ABCD-EFGH',
    });
  });
  it('waits for the join that follows the ready phase', async () => {
    const C = 'c4'.repeat(16);
    const { user, fake, onFinish } = setup();
    await ticked(user);
    let resolve: (value: unknown) => void = () => {};
    fake.handler = () => new Promise(r => { resolve = r; });
    await user.click(button('I saved it'));
    act(() => fake.set('account', account({ phase: 'ready' })));
    expect(h1()).toHaveTextContent('Save your recovery key');
    await act(async () => { resolve({ communityId: C, joinError: null }); await Promise.resolve(); });
    expect(h1()).toHaveTextContent('You’re in');
    await user.click(button('Open dilla'));
    expect(onFinish).toHaveBeenCalledWith({ communityId: C, joinError: null, invite: 'ABCD-EFGH' });
  });
  it('reads a malformed result as no server', async () => {
    const { user, fake, onFinish } = setup();
    await ticked(user);
    fake.handler = () => Promise.resolve({ communityId: 'C4'.repeat(16), joinError: null });
    await user.click(button('I saved it'));
    act(() => fake.set('account', account({ phase: 'ready' })));
    await user.click(button('Open dilla'));
    expect(onFinish).toHaveBeenCalledWith({ communityId: null, joinError: null, invite: 'ABCD-EFGH' });
  });
  // Pre-flight ruling (f): a reload during registration resumes in the worker and finishes the
  // account; the screen, mounted again with nothing submitted, ends at done once the phase is ready.
  it('ends at done when a reload during registration finishes the account', async () => {
    const { user, fake, onFinish } = setup({ phase: 'registering', recoveryKey: KEY }, '/welcome?invite=ABCD-EFGH');
    act(() => fake.set('account', account({ phase: 'ready' })));
    expect(h1()).toHaveTextContent('You’re in');
    await user.click(button('Open dilla'));
    expect(onFinish).toHaveBeenCalledWith({ communityId: null, joinError: null, invite: 'ABCD-EFGH' });
    expect(fake.callsOf('signupSubmit')).toHaveLength(0);
  });
});

// Jonas (task 3b): a refused creation sends the person back to step 2 (or 1, or the closed frame) with the reason, the key
// of that attempt discarded; the next Create account makes a new one. A lost connection is the exception: the account
// may exist, so the page stays and Reload is the only way on (Requirement 6).
describe('refusals of I saved it', () => {
  it('an invalid invite returns to step 1 with focus on the invite field, and the key is discarded', async () => {
    const { user, fake } = setup();
    await ticked(user);
    refuseSubmit(fake, { code: 'E_INVITE_INVALID', detail: 'the invite is spent, expired, revoked or unknown', status: 410 });
    await user.click(button('I saved it'));
    expect(h1()).toHaveTextContent('Join with an invite');
    expect(field('Invite link')).toHaveAccessibleDescription(expect.stringContaining('This invite is expired, used up or unknown. Ask for a new one.'));
    expect(document.activeElement).toBe(field('Invite link'));
    expect(fake.callsOf('signupReset')).toEqual([{ m: 'signupReset' }]);
  });
  it('a taken username returns to step 2 with the error on the field; the next Create account shows a new key, unticked', async () => {
    const { user, fake } = setup();
    await ticked(user);
    refuseSubmit(fake, { code: 'E_INVALID_REQUEST', detail: 'reworded by the server', status: 409, retryAfterMs: null });
    await user.click(button('I saved it'));
    expect(h1()).toHaveTextContent('Create your account');
    expect(field('Username')).toHaveAccessibleDescription(expect.stringContaining('That username is taken. Try another.'));
    expect(document.activeElement).toBe(field('Username'));
    expect(fake.callsOf('signupReset')).toHaveLength(1);
    await user.clear(field('Username'));
    await user.type(field('Username'), 'ada2');
    expect(field('Username')).not.toHaveAccessibleDescription(expect.stringContaining('taken'));
    await user.click(button('Create account'));
    for (const group of KEY2) expect(screen.getByText(group)).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: ACK })).not.toBeChecked();
    expect(button('I saved it')).toHaveAttribute('aria-disabled', 'true');
    expect(fake.callsOf('signupBegin')).toHaveLength(2);
  });
  it('a refused detail is not parsed: step 2, focus on the heading, the banner', async () => {
    const { user, fake } = setup();
    await ticked(user);
    refuseSubmit(fake, { code: 'E_INVALID_REQUEST', detail: 'username taken', status: 400, retryAfterMs: null });
    await user.click(button('I saved it'));
    expect(h1()).toHaveTextContent('Create your account');
    expect(document.activeElement).toBe(h1());
    expect(screen.getByRole('alert')).toHaveTextContent('dilla.test did not accept these details. Check the username and the password.');
    expect(screen.queryByText(/That username is taken/)).toBeNull();
    expect(fake.callsOf('signupReset')).toHaveLength(1);
  });
  it('shows the closed frame when registration closed meanwhile', async () => {
    const { user, fake } = setup();
    await ticked(user);
    refuseSubmit(fake, { code: 'E_FORBIDDEN', detail: 'this instance is not accepting registrations', status: 403 });
    await user.click(button('I saved it'));
    expect(h1()).toHaveTextContent('dilla.test is not taking new accounts');
    expect(screen.queryByRole('button')).toBeNull();
    expect(fake.callsOf('signupReset')).toHaveLength(1);
  });
  it.each([
    [4200, 'Too many attempts from this network. Try again in 5 s.'],
    [null, 'Too many attempts from this network. Wait a few minutes, then try again.'],
  ] as const)('the wait is shown on step 2 (retryAfterMs %s)', async (retryAfterMs, message) => {
    const { user, fake } = setup();
    await ticked(user);
    refuseSubmit(fake, { code: 'E_RATE_LIMITED', detail: '', status: 429, retryAfterMs });
    await user.click(button('I saved it'));
    expect(screen.getByRole('alert')).toHaveTextContent(message);
    expect(h1()).toHaveTextContent('Create your account');
    expect(button('Create account')).toBeEnabled();
    expect(fake.callsOf('signupReset')).toHaveLength(1);
  });
  it('another refusal can be tried again, with a new key', async () => {
    const { user, fake } = setup();
    await ticked(user);
    refuseSubmit(fake, { code: 'E_CORE_STATE', detail: '', status: 0, retryAfterMs: null });
    await user.click(button('I saved it'));
    const banner = screen.getByRole('alert');
    expect(banner).toHaveTextContent('The account could not be created (E_CORE_STATE). Try again.');
    expect(within(banner).queryByRole('button')).toBeNull();
    expect(h1()).toHaveTextContent('Create your account');
    await user.click(button('Create account'));
    expect(screen.getByText(KEY2[0])).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
  });
  // Pre-flight ruling (a): the banner is the first child of the step's body, inside main, under the step line.
  it('puts a refusal banner first in the step body', async () => {
    const { user, fake } = setup();
    await ticked(user);
    refuseSubmit(fake, { code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 1000 });
    await user.click(button('I saved it'));
    const banner = screen.getByRole('alert');
    expect(screen.getByRole('main')).toContainElement(banner);
    expect(banner.parentElement).toHaveClass('d-onboarding-frame__body');
    expect(banner.parentElement?.firstElementChild).toBe(banner);
  });
  it.each([
    [{ code: 'E_NETWORK', detail: '', status: 0, retryAfterMs: null }],
    [{ code: 'E_HTTP', status: 502 }],
  ])('a lost response keeps the key and is never answered with a second registration (%j)', async init => {
    const reload = vi.spyOn(browser, 'reload').mockImplementation(() => {});
    const { user, fake } = setup();
    await ticked(user);
    refuseSubmit(fake, init);
    await user.click(button('I saved it'));
    const banner = screen.getByRole('alert');
    expect(banner).toHaveTextContent('The connection dropped while creating your account. Reload the page to finish.');
    expect(h1()).toHaveTextContent('Save your recovery key');
    expect(screen.getByText(KEY[0])).toBeInTheDocument();
    expect(fake.callsOf('signupReset')).toEqual([]);
    // Requirement 6: I saved it and Back stay blocked; Reload is the only way on.
    expect(button('I saved it')).toHaveAttribute('aria-disabled', 'true');
    expect(button('Back')).toHaveAttribute('aria-disabled', 'true');
    await user.click(button('I saved it'));
    await user.keyboard('{Enter}');
    await user.click(button('Back'));
    expect(fake.callsOf('signupSubmit')).toHaveLength(1);
    expect(fake.callsOf('signupReset')).toEqual([]);
    await user.click(within(banner).getByRole('button', { name: 'Reload' }));
    expect(reload).toHaveBeenCalledTimes(1);
    reload.mockRestore();
  });
});

describe('signing in instead (the prototype\'s "Already have an account? Sign in")', () => {
  const SIGN_IN = 'Already have an account? Sign in';
  it('sits under the button at the right on step 1 when the instance takes passwords', async () => {
    const { user, fake, view } = setup(PASSWORD_SIGNUP);
    const entry = button(SIGN_IN);
    expect(entry).toHaveAttribute('type', 'button');
    expect(entry.closest('.d-onboarding-frame__nav')).not.toBeNull();
    expect(field('Invite link').compareDocumentPosition(entry) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    await expectNoAxeViolations(view.container);
    await user.click(entry);
    expect(fake.callsOf('signInBegin')).toEqual([{ m: 'signInBegin' }]);
    expect(fake.callsOf('signupBegin')).toEqual([]);
  });
  it('offers no entry when the instance has no password login', () => {
    setup();
    expect(screen.queryByRole('button', { name: SIGN_IN })).toBeNull();
  });
  it('offers the entry on a closed instance too, as its only control', () => {
    setup({ instance: { ...INSTANCE, registrationMode: 2, passwordSignup: true } });
    expect(h1()).toHaveTextContent('dilla.test is not taking new accounts');
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual([SIGN_IN]);
  });
  it('is offered on step 2 with Back at the left', async () => {
    const { user } = setup(PASSWORD_SIGNUP);
    await toAccount(user);
    const row = button(SIGN_IN).closest('.d-onboarding-frame__nav');
    expect(row).not.toBeNull();
    expect(within(row as HTMLElement).getAllByRole('button').map(b => b.textContent)).toEqual(['Back', SIGN_IN]);
  });
  it('on step 3 discards the shown key before it begins the sign-in', async () => {
    const { user, fake } = setup(PASSWORD_SIGNUP);
    await toKeys(user, 'correct horse battery');
    await user.click(button(SIGN_IN));
    expect(fake.calls.filter(c => c.m === 'signupReset' || c.m === 'signInBegin').map(c => c.m)).toEqual(['signupReset', 'signInBegin']);
    expect(fake.callsOf('signupSubmit')).toEqual([]);
  });
  it('shows a refused entry and sends it once while in flight', async () => {
    const { user, fake } = setup(PASSWORD_SIGNUP);
    let release: (e: unknown) => void = () => {};
    fake.handler = c => (c.m === 'signInBegin' ? new Promise((_, reject) => { release = reject; }) : Promise.resolve(null));
    await user.click(button(SIGN_IN));
    expect(button(SIGN_IN)).toHaveAttribute('aria-disabled', 'true');
    await user.click(button(SIGN_IN));
    expect(fake.callsOf('signInBegin')).toHaveLength(1);
    await act(async () => { release(refusal({ code: 'E_STATE' })); await Promise.resolve(); });
    expect(screen.getByRole('alert')).toHaveTextContent('Signing in did not work (E_STATE). Try again.');
    expect(button(SIGN_IN)).not.toHaveAttribute('aria-disabled');
  });
});

describe('after a list race', () => {
  const RACE = 'The account’s devices changed while you were signing in. Sign in again.';
  it('says the devices changed on the invite step, and drops the query when it moves on', async () => {
    const { user, view } = setup(PASSWORD_SIGNUP, '/welcome?signin=race');
    expect(h1()).toHaveTextContent('Join with an invite');
    expect(screen.getByRole('alert')).toHaveTextContent(RACE);
    await expectNoAxeViolations(view.container);
    await toAccount(user);
    expect(window.location.pathname + window.location.search).toBe('/welcome');
    expect(screen.queryByText(RACE)).toBeNull();
  });
  it('keeps the invite in the address when the entry drops the race', async () => {
    const { user, fake } = setup(PASSWORD_SIGNUP, '/welcome?invite=ABCD-EFGH&signin=race');
    expect(field('Invite link')).toHaveValue('ABCD-EFGH');
    await user.click(button('Already have an account? Sign in'));
    expect(fake.callsOf('signInBegin')).toEqual([{ m: 'signInBegin' }]);
    expect(window.location.pathname + window.location.search).toBe('/welcome?invite=ABCD-EFGH');
    expect(screen.queryByText(RACE)).toBeNull();
  });
  it('says it on a closed instance too', () => {
    setup({ instance: { ...INSTANCE, registrationMode: 2, passwordSignup: true } }, '/welcome?signin=race');
    expect(screen.getByRole('alert')).toHaveTextContent(RACE);
  });
  it('says nothing without the query', () => {
    setup({}, '/welcome');
    expect(screen.queryByRole('alert')).toBeNull();
  });
});

describe('after an evicted sign-in (REGISTRATION-DEVICES-02)', () => {
  const EVICTED = 'Someone else is signing in to this account. Change your password from a device you still have, or ask the operator.';
  it('says someone else is signing in on the invite step, and drops the query when it moves on', async () => {
    const { user, view } = setup(PASSWORD_SIGNUP, '/welcome?signin=evicted');
    expect(h1()).toHaveTextContent('Join with an invite');
    expect(screen.getByRole('alert')).toHaveTextContent(EVICTED);
    await expectNoAxeViolations(view.container);
    await user.click(button('Already have an account? Sign in'));
    expect(window.location.pathname + window.location.search).toBe('/welcome');
    expect(screen.queryByText(EVICTED)).toBeNull();
  });
});
