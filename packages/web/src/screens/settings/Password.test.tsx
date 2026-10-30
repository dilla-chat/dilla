// E1 (flow 15): Settings → Sign-in, change or set the password.
import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { Command, FactorsState } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { account } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { en } from '../../strings/index.ts';
import { Settings } from '../Settings.tsx';
import { getPasskey } from '../../webauthn.ts';

vi.mock('../../webauthn.ts', async (original) => ({ ...await original<typeof import('../../webauthn.ts')>(), getPasskey: vi.fn(), createPasskey: vi.fn() }));

const PK = 'ab'.repeat(16);
const WITH_PASSWORD: FactorsState = { owed: false, totp: { confirmedAt: 1_900_000_000 }, passkeys: [], backupCodesLeft: 8, hasPassword: true };
const NO_PASSWORD_NO_FACTOR: FactorsState = { owed: false, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: false };
const NO_PASSWORD_TOTP: FactorsState = { owed: false, totp: { confirmedAt: 1_900_000_000 }, passkeys: [], backupCodesLeft: 8, hasPassword: false };
const NO_PASSWORD_PASSKEY: FactorsState = { owed: false, totp: null,
  passkeys: [{ id: PK, name: 'laptop', created: 1_900_000_100, lastUsed: null }], backupCodesLeft: 8, hasPassword: false };

type Script = (c: Command, fake: FakeClient) => Promise<unknown>;
const happy: Script = (c, f) => {
  switch (c.m) {
    case 'stepUp': return Promise.resolve({ token: 'tok-code' });
    case 'stepUpPasskeyBegin': return Promise.resolve({ ceremonyId: '11'.repeat(16), options: '{}' });
    case 'stepUpPasskeyFinish': return Promise.resolve({ token: 'tok-pk' });
    case 'changePassword':
      f.set('factors', { ...(f.get('factors') as FactorsState), hasPassword: true });
      return Promise.resolve(null);
    default: return Promise.resolve(null);
  }
};

function setup(factors: FactorsState, script: Script = happy) {
  window.history.replaceState(null, '', '/settings/signin');
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('devices', []);
  fake.set('settings', {});
  fake.set('communities', []);
  fake.set('dms', []);
  fake.set('factors', factors);
  fake.handler = c => script(c, fake);
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><Settings /></CoreProvider></div>);
  return { fake, user, view };
}
const dialog = (name: string) => screen.getByRole('dialog', { name });
async function fill(user: ReturnType<typeof userEvent.setup>, d: HTMLElement, fields: { old?: string; next?: string; again?: string }) {
  if (fields.old !== undefined) await user.type(within(d).getByLabelText(en['settings.password.old']), fields.old);
  if (fields.next !== undefined) await user.type(within(d).getByLabelText(en['settings.password.new']), fields.next);
  if (fields.again !== undefined) await user.type(within(d).getByLabelText(en['settings.password.confirm']), fields.again);
}

describe('Settings → Sign-in → password row', () => {
  it('offers "change password" where the account has a password and "set password" where it has none', async () => {
    setup(WITH_PASSWORD);
    expect(await screen.findByRole('button', { name: en['settings.password.change'] })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: en['settings.password.add'] })).toBeNull();
  });
  it('says "set password" for an account with none', async () => {
    setup(NO_PASSWORD_NO_FACTOR);
    expect(await screen.findByRole('button', { name: en['settings.password.add'] })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: en['settings.password.change'] })).toBeNull();
  });
});

describe('changing the password', () => {
  it('sends old and new once, with no step-up, closes, announces and returns focus to the row button', async () => {
    const { fake, user } = setup(WITH_PASSWORD);
    const opener = await screen.findByRole('button', { name: en['settings.password.change'] });
    await user.click(opener);
    const d = dialog(en['settings.password.titleChange']);
    expect(within(d).getByText(en['settings.password.bodyChange'])).toBeInTheDocument();
    expect(within(d).getByLabelText(en['settings.password.old'])).toHaveFocus();
    expect(within(d).getByLabelText(en['settings.password.old'])).toHaveAttribute('type', 'password');
    expect(within(d).getByLabelText(en['settings.password.old'])).toHaveAttribute('autocomplete', 'current-password');
    expect(within(d).getByLabelText(en['settings.password.new'])).toHaveAttribute('autocomplete', 'new-password');
    expect(within(d).getByLabelText(en['settings.password.confirm'])).toHaveAttribute('autocomplete', 'new-password');
    await fill(user, d, { old: 'old password', next: 'new password', again: 'new password{Enter}' });
    await waitFor(() => { expect(screen.queryByRole('dialog', { name: en['settings.password.titleChange'] })).toBeNull(); });
    expect(fake.callsOf('changePassword')).toEqual([{ m: 'changePassword', old: 'old password', next: 'new password', stepup: null }]);
    expect(fake.callsOf('stepUp')).toEqual([]);
    expect(screen.getByTestId('settings-announcer')).toHaveTextContent(en['settings.password.changed']);
    expect(opener).toHaveFocus();
  });
  it('refuses a short, an empty and a mismatched password before anything is sent, and focuses the field', async () => {
    const { fake, user } = setup(WITH_PASSWORD);
    await user.click(await screen.findByRole('button', { name: en['settings.password.change'] }));
    const d = dialog(en['settings.password.titleChange']);
    const confirm = () => within(d).getByRole('button', { name: en['settings.password.confirmChange'] });
    await user.click(confirm());
    expect(within(d).getByText(en['settings.password.error.oldRequired'])).toBeInTheDocument();
    expect(within(d).getByLabelText(en['settings.password.old'])).toHaveFocus();
    await fill(user, d, { old: 'old password', next: 'short', again: 'short' });
    await user.click(confirm());
    expect(within(d).getByText(en['settings.password.error.short'])).toBeInTheDocument();
    expect(within(d).getByLabelText(en['settings.password.new'])).toHaveFocus();
    await user.type(within(d).getByLabelText(en['settings.password.new']), 'er and longer');
    await user.click(confirm());
    expect(within(d).getByText(en['settings.password.error.mismatch'])).toBeInTheDocument();
    expect(within(d).getByLabelText(en['settings.password.confirm'])).toHaveFocus();
    expect(fake.callsOf('changePassword')).toEqual([]);
  });
  // Wave B task 3b (Jonas): a new password has at least 12 characters, counted in code points; 11 is refused here.
  it('refuses 11 characters and sends 12 (code points, not UTF-16 units)', async () => {
    const { fake, user } = setup(WITH_PASSWORD);
    await user.click(await screen.findByRole('button', { name: en['settings.password.change'] }));
    const d = dialog(en['settings.password.titleChange']);
    expect(en['settings.password.newHint']).toBe('At least 12 characters.');
    await fill(user, d, { old: 'old password', next: '🔑'.repeat(11), again: '🔑'.repeat(11) });
    await user.click(within(d).getByRole('button', { name: en['settings.password.confirmChange'] }));
    expect(within(d).getByText('Use at least 12 characters.')).toBeInTheDocument();
    expect(fake.callsOf('changePassword')).toEqual([]);
    await user.type(within(d).getByLabelText(en['settings.password.new']), '🔑');
    await user.type(within(d).getByLabelText(en['settings.password.confirm']), '🔑');
    await user.click(within(d).getByRole('button', { name: en['settings.password.confirmChange'] }));
    await waitFor(() => { expect(fake.callsOf('changePassword')).toHaveLength(1); });
    expect(fake.callsOf('changePassword')[0]).toMatchObject({ next: '🔑'.repeat(12) });
  });
  it('a wrong old password is an alert, empties the old field and puts focus there; the new ones stay', async () => {
    const { fake, user } = setup(WITH_PASSWORD, (c, f) => c.m === 'changePassword' ? Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 })) : happy(c, f));
    await user.click(await screen.findByRole('button', { name: en['settings.password.change'] }));
    const d = dialog(en['settings.password.titleChange']);
    await fill(user, d, { old: 'wrong one', next: 'new password', again: 'new password{Enter}' });
    expect(await within(d).findByRole('alert')).toHaveTextContent(en['settings.password.error.wrong']);
    expect(within(d).getByLabelText(en['settings.password.old'])).toHaveValue('');
    expect(within(d).getByLabelText(en['settings.password.old'])).toHaveFocus();
    expect(within(d).getByLabelText(en['settings.password.new'])).toHaveValue('new password');
    expect(fake.callsOf('changePassword')).toHaveLength(1);
  });
  it('a throttle shows the seconds the instance names', async () => {
    const { user } = setup(WITH_PASSWORD, (c, f) => c.m === 'changePassword' ? Promise.reject(refusal({ code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 42_000 })) : happy(c, f));
    await user.click(await screen.findByRole('button', { name: en['settings.password.change'] }));
    const d = dialog(en['settings.password.titleChange']);
    await fill(user, d, { old: 'old password', next: 'new password', again: 'new password{Enter}' });
    expect(await within(d).findByRole('alert')).toHaveTextContent(en['factor.error.tooMany'].replace('{seconds}', '42'));
  });
  it('any other refusal names its code', async () => {
    const { user } = setup(WITH_PASSWORD, (c, f) => c.m === 'changePassword' ? Promise.reject(refusal({ code: 'E_INTERNAL', status: 500 })) : happy(c, f));
    await user.click(await screen.findByRole('button', { name: en['settings.password.change'] }));
    const d = dialog(en['settings.password.titleChange']);
    await fill(user, d, { old: 'old password', next: 'new password', again: 'new password{Enter}' });
    expect(await within(d).findByRole('alert')).toHaveTextContent(en['settings.password.error.other'].replace('{code}', 'E_INTERNAL'));
  });
  it('Escape closes only the dialog, sends nothing and returns focus to the row button', async () => {
    const { fake, user } = setup(WITH_PASSWORD);
    const opener = await screen.findByRole('button', { name: en['settings.password.change'] });
    await user.click(opener);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: en['settings.password.titleChange'] })).toBeNull();
    expect(screen.getByRole('dialog', { name: 'Settings' })).toBeInTheDocument();
    expect(opener).toHaveFocus();
    expect(fake.callsOf('changePassword')).toEqual([]);
  });
  it('has no axe violations', async () => {
    const { user, view } = setup(WITH_PASSWORD);
    await user.click(await screen.findByRole('button', { name: en['settings.password.change'] }));
    await screen.findByRole('dialog', { name: en['settings.password.titleChange'] });
    await expectNoAxeViolations(view.container);
  });
});

describe('setting a first password', () => {
  it('on an account with no second factor takes no step-up and sends a null old password', async () => {
    const { fake, user } = setup(NO_PASSWORD_NO_FACTOR);
    await user.click(await screen.findByRole('button', { name: en['settings.password.add'] }));
    const d = dialog(en['settings.password.titleSet']);
    expect(within(d).queryByLabelText(en['settings.password.old'])).toBeNull();
    await fill(user, d, { next: 'first password', again: 'first password{Enter}' });
    await waitFor(() => { expect(screen.queryByRole('dialog', { name: en['settings.password.titleSet'] })).toBeNull(); });
    expect(fake.callsOf('changePassword')).toEqual([{ m: 'changePassword', old: null, next: 'first password', stepup: null }]);
    expect(screen.getByTestId('settings-announcer')).toHaveTextContent(en['settings.password.added']);
  });
  it('on an account with a passkey steps up first and sends the token', async () => {
    vi.mocked(getPasskey).mockResolvedValueOnce('{"assertion":1}');
    const { fake, user } = setup(NO_PASSWORD_PASSKEY);
    const opener = await screen.findByRole('button', { name: en['settings.password.add'] });
    await user.click(opener);
    await user.click(within(screen.getByRole('dialog', { name: en['factorSettings.stepUp.title'] })).getByRole('button', { name: en['factorSettings.stepUp.usePasskey'] }));
    const d = await screen.findByRole('dialog', { name: en['settings.password.titleSet'] });
    await fill(user, d, { next: 'first password', again: 'first password{Enter}' });
    await waitFor(() => { expect(screen.queryByRole('dialog', { name: en['settings.password.titleSet'] })).toBeNull(); });
    expect(fake.callsOf('changePassword')).toEqual([{ m: 'changePassword', old: null, next: 'first password', stepup: 'tok-pk' }]);
  });
  it('a token the instance spent or let run out goes back to the step-up with the expired notice', async () => {
    vi.mocked(getPasskey).mockResolvedValue('{"assertion":1}');
    const { user } = setup(NO_PASSWORD_PASSKEY, (c, f) => c.m === 'changePassword' ? Promise.reject(refusal({ code: 'E_STEP_UP', status: 403 })) : happy(c, f));
    await user.click(await screen.findByRole('button', { name: en['settings.password.add'] }));
    await user.click(within(screen.getByRole('dialog', { name: en['factorSettings.stepUp.title'] })).getByRole('button', { name: en['factorSettings.stepUp.usePasskey'] }));
    const d = await screen.findByRole('dialog', { name: en['settings.password.titleSet'] });
    await fill(user, d, { next: 'first password', again: 'first password{Enter}' });
    const again = await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] });
    expect(within(again).getByText(en['factorSettings.stepUp.expired'])).toBeInTheDocument();
  });
  it('a session that went stale while the dialog was open closes it with a calm line, and the next press mints a fresh one', async () => {
    let stale = true;
    const { fake, user } = setup(NO_PASSWORD_NO_FACTOR, (c, f) => c.m === 'changePassword' && stale ? Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 })) : happy(c, f));
    await user.click(await screen.findByRole('button', { name: en['settings.password.add'] }));
    const d = dialog(en['settings.password.titleSet']);
    await fill(user, d, { next: 'first password', again: 'first password{Enter}' });
    await waitFor(() => { expect(screen.queryByRole('dialog', { name: en['settings.password.titleSet'] })).toBeNull(); });
    expect(await screen.findByText(en['settings.password.error.stale'])).toBeInTheDocument();
    expect(en['settings.password.error.stale']).not.toMatch(/sign out/i);
    stale = false;
    await user.click(screen.getByRole('button', { name: en['settings.password.add'] }));
    expect(fake.callsOf('freshSession')).toHaveLength(2);
  });
  it('mints a fresh session before the step-up, and again before a set with no factor', async () => {
    vi.mocked(getPasskey).mockResolvedValueOnce('{"assertion":1}');
    const { fake, user } = setup(NO_PASSWORD_PASSKEY);
    await user.click(await screen.findByRole('button', { name: en['settings.password.add'] }));
    await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] });
    await user.click(within(screen.getByRole('dialog', { name: en['factorSettings.stepUp.title'] })).getByRole('button', { name: en['factorSettings.stepUp.usePasskey'] }));
    await screen.findByRole('dialog', { name: en['settings.password.titleSet'] });
    const order = fake.calls.map(c => c.m);
    expect(order.indexOf('freshSession')).toBeGreaterThanOrEqual(0);
    expect(order.indexOf('freshSession')).toBeLessThan(order.indexOf('stepUpPasskeyBegin'));
  });
  it('a change does not mint a session', async () => {
    const { fake, user } = setup(WITH_PASSWORD);
    await user.click(await screen.findByRole('button', { name: en['settings.password.change'] }));
    expect(fake.callsOf('freshSession')).toEqual([]);
  });
  it('an account with only an authenticator app confirms with a code and sets a password', async () => {
    const { fake, user } = setup(NO_PASSWORD_TOTP);
    await user.click(await screen.findByRole('button', { name: en['settings.password.add'] }));
    const step = await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] });
    expect(within(step).getByText(en['factorSettings.stepUp.codeBody'])).toBeInTheDocument();
    await user.type(within(step).getByLabelText(en['factorSettings.stepUp.code']), '123456{Enter}');
    const d = await screen.findByRole('dialog', { name: en['settings.password.titleSet'] });
    await fill(user, d, { next: 'first password', again: 'first password{Enter}' });
    await waitFor(() => { expect(screen.queryByRole('dialog', { name: en['settings.password.titleSet'] })).toBeNull(); });
    expect(fake.callsOf('stepUp')).toEqual([{ m: 'stepUp', method: 'totp', secret: '123456' }]);
    expect(fake.callsOf('changePassword')).toEqual([{ m: 'changePassword', old: null, next: 'first password', stepup: 'tok-code' }]);
  });
  it('a backup code is sent as a backup code, and a malformed one is refused before it is sent', async () => {
    const { fake, user } = setup(NO_PASSWORD_TOTP);
    await user.click(await screen.findByRole('button', { name: en['settings.password.add'] }));
    const step = await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] });
    await user.type(within(step).getByLabelText(en['factorSettings.stepUp.code']), 'nope{Enter}');
    expect(await within(step).findByRole('alert')).toHaveTextContent(en['factorSettings.stepUp.codeFailed']);
    expect(fake.callsOf('stepUp')).toEqual([]);
    await user.clear(within(step).getByLabelText(en['factorSettings.stepUp.code']));
    await user.type(within(step).getByLabelText(en['factorSettings.stepUp.code']), 'aaaaa-aaaaa-aaaaa-aaaaa-aaaaa-a{Enter}');
    await screen.findByRole('dialog', { name: en['settings.password.titleSet'] });
    expect(fake.callsOf('stepUp')).toEqual([{ m: 'stepUp', method: 'backup', secret: 'a'.repeat(26) }]);
  });
});

describe('the dialog\'s form', () => {
  it('holds a hidden-from-sight username for the password manager', async () => {
    const { user } = setup(WITH_PASSWORD);
    await user.click(await screen.findByRole('button', { name: en['settings.password.change'] }));
    const field = within(dialog(en['settings.password.titleChange'])).getByLabelText(en['settings.password.username']);
    expect(field).toHaveAttribute('autocomplete', 'username');
    expect(field).toHaveAttribute('readonly');
    expect(field).toHaveValue(account().user?.username);
  });
  it('caps the new password at 1024 characters, the server has no bound of its own', async () => {
    const { user } = setup(WITH_PASSWORD);
    await user.click(await screen.findByRole('button', { name: en['settings.password.change'] }));
    const d = dialog(en['settings.password.titleChange']);
    expect(within(d).getByLabelText(en['settings.password.new'])).toHaveAttribute('maxlength', '1024');
    expect(within(d).getByLabelText(en['settings.password.confirm'])).toHaveAttribute('maxlength', '1024');
  });
  it('is empty when it is opened again after Cancel, and after a refusal', async () => {
    const { user } = setup(WITH_PASSWORD, (c, f) => c.m === 'changePassword' ? Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 })) : happy(c, f));
    const open = async () => { await user.click(await screen.findByRole('button', { name: en['settings.password.change'] })); return dialog(en['settings.password.titleChange']); };
    let d = await open();
    await fill(user, d, { old: 'old password', next: 'new password', again: 'new password' });
    await user.keyboard('{Escape}');
    d = await open();
    for (const key of ['old', 'new', 'confirm'] as const) expect(within(d).getByLabelText(en[`settings.password.${key}`])).toHaveValue('');
    await fill(user, d, { old: 'old password', next: 'new password', again: 'new password{Enter}' });
    await within(d).findByRole('alert');
    await user.click(within(d).getByRole('button', { name: en['settings.password.cancel'] }));
    d = await open();
    for (const key of ['old', 'new', 'confirm'] as const) expect(within(d).getByLabelText(en[`settings.password.${key}`])).toHaveValue('');
  });
});
