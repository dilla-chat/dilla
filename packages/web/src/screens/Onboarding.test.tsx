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
const ACK = 'I have written down or printed my recovery key';
const RULE = 'Use 3 to 32 characters: a–z, 0–9, dot, underscore or hyphen, with no dot at the start or end.';
// INSTANCE.name of task 22's fixture is 'dilla.test'; every expected text below writes it out.

function setup(over: Partial<AccountState> = {}, path = '/') {
  window.history.replaceState(null, '', path);
  const fake = new FakeClient();
  fake.set('account', account({ phase: 'needs-signup', user: null, deviceId: null, ...over }));
  fake.handler = c => {
    if (c.m === 'signupBegin') fake.set('account', { ...fake.get('account')!, phase: 'signup-keys', recoveryKey: KEY });
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

async function toIdentity(user: UserEvent, invite = 'ABCD-EFGH') {
  await user.type(field('Invite'), invite);
  await user.click(button('Continue'));
}
async function toKeys(user: UserEvent) {
  await toIdentity(user);
  await user.type(field('Username'), ' Ada ');
  await user.type(field('Display name (optional)'), 'Ada L');
  await user.click(button('Continue'));
}
async function toBrowser(user: UserEvent) {
  await toKeys(user);
  await user.click(screen.getByRole('checkbox', { name: ACK }));
  await user.click(button('Continue'));
}

describe('connect', () => {
  it('starts at the invite, prefilled from the link, with focus on the title', async () => {
    const { view } = setup({}, '/welcome?invite=ABCD-EFGH');
    expect(h1()).toHaveTextContent('Join dilla.test');
    expect(document.activeElement).toBe(h1());
    expect(screen.getByText('New account · step 1 of 6')).toBeInTheDocument();
    expect(screen.getByText('You need an invite from someone on dilla.test. Paste it below.')).toBeInTheDocument();
    expect(field('Invite')).toHaveValue('ABCD-EFGH');
    expect(field('Invite')).toHaveAccessibleDescription(expect.stringContaining('A code or a link, as you received it.'));
    await expectNoAxeViolations(view.container);
  });
  it('requires an invite on an invite-only instance', async () => {
    const { user, fake } = setup();
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent('Join dilla.test');
    expect(field('Invite')).toHaveAccessibleDescription(expect.stringContaining('Paste the invite you were given.'));
    expect(screen.getByRole('alert')).toHaveTextContent('Paste the invite you were given.');
    expect(fake.calls.filter(c => c.m !== 'start')).toEqual([]);
  });
  it('makes the invite optional on an open instance', async () => {
    const { user } = setup({ instance: { ...INSTANCE, registrationMode: 1 } });
    expect(screen.getByText('dilla.test is open to new accounts. Paste an invite if you were given one.')).toBeInTheDocument();
    expect(field('Invite (optional)')).toHaveValue('');
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent('Choose your name');
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
    expect(screen.queryByText(/^Step \d of 6$/)).toBeNull();
    expect(view.container.querySelector('.d-onboarding-frame__step')).toBeNull();
  });
});

describe('identity', () => {
  it('checks every field before making keys', async () => {
    const { user, fake } = setup();
    await toIdentity(user);
    expect(screen.getByText('New account · step 2 of 6')).toBeInTheDocument();
    await user.type(field('Username'), '.ada');
    await user.type(field('Display name (optional)'), 'x'.repeat(65));
    await user.click(button('Continue'));
    expect(field('Username')).toHaveAccessibleDescription(expect.stringContaining(RULE));
    expect(field('Display name (optional)')).toHaveAccessibleDescription(expect.stringContaining('Use at most 64 characters.'));
    expect(fake.callsOf('signupBegin')).toHaveLength(0);
    await user.clear(field('Username'));
    await user.type(field('Username'), 'ab');
    await user.clear(field('Display name (optional)'));
    await user.click(button('Continue'));
    expect(field('Username')).toHaveAccessibleDescription(expect.stringContaining(RULE));
    await user.clear(field('Username'));
    await user.type(field('Username'), 'Ada.L');
    await user.click(button('Continue'));
    expect(fake.callsOf('signupBegin')).toEqual([{ m: 'signupBegin' }]);
    expect(h1()).toHaveTextContent('Your recovery key');
  });
  it('names a display name the server would refuse next to the field, before making keys', async () => {
    const { user, fake } = setup();
    await toIdentity(user);
    await user.type(field('Username'), 'ada');
    // an invisible character (U+200B) pasted into the name
    await user.click(field('Display name (optional)'));
    await user.paste(`Ada${String.fromCodePoint(0x200b)}L`);
    await user.click(button('Continue'));
    expect(field('Display name (optional)')).toHaveAccessibleDescription(expect.stringContaining('That has characters dilla can’t show.'));
    expect(fake.callsOf('signupBegin')).toHaveLength(0);
    await user.clear(field('Display name (optional)'));
    await user.type(field('Display name (optional)'), '   ');
    await user.click(button('Continue'));
    expect(fake.callsOf('signupBegin')).toEqual([{ m: 'signupBegin' }]);
  });
  it('names a display name of combining marks only, before making keys', async () => {
    const { user, fake } = setup();
    await toIdentity(user);
    await user.type(field('Username'), 'ada');
    await user.click(field('Display name (optional)'));
    await user.paste(String.fromCodePoint(0x301, 0x302));
    await user.click(button('Continue'));
    expect(field('Display name (optional)')).toHaveAccessibleDescription(expect.stringContaining('Write something, or leave it empty.'));
    expect(fake.callsOf('signupBegin')).toHaveLength(0);
  });
  it('Enter in the username field advances', async () => {
    const { user, fake } = setup();
    await toIdentity(user);
    await user.type(field('Username'), 'ada{Enter}');
    expect(fake.callsOf('signupBegin')).toEqual([{ m: 'signupBegin' }]);
    expect(h1()).toHaveTextContent('Your recovery key');
    expect(document.activeElement).toBe(h1());
  });
  it('focus goes to the first field in error', async () => {
    const { user, fake } = setup({ instance: { ...INSTANCE, passwordSignup: true } });
    await toIdentity(user);
    await user.type(field('Username'), 'A');
    await user.click(button('Continue'));
    expect(field('Username')).toHaveAccessibleDescription(expect.stringContaining(RULE));
    expect(screen.getByLabelText('Password')).toHaveAccessibleDescription(expect.stringContaining('Use at least 8 characters.'));
    expect(document.activeElement).toBe(field('Username'));
    expect(fake.callsOf('signupBegin')).toHaveLength(0);
  });
  it('Back never submits the step', async () => {
    const { user, fake } = setup();
    await toIdentity(user);
    expect(button('Back')).toHaveAttribute('type', 'button');
    expect(button('Continue')).toHaveAttribute('type', 'submit');
    await user.click(button('Back'));
    expect(h1()).toHaveTextContent('Join dilla.test');
    expect(fake.callsOf('signupBegin')).toHaveLength(0);
  });
  it('asks for a password only when the instance offers one', async () => {
    const first = setup();
    await toIdentity(first.user);
    expect(screen.queryByLabelText('Password')).toBeNull();
    first.view.unmount();
    const { user, fake } = setup({ instance: { ...INSTANCE, passwordSignup: true } });
    await toIdentity(user);
    await user.type(field('Username'), 'ada');
    await user.type(screen.getByLabelText('Password'), '1234567');
    await user.click(button('Continue'));
    expect(screen.getByLabelText('Password')).toHaveAccessibleDescription(expect.stringContaining('Use at least 8 characters.'));
    expect(fake.callsOf('signupBegin')).toHaveLength(0);
  });
  it('shows a failed key generation and stays', async () => {
    const { user, fake } = setup();
    fake.handler = () => Promise.reject(refusal({ code: 'E_CORE_STORAGE', detail: 'disk' }));
    await toIdentity(user);
    await user.type(field('Username'), 'ada');
    await user.click(button('Continue'));
    expect(await screen.findByRole('alert')).toHaveTextContent('The account could not be created (E_CORE_STORAGE). Try again.');
    expect(h1()).toHaveTextContent('Choose your name');
  });
});

describe('recovery key', () => {
  it('shows the key once ready and gates Continue on the acknowledgement', async () => {
    const print = vi.spyOn(browser, 'print').mockImplementation(() => {});
    const { user, view } = setup();
    await toKeys(user);
    expect(screen.getByText('New account · step 3 of 6')).toBeInTheDocument();
    expect(document.activeElement).toBe(h1());
    for (const group of KEY) expect(screen.getByText(group)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /copy/i })).toBeNull();
    expect(screen.getByText('This key is shown once and is kept nowhere. Write it down or print it, and keep it away from this computer.')).toBeInTheDocument();
    expect(screen.getByText('This key is the only way to get your account back or to add another browser. If every browser you use loses its data and you do not have the key, the account and its history are gone, and the host cannot bring them back.')).toBeInTheDocument();
    // Blocked for a reason the person must hear, so focusable (Global Constraints line 98, A11Y-DESIGN-03).
    expect(button('Continue')).toHaveAttribute('aria-disabled', 'true');
    expect(button('Continue')).toHaveAccessibleDescription('Tick the box to continue.');
    act(() => button('Back').focus());
    await user.tab();
    expect(button('Continue')).toHaveFocus();
    await user.keyboard('{Enter}');
    expect(h1()).toHaveTextContent('Your recovery key');
    await user.click(screen.getByRole('checkbox', { name: ACK }));
    expect(button('Continue')).toBeEnabled();
    expect(button('Continue')).not.toHaveAttribute('aria-disabled');
    expect(button('Continue')).not.toHaveAttribute('aria-describedby');
    expect(screen.queryByText('Tick the box to continue.')).toBeNull();
    await user.click(screen.getByRole('checkbox', { name: ACK }));
    expect(button('Continue')).toHaveAttribute('aria-disabled', 'true');
    expect(button('Continue')).toHaveAccessibleDescription('Tick the box to continue.');
    await user.click(button('Print'));
    expect(print).toHaveBeenCalledTimes(1);
    await expectNoAxeViolations(view.container);
    print.mockRestore();
  });
  it('waits for the key before showing it', async () => {
    const { user, fake } = setup();
    fake.handler = () => Promise.resolve(null);
    await toKeys(user);
    expect(h1()).toHaveTextContent('Your recovery key');
    expect(screen.getByRole('status')).toHaveTextContent('Making your keys');
    expect(screen.queryByRole('checkbox')).toBeNull();
    expect(button('Continue')).toHaveAttribute('aria-disabled', 'true');
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent('Your recovery key');
    act(() => fake.set('account', account({ phase: 'signup-keys', user: null, recoveryKey: KEY })));
    expect(screen.getByRole('checkbox', { name: ACK })).not.toBeChecked();
  });
  it('never makes new keys when going back', async () => {
    const { user, fake } = setup();
    await toKeys(user);
    await user.click(screen.getByRole('checkbox', { name: ACK }));
    await user.click(button('Back'));
    expect(h1()).toHaveTextContent('Choose your name');
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent('Your recovery key');
    expect(screen.getByRole('checkbox', { name: ACK })).toBeChecked();
    expect(fake.callsOf('signupBegin')).toHaveLength(1);
  });
});

describe('creating the account', () => {
  it('explains what this browser keeps and submits exactly what was entered', async () => {
    const { user, fake, view } = setup();
    await toBrowser(user);
    expect(h1()).toHaveTextContent('What this browser keeps');
    expect(screen.getByText('New account · step 4 of 6')).toBeInTheDocument();
    const paragraphs = [
      'This browser now holds the key of this device, in its storage for dilla.test. The key never leaves this browser.',
      'Clearing this site’s data removes the key and the messages kept here, and this browser stops being your device.',
      "To use this account in another browser, sign in there with your password and the second factor you add next, then this recovery key. Messages sent before that browser joins are not shown in it.",
      'A private window forgets all of this when it closes.',
    ].map(text => screen.getByText(text));
    for (let i = 1; i < paragraphs.length; i++) {
      expect(paragraphs[i - 1].compareDocumentPosition(paragraphs[i]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
    await expectNoAxeViolations(view.container);
    fake.handler = () => new Promise(() => {});
    await user.click(button('Create account'));
    expect(fake.callsOf('signupSubmit')).toEqual([{
      m: 'signupSubmit', invite: 'ABCD-EFGH', username: 'ada', display: 'Ada L', password: null, recoveryKeyAcknowledged: true,
    }]);
    // Pre-flight ruling (c) and Global Constraints "Accessibility rules": while busy the two buttons are
    // blocked with aria-disabled, never the native attribute, so focus stays on the pressed button.
    expect(button('Creating account…')).toHaveAttribute('aria-disabled', 'true');
    expect(button('Back')).toHaveAttribute('aria-disabled', 'true');
    expect(document.activeElement).toBe(button('Creating account…'));
    expect(screen.getByRole('status')).toHaveTextContent('Creating your account on dilla.test…');
  });
  it('sends nothing more and goes nowhere while the account is being created', async () => {
    const { user, fake } = setup();
    await toBrowser(user);
    fake.handler = () => new Promise(() => {});
    await user.click(button('Create account'));
    await user.click(button('Creating account…'));
    await user.keyboard('{Enter}');
    await user.click(button('Back'));
    expect(fake.callsOf('signupSubmit')).toHaveLength(1);
    expect(h1()).toHaveTextContent('What this browser keeps');
  });
  it('sends the code of a pasted link and the password when asked for', async () => {
    const { user, fake } = setup({ instance: { ...INSTANCE, passwordSignup: true } });
    await toIdentity(user, 'https://dilla.test/i/WXYZ-1234');
    await user.type(field('Username'), 'ada');
    await user.type(screen.getByLabelText('Password'), 'correct horse');
    await user.click(button('Continue'));
    await user.click(screen.getByRole('checkbox', { name: ACK }));
    await user.click(button('Continue'));
    await user.click(button('Create account'));
    expect(fake.callsOf('signupSubmit')[0]).toEqual({
      m: 'signupSubmit', invite: 'WXYZ-1234', username: 'ada', display: '', password: 'correct horse', recoveryKeyAcknowledged: true,
    });
  });
  it('finishes only once the account is ready', async () => {
    const { user, fake, onFinish } = setup();
    await toBrowser(user);
    await user.click(button('Create account'));
    act(() => fake.set('account', account({ phase: 'registering', user: null, recoveryKey: KEY })));
    expect(h1()).toHaveTextContent('What this browser keeps');
    expect(screen.getByRole('status')).toHaveTextContent('Creating your account on dilla.test…');
    act(() => fake.set('account', account({ phase: 'ready' })));
    expect(h1()).toHaveTextContent('You’re in');
    expect(document.activeElement).toBe(h1());
    expect(screen.getByText('New account · step 6 of 6')).toBeInTheDocument();
    expect(screen.getByText('You are ada on dilla.test.')).toBeInTheDocument();
    await user.click(button('Open dilla'));
    expect(onFinish).toHaveBeenCalledTimes(1);
    expect(onFinish).toHaveBeenCalledWith({ communityId: null, joinError: null, invite: 'ABCD-EFGH' });
  });
  it('hands the joined server and a refused join to onFinish', async () => {
    const C = 'c4'.repeat(16);
    const joined = setup();
    await toBrowser(joined.user);
    joined.fake.handler = c => Promise.resolve(c.m === 'signupSubmit' ? { communityId: C, joinError: null } : null);
    await joined.user.click(button('Create account'));
    act(() => joined.fake.set('account', account({ phase: 'ready' })));
    await joined.user.click(button('Open dilla'));
    expect(joined.onFinish).toHaveBeenCalledWith({ communityId: C, joinError: null, invite: 'ABCD-EFGH' });
    joined.view.unmount();
    const refused = setup();
    await toBrowser(refused.user);
    refused.fake.handler = c => Promise.resolve(c.m === 'signupSubmit'
      ? { communityId: null, joinError: { code: 'E_INVITE_INVALID', detail: 'spent', status: 410, retryAfterMs: null } } : null);
    await refused.user.click(button('Create account'));
    act(() => refused.fake.set('account', account({ phase: 'ready' })));
    await refused.user.click(button('Open dilla'));
    expect(refused.onFinish).toHaveBeenCalledWith({
      communityId: null, joinError: { code: 'E_INVITE_INVALID', detail: 'spent', status: 410, retryAfterMs: null }, invite: 'ABCD-EFGH',
    });
  });
  it('waits for the join that follows the ready phase', async () => {
    const C = 'c4'.repeat(16);
    const { user, fake, onFinish } = setup();
    await toBrowser(user);
    let resolve: (value: unknown) => void = () => {};
    fake.handler = () => new Promise(r => { resolve = r; });
    await user.click(button('Create account'));
    act(() => fake.set('account', account({ phase: 'ready' })));
    expect(h1()).toHaveTextContent('What this browser keeps');
    await act(async () => { resolve({ communityId: C, joinError: null }); await Promise.resolve(); });
    expect(h1()).toHaveTextContent('You’re in');
    await user.click(button('Open dilla'));
    expect(onFinish).toHaveBeenCalledWith({ communityId: C, joinError: null, invite: 'ABCD-EFGH' });
  });
  it('reads a malformed result as no server', async () => {
    const { user, fake, onFinish } = setup();
    await toBrowser(user);
    fake.handler = () => Promise.resolve({ communityId: 'C4'.repeat(16), joinError: null });
    await user.click(button('Create account'));
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

describe('refusals', () => {
  it('an invalid invite returns focus to the invite field', async () => {
    const { user, fake } = setup();
    await toBrowser(user);
    fake.handler = () => Promise.reject(refusal({ code: 'E_INVITE_INVALID', detail: 'the invite is spent, expired, revoked or unknown', status: 410 }));
    await user.click(button('Create account'));
    expect(h1()).toHaveTextContent('Join dilla.test');
    expect(field('Invite')).toHaveAccessibleDescription(expect.stringContaining('This invite is expired, used up or unknown. Ask for a new one.'));
    expect(document.activeElement).toBe(field('Invite'));
  });
  it('a taken username is told by its status', async () => {
    const { user, fake } = setup();
    await toBrowser(user);
    fake.handler = () => Promise.reject(refusal({ code: 'E_INVALID_REQUEST', detail: 'reworded by the server', status: 409, retryAfterMs: null }));
    await user.click(button('Create account'));
    expect(h1()).toHaveTextContent('Choose your name');
    expect(field('Username')).toHaveAccessibleDescription(expect.stringContaining('That username is taken. Try another.'));
    expect(document.activeElement).toBe(field('Username'));
  });
  it('a refused detail is not parsed', async () => {
    const { user, fake } = setup();
    await toBrowser(user);
    fake.handler = () => Promise.reject(refusal({ code: 'E_INVALID_REQUEST', detail: 'username taken', status: 400, retryAfterMs: null }));
    await user.click(button('Create account'));
    expect(h1()).toHaveTextContent('Choose your name');
    expect(document.activeElement).toBe(h1());
    expect(screen.getByRole('alert')).toHaveTextContent('dilla.test did not accept these details. Check the username and the display name.');
    expect(screen.queryByText(/That username is taken/)).toBeNull();
    expect(field('Username')).not.toHaveAccessibleDescription(expect.stringContaining('taken'));
  });
  it('lets a taken username be changed without new keys or a new acknowledgement', async () => {
    const { user, fake } = setup();
    await toBrowser(user);
    fake.handler = () => Promise.reject(refusal({ code: 'E_INVALID_REQUEST', status: 409 }));
    await user.click(button('Create account'));
    fake.handler = () => Promise.resolve({ communityId: null, joinError: null });
    await user.clear(field('Username'));
    await user.type(field('Username'), 'ada2');
    expect(field('Username')).not.toHaveAccessibleDescription(expect.stringContaining('taken'));
    await user.click(button('Continue'));
    expect(button('Continue')).toBeEnabled();
    await user.click(button('Continue'));
    await user.click(button('Create account'));
    expect(fake.callsOf('signupSubmit').map(c => c.username)).toEqual(['ada', 'ada2']);
    expect(fake.callsOf('signupBegin')).toHaveLength(1);
  });
  it('shows the closed frame when registration closed meanwhile', async () => {
    const { user, fake } = setup();
    await toBrowser(user);
    fake.handler = () => Promise.reject(refusal({ code: 'E_FORBIDDEN', detail: 'this instance is not accepting registrations', status: 403 }));
    await user.click(button('Create account'));
    expect(h1()).toHaveTextContent('dilla.test is not taking new accounts');
    expect(screen.queryByRole('button')).toBeNull();
  });
  it.each([
    [4200, 'Too many attempts from this network. Try again in 5 s.'],
    [null, 'Too many attempts from this network. Wait a few minutes, then try again.'],
  ] as const)('the wait is shown (retryAfterMs %s)', async (retryAfterMs, message) => {
    const { user, fake } = setup();
    await toBrowser(user);
    fake.handler = () => Promise.reject(refusal({ code: 'E_RATE_LIMITED', detail: '', status: 429, retryAfterMs }));
    await user.click(button('Create account'));
    expect(screen.getByRole('alert')).toHaveTextContent(message);
    expect(h1()).toHaveTextContent('What this browser keeps');
    expect(button('Create account')).toBeEnabled();
  });
  it.each([
    [{ code: 'E_NETWORK', detail: '', status: 0, retryAfterMs: null }],
    [{ code: 'E_HTTP', status: 502 }],
  ])('a lost response is never answered with a second registration (%j)', async init => {
    const reload = vi.spyOn(browser, 'reload').mockImplementation(() => {});
    const { user, fake } = setup();
    await toBrowser(user);
    fake.handler = () => Promise.reject(refusal(init));
    await user.click(button('Create account'));
    const banner = screen.getByRole('alert');
    expect(banner).toHaveTextContent('The connection dropped while creating your account. Reload the page to finish.');
    await user.click(within(banner).getByRole('button', { name: 'Reload' }));
    expect(reload).toHaveBeenCalledTimes(1);
    expect(fake.callsOf('signupSubmit')).toHaveLength(1);
    reload.mockRestore();
  });
  // Requirement 6, the E_NETWORK row: the page never sends signupSubmit again in this case, so after
  // the network banner Create account and Back stay blocked (aria-disabled) and Reload is the only way on.
  it.each([
    [{ code: 'E_NETWORK', detail: '', status: 0, retryAfterMs: null }],
    [{ code: 'E_HTTP', status: 502 }],
  ])('after a lost response only Reload goes on (%j)', async init => {
    const { user, fake } = setup();
    await toBrowser(user);
    fake.handler = () => Promise.reject(refusal(init));
    await user.click(button('Create account'));
    expect(screen.getByRole('alert')).toHaveTextContent('The connection dropped while creating your account. Reload the page to finish.');
    expect(button('Create account')).toHaveAttribute('aria-disabled', 'true');
    expect(button('Back')).toHaveAttribute('aria-disabled', 'true');
    expect(document.activeElement).toBe(button('Create account'));
    await user.click(button('Create account'));
    await user.keyboard('{Enter}');
    await user.click(button('Back'));
    expect(fake.callsOf('signupSubmit')).toHaveLength(1);
    expect(h1()).toHaveTextContent('What this browser keeps');
    expect(screen.getByRole('alert')).toHaveTextContent('The connection dropped while creating your account. Reload the page to finish.');
  });
  it('another refusal can be tried again', async () => {
    const { user, fake } = setup();
    await toBrowser(user);
    fake.handler = () => Promise.reject(refusal({ code: 'E_CORE_STATE', detail: '', status: 0, retryAfterMs: null }));
    await user.click(button('Create account'));
    const banner = screen.getByRole('alert');
    expect(banner).toHaveTextContent('The account could not be created (E_CORE_STATE). Try again.');
    expect(within(banner).queryByRole('button')).toBeNull();
    expect(button('Create account')).toBeEnabled();
  });
  // Pre-flight ruling (a): the banner is the first child of the step's body, inside main, under the step line.
  it('puts a refusal banner first in the step body, without moving focus', async () => {
    const { user, fake } = setup();
    await toBrowser(user);
    fake.handler = () => Promise.reject(refusal({ code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 1000 }));
    await user.click(button('Create account'));
    const banner = screen.getByRole('alert');
    expect(screen.getByRole('main')).toContainElement(banner);
    expect(banner.parentElement).toHaveClass('d-onboarding-frame__body');
    expect(banner.parentElement?.firstElementChild).toBe(banner);
    expect(document.activeElement).toBe(button('Create account'));
  });
  // Pre-flight ruling (d): Back clears the banner.
  it('clears a banner on Back', async () => {
    const { user, fake } = setup();
    await toBrowser(user);
    fake.handler = () => Promise.reject(refusal({ code: 'E_CORE_STATE' }));
    await user.click(button('Create account'));
    expect(screen.getByRole('alert')).toHaveTextContent('The account could not be created (E_CORE_STATE). Try again.');
    await user.click(button('Back'));
    expect(h1()).toHaveTextContent('Your recovery key');
    expect(screen.queryByRole('alert')).toBeNull();
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent('What this browser keeps');
    expect(screen.queryByRole('alert')).toBeNull();
  });
});

describe('signing in instead', () => {
  it('offers an existing account below the invite when the instance takes passwords', async () => {
    const { user, fake, view } = setup({ instance: { ...INSTANCE, passwordSignup: true } });
    const entry = button('Use an existing account');
    expect(entry).toHaveAttribute('type', 'button');
    expect(field('Invite').compareDocumentPosition(entry) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(entry.compareDocumentPosition(button('Continue')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    await expectNoAxeViolations(view.container);
    await user.click(entry);
    expect(fake.callsOf('signInBegin')).toEqual([{ m: 'signInBegin' }]);
    expect(fake.callsOf('signupBegin')).toEqual([]);
  });
  it('offers no entry when the instance has no password login', () => {
    setup();
    expect(screen.queryByRole('button', { name: 'Use an existing account' })).toBeNull();
  });
  it('offers the entry on a closed instance too, as its only control', () => {
    setup({ instance: { ...INSTANCE, registrationMode: 2, passwordSignup: true } });
    expect(h1()).toHaveTextContent('dilla.test is not taking new accounts');
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual(['Use an existing account']);
  });
  it('is not offered after the first step', async () => {
    const { user } = setup({ instance: { ...INSTANCE, passwordSignup: true } });
    await toIdentity(user);
    expect(screen.queryByRole('button', { name: 'Use an existing account' })).toBeNull();
  });
  it('shows a refused entry and sends it once while in flight', async () => {
    const { user, fake } = setup({ instance: { ...INSTANCE, passwordSignup: true } });
    let release: (e: unknown) => void = () => {};
    fake.handler = c => (c.m === 'signInBegin' ? new Promise((_, reject) => { release = reject; }) : Promise.resolve(null));
    await user.click(button('Use an existing account'));
    expect(button('Use an existing account')).toHaveAttribute('aria-disabled', 'true');
    await user.click(button('Use an existing account'));
    expect(fake.callsOf('signInBegin')).toHaveLength(1);
    await act(async () => { release(refusal({ code: 'E_STATE' })); await Promise.resolve(); });
    expect(screen.getByRole('alert')).toHaveTextContent('Signing in did not work (E_STATE). Try again.');
    expect(button('Use an existing account')).not.toHaveAttribute('aria-disabled');
  });
});

describe('after a list race', () => {
  const RACE = 'The account’s devices changed while you were signing in. Sign in again.';
  it('says the devices changed on the connect step, and drops the query when it moves on', async () => {
    const { user, view } = setup({ instance: { ...INSTANCE, passwordSignup: true } }, '/welcome?signin=race');
    expect(h1()).toHaveTextContent('Join dilla.test');
    expect(screen.getByRole('alert')).toHaveTextContent(RACE);
    await expectNoAxeViolations(view.container);
    await toIdentity(user);
    expect(window.location.pathname + window.location.search).toBe('/welcome');
    expect(screen.queryByText(RACE)).toBeNull();
  });
  it('keeps the invite in the address when the entry drops the race', async () => {
    const { user, fake } = setup({ instance: { ...INSTANCE, passwordSignup: true } }, '/welcome?invite=ABCD-EFGH&signin=race');
    expect(field('Invite')).toHaveValue('ABCD-EFGH');
    await user.click(button('Use an existing account'));
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
  it('says someone else is signing in on the connect step, and drops the query when it moves on', async () => {
    const { user, view } = setup({ instance: { ...INSTANCE, passwordSignup: true } }, '/welcome?signin=evicted');
    expect(h1()).toHaveTextContent('Join dilla.test');
    expect(screen.getByRole('alert')).toHaveTextContent(EVICTED);
    await expectNoAxeViolations(view.container);
    await user.click(button('Use an existing account'));
    expect(window.location.pathname + window.location.search).toBe('/welcome');
    expect(screen.queryByText(EVICTED)).toBeNull();
  });
});
