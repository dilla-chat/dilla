// web3.6 W3: Settings → Sign-in (flow 10 state 5): the factor rows, the step-up dialog and the three actions behind it.
import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { Command, FactorsState } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { account } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { en, formatDay } from '../../strings/index.ts';
import { liveRegions } from '../../test/live-regions.ts';
import { Settings } from '../Settings.tsx';
import { PasskeyError, createPasskey, getPasskey } from '../../webauthn.ts';

vi.mock('../../webauthn.ts', async (original) => ({ ...await original<typeof import('../../webauthn.ts')>(), getPasskey: vi.fn(), createPasskey: vi.fn() }));

const PK = 'ab'.repeat(16);
const PK2 = 'cd'.repeat(16);
const BOTH: FactorsState = { owed: false, totp: { confirmedAt: 1_900_000_000 },
  passkeys: [{ id: PK, name: 'laptop', created: 1_900_000_100, lastUsed: 1_900_000_200 }, { id: PK2, name: 'phone', created: 1_900_000_300, lastUsed: null }],
  backupCodesLeft: 8, hasPassword: true };
const ONLY_TOTP: FactorsState = { owed: false, totp: { confirmedAt: 1_900_000_000 }, passkeys: [], backupCodesLeft: 1, hasPassword: true };
const ONLY_PASSKEY: FactorsState = { owed: false, totp: null, passkeys: [{ id: PK, name: 'laptop', created: 1_900_000_100, lastUsed: null }],
  backupCodesLeft: 0, hasPassword: true };
const SECRET = 'JBSWY3DPEHPK3PXP';
const CODES = ['AAAAABBBBBCCCCCDDDDDEEEEEF', 'GGGGGHHHHHIIIIIJJJJJKKKKKL'];

type Script = (c: Command, fake: FakeClient) => Promise<unknown>;
const happy: Script = (c, f) => {
  switch (c.m) {
    case 'stepUp': return Promise.resolve({ token: 'tok-1' });
    case 'stepUpPasskeyBegin': return Promise.resolve({ ceremonyId: '11'.repeat(16), options: '{}' });
    case 'regenerateBackupCodes': return Promise.resolve({ backupCodes: CODES });
    case 'removeFactor':
      f.set('factors', { ...BOTH, totp: c.kind === 'totp' ? null : BOTH.totp,
        passkeys: c.kind === 'passkey' ? BOTH.passkeys.filter(k => k.id !== c.credId) : BOTH.passkeys });
      return Promise.resolve(null);
    case 'factorTotpBegin': return Promise.resolve({ secret: SECRET, uri: `otpauth://totp/dilla.test:ada?secret=${SECRET}` });
    case 'factorTotpConfirm': return Promise.resolve({ backupCodes: CODES });
    default: return Promise.resolve(null);
  }
};

function setup(factors: FactorsState | undefined = BOTH, script: Script = happy) {
  window.history.replaceState(null, '', '/settings/signin');
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('devices', []);
  fake.set('settings', {});
  fake.set('communities', []);
  fake.set('dms', []);
  if (factors !== undefined) fake.set('factors', factors);
  fake.handler = c => script(c, fake);
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><Settings /></CoreProvider></div>);
  return { fake, user, view };
}
const button = (name: string | RegExp) => screen.getByRole('button', { name });
const stepUp = () => screen.getByRole('dialog', { name: en['factorSettings.stepUp.title'] });
const removeTitle = (name: string) => en['factorSettings.removeTitle.passkey'].replace('{name}', name);
async function confirmPassword(user: ReturnType<typeof userEvent.setup>, password = 'hunter2') {
  await user.type(within(stepUp()).getByLabelText(en['factorSettings.stepUp.password']), `${password}{Enter}`);
}
/** Every non-empty text the page's polite regions held, in order. The page has exactly one region at any time. */
function watchLive() {
  const history: string[] = [];
  let last = '';
  const read = () => {
    const text = liveRegions().map(r => r.textContent ?? '').join('');
    if (text !== '' && text !== last) history.push(text);
    last = text;
  };
  const observer = new MutationObserver(read);
  observer.observe(document.body, { childList: true, characterData: true, subtree: true });
  return { history, stop: () => { observer.disconnect(); } };
}
/** The dialog the one region sits in (the topmost modal: the frame beneath is inert while another is open), null for the frame. */
function regionHome(): HTMLElement | null {
  const regions = liveRegions();
  expect(regions).toHaveLength(1);
  return (regions[0] as HTMLElement).closest('dialog:not(.d-settings-frame)');
}

describe('Settings → Sign-in rows', () => {
  it('gives the password, app, passkeys and backup codes groups h3 headings under the h2, each naming its group', async () => {
    setup();
    await screen.findByRole('heading', { level: 2, name: en['factorSettings.title'] });
    for (const key of ['factorSettings.password', 'factorSettings.totp', 'factorSettings.passkeys', 'factorSettings.codes'] as const) {
      expect(screen.getByRole('heading', { level: 3, name: en[key] })).toBeInTheDocument();
      expect(screen.getByRole('group', { name: en[key] })).toBeInTheDocument();
    }
    expect(screen.getAllByRole('heading', { level: 3 })).toHaveLength(4);
  });

  it('lists the password, the app, each passkey and the backup codes, and refreshes the factors once', async () => {
    const { fake } = setup();
    expect(await screen.findByRole('heading', { level: 2, name: en['factorSettings.title'] })).toBeInTheDocument();
    expect(fake.callsOf('refreshFactors')).toHaveLength(1);
    expect(screen.getByText(en['factorSettings.body'].replace('{instance}', 'dilla.test'))).toBeInTheDocument();
    // Task 11 (Jonas, Q7): the prototype's copy; a password whose change date is unknown shows no line under it.
    expect(screen.queryByText(/^Changed /)).toBeNull();
    expect(screen.getByText(en['factorSettings.totp.on'].replace('{date}', formatDay(1_900_000_000)))).toBeInTheDocument();
    expect(screen.getAllByText(en['factorSettings.passkey'], { exact: true })).toHaveLength(2);
    expect(screen.getByText('laptop')).toBeInTheDocument();
    expect(screen.getByText(en['factorSettings.codes.other'].replace('{count}', '8'))).toBeInTheDocument();
    expect(screen.queryByText(/Lost your phone/)).toBeNull();
    expect(button('Remove the authenticator app')).toBeInTheDocument();
    expect(button('Remove laptop')).toBeInTheDocument();
    expect(button(en['factorSettings.passkeys.add'])).toBeInTheDocument();
    expect(button(en['factorSettings.codes.make'])).toBeInTheDocument();
  });
  it('shows the empty states: no app, no passkeys, no password, one code', async () => {
    setup({ owed: false, totp: null, passkeys: [], backupCodesLeft: 1, hasPassword: false });
    expect(await screen.findByText(en['factorSettings.password.none'])).toBeInTheDocument();
    expect(screen.getByText(en['factorSettings.totp.off'])).toBeInTheDocument();
    expect(screen.getByText(en['factorSettings.passkeys.none'])).toBeInTheDocument();
    expect(screen.getByText(en['factorSettings.codes.one'])).toBeInTheDocument();
    expect(button(en['factorSettings.totp.add'])).toBeInTheDocument();
  });
  it('says there are no codes left', async () => {
    setup(ONLY_PASSKEY);
    expect(await screen.findByText(en['factorSettings.codes.none'])).toBeInTheDocument();
  });
  it('disables the remove on the only factor, with the reason, and does nothing on press', async () => {
    const { fake, user } = setup(ONLY_TOTP);
    const remove = await screen.findByRole('button', { name: 'Remove the authenticator app' });
    expect(remove).toHaveAttribute('aria-disabled', 'true');
    expect(remove).toHaveAccessibleDescription(en['factorSettings.only']);
    await user.click(remove);
    expect(screen.queryByRole('dialog', { name: en['factorSettings.stepUp.title'] })).toBeNull();
    expect(fake.callsOf('stepUp')).toEqual([]);
  });
  it('disables the remove on a single passkey when it is the only factor, but not when an app is also set up', async () => {
    const first = setup(ONLY_PASSKEY);
    expect(await screen.findByRole('button', { name: 'Remove laptop' })).toHaveAttribute('aria-disabled', 'true');
    first.view.unmount();
    setup({ ...ONLY_PASSKEY, totp: { confirmedAt: 1_900_000_000 } });
    expect(await screen.findByRole('button', { name: 'Remove laptop' })).not.toHaveAttribute('aria-disabled');
  });
  it('shows the load failure with its code and a retry that reads again', async () => {
    let n = 0;
    const { fake, user } = setup(undefined, (c, f) => {
      if (c.m === 'refreshFactors') { n += 1; if (n === 1) return Promise.reject(refusal({ code: 'E_NETWORK' })); f.set('factors', BOTH); }
      return Promise.resolve(null);
    });
    expect(await screen.findByRole('alert')).toHaveTextContent(en['factorSettings.error.load'].replace('{code}', 'E_NETWORK'));
    await user.click(button(en['factorSettings.retry']));
    expect(await screen.findByRole('heading', { level: 3, name: en['factorSettings.password'] })).toBeInTheDocument();
    expect(fake.callsOf('refreshFactors')).toHaveLength(2);
  });
  // I1, I2: every factor is a settings row, its action in the control column.
  it('draws the password, app, each passkey, the add-passkey action and the codes as settings rows', async () => {
    const { view } = setup();
    await screen.findByRole('heading', { level: 3, name: en['factorSettings.password'] });
    expect(view.container.querySelectorAll('.d-settings-row')).toHaveLength(6);
    const laptop = button('Remove laptop').closest('.d-settings-row') as HTMLElement;
    expect(laptop.querySelector('.d-settings-row__label')).toHaveTextContent(en['factorSettings.passkey']);
    expect(laptop).toHaveTextContent('laptop');
    expect(laptop.querySelector('.d-settings-row__control')).toContainElement(button('Remove laptop'));
  });
  it('has no axe violations', async () => {
    const { view } = setup();
    await screen.findByRole('heading', { level: 3, name: en['factorSettings.password'] });
    await expectNoAxeViolations(view.container);
  });
});

describe('the step-up dialog', () => {
  it('asks for the password, sends it once and clears the field', async () => {
    const { fake, user } = setup(BOTH, (c, f) => c.m === 'stepUp' ? Promise.reject(refusal({ code: 'E_STEP_UP', status: 403 })) : happy(c, f));
    await user.click(await screen.findByRole('button', { name: 'Remove laptop' }));
    expect(within(stepUp()).getByText(en['factorSettings.stepUp.body'])).toBeInTheDocument();
    expect(within(stepUp()).getByLabelText(en['factorSettings.stepUp.password'])).toHaveAttribute('type', 'password');
    await confirmPassword(user);
    expect(fake.callsOf('stepUp')).toEqual([{ m: 'stepUp', method: 'password', secret: 'hunter2' }]);
    expect(await within(stepUp()).findByRole('alert')).toHaveTextContent(en['factorSettings.stepUp.failed']);
    expect(within(stepUp()).getByLabelText(en['factorSettings.stepUp.password'])).toHaveValue('');
  });
  it('shows the rate limit with its seconds', async () => {
    const { user } = setup(BOTH, (c, f) => c.m === 'stepUp' ? Promise.reject(refusal({ code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 42_000 })) : happy(c, f));
    await user.click(await screen.findByRole('button', { name: 'Remove laptop' }));
    await confirmPassword(user);
    expect(await within(stepUp()).findByRole('alert')).toHaveTextContent(en['factor.error.tooMany'].replace('{seconds}', '42'));
  });
  it('closes on Escape and leaves Settings open, with focus back on the opener', async () => {
    const { user } = setup();
    const opener = await screen.findByRole('button', { name: 'Remove laptop' });
    await user.click(opener);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: en['factorSettings.stepUp.title'] })).toBeNull();
    expect(screen.getByRole('dialog', { name: 'Settings' })).toBeInTheDocument();
    expect(window.location.pathname).toBe('/settings/signin');
    expect(opener).toHaveFocus();
  });
  it('runs the passkey ceremony when the account has no password, and goes on with the token', async () => {
    vi.mocked(getPasskey).mockResolvedValueOnce('{"assertion":1}');
    const { fake, user } = setup({ ...BOTH, hasPassword: false }, (c, f) => c.m === 'stepUpPasskeyFinish' ? Promise.resolve({ token: 'tok-pk' }) : happy(c, f));
    await user.click(await screen.findByRole('button', { name: en['factorSettings.codes.make'] }));
    await user.click(within(await screen.findByRole('dialog', { name: en['factorSettings.codes.makeTitle'] })).getByRole('button', { name: en['factorSettings.codes.makeConfirm'] }));
    await user.click(within(await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] })).getByRole('button', { name: en['factorSettings.stepUp.usePasskey'] }));
    await screen.findByRole('dialog', { name: en['factorSettings.codes.newTitle'] });
    expect(fake.callsOf('stepUpPasskeyFinish')).toEqual([{ m: 'stepUpPasskeyFinish', ceremonyId: '11'.repeat(16), response: '{"assertion":1}' }]);
    expect(fake.callsOf('regenerateBackupCodes')).toEqual([{ m: 'regenerateBackupCodes', stepup: 'tok-pk' }]);
  });
  it('keeps the passkey dialog open with a line when the ceremony is cancelled', async () => {
    vi.mocked(getPasskey).mockRejectedValueOnce(new PasskeyError('cancelled', 'no'));
    const { user } = setup({ ...BOTH, hasPassword: false });
    await user.click(await screen.findByRole('button', { name: 'Remove laptop' }));
    await user.click(within(stepUp()).getByRole('button', { name: en['factorSettings.stepUp.usePasskey'] }));
    expect(await within(stepUp()).findByRole('alert')).toHaveTextContent(en['factorSettings.stepUp.passkeyFailed']);
  });
  it('offers a passkey, not a password, when the account has no password', async () => {
    const { fake, user } = setup({ ...BOTH, hasPassword: false });
    await user.click(await screen.findByRole('button', { name: 'Remove laptop' }));
    expect(within(stepUp()).queryByLabelText(en['factorSettings.stepUp.password'])).toBeNull();
    expect(within(stepUp()).getByText(en['factorSettings.stepUp.passkeyBody'])).toBeInTheDocument();
    expect(within(stepUp()).getByRole('button', { name: en['factorSettings.stepUp.usePasskey'] })).toBeInTheDocument();
    expect(fake.callsOf('stepUp')).toEqual([]);
  });
});

describe('removing a factor', () => {
  it('steps up, removes the passkey with the token, announces it once and focuses the add-passkey button', async () => {
    const { fake, user } = setup();
    const watch = watchLive();
    await user.click(await screen.findByRole('button', { name: 'Remove laptop' }));
    await confirmPassword(user);
    const confirm = await screen.findByRole('dialog', { name: removeTitle('laptop') });
    expect(within(confirm).getByText(en['factorSettings.removeBody'])).toBeInTheDocument();
    await user.click(within(confirm).getByRole('button', { name: en['factorSettings.removeConfirm'] }));
    await waitFor(() => expect(fake.callsOf('removeFactor')).toEqual([{ m: 'removeFactor', stepup: 'tok-1', kind: 'passkey', credId: PK }]));
    await waitFor(() => expect(watch.history).toEqual([en['factorSettings.removed.passkey'].replace('{name}', 'laptop')]));
    await waitFor(() => expect(button(en['factorSettings.passkeys.add'])).toHaveFocus());
    expect(screen.queryByText('laptop')).toBeNull();
    watch.stop();
  });
  it('removes the app and focuses its add button', async () => {
    const { fake, user } = setup();
    await user.click(await screen.findByRole('button', { name: 'Remove the authenticator app' }));
    await confirmPassword(user);
    const confirm = await screen.findByRole('dialog', { name: en['factorSettings.removeTitle.totp'] });
    await user.click(within(confirm).getByRole('button', { name: en['factorSettings.removeConfirm'] }));
    await waitFor(() => expect(fake.callsOf('removeFactor')).toEqual([{ m: 'removeFactor', stepup: 'tok-1', kind: 'totp', credId: null }]));
    await waitFor(() => expect(button(en['factorSettings.totp.add'])).toHaveFocus());
  });
  it('cancel leaves the factor and returns focus to the row button', async () => {
    const { fake, user } = setup();
    await user.click(await screen.findByRole('button', { name: 'Remove laptop' }));
    await confirmPassword(user);
    const confirm = await screen.findByRole('dialog', { name: removeTitle('laptop') });
    await user.click(within(confirm).getByRole('button', { name: en['factorSettings.cancel'] }));
    expect(fake.callsOf('removeFactor')).toEqual([]);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Remove laptop' })).toHaveFocus());
  });
  it('asks to confirm again when the token expired, saying so', async () => {
    let calls = 0;
    const { fake, user } = setup(BOTH, (c, f) => {
      if (c.m === 'removeFactor') { calls += 1; if (calls === 1) return Promise.reject(refusal({ code: 'E_STEP_UP', status: 403 })); }
      return happy(c, f);
    });
    await user.click(await screen.findByRole('button', { name: 'Remove laptop' }));
    await confirmPassword(user);
    await user.click(within(await screen.findByRole('dialog', { name: removeTitle('laptop') })).getByRole('button', { name: en['factorSettings.removeConfirm'] }));
    const again = await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] });
    expect(await within(again).findByText(en['factorSettings.stepUp.expired'])).toBeInTheDocument();
    await confirmPassword(user, 'hunter3');
    await user.click(within(await screen.findByRole('dialog', { name: removeTitle('laptop') })).getByRole('button', { name: en['factorSettings.removeConfirm'] }));
    await waitFor(() => expect(fake.callsOf('removeFactor')).toHaveLength(2));
    expect(fake.callsOf('stepUp').map(c => c.secret)).toEqual(['hunter2', 'hunter3']);
  });
  const refusals: [string, Parameters<typeof refusal>[0], string][] = [
    ['409', { code: 'E_INVALID_REQUEST', status: 409, detail: 'anything' }, en['factorSettings.error.only']],
    ['404', { code: 'E_NOT_FOUND', status: 404 }, en['factorSettings.error.changed']],
    ['403 E_FORBIDDEN', { code: 'E_FORBIDDEN', status: 403 }, en['factorSettings.error.changed']],
    ['other', { code: 'E_BOOM', status: 500 }, en['factorSettings.error.other'].replace('{code}', 'E_BOOM')],
  ];
  for (const [label, init, text] of refusals) {
    it(`maps ${label} by status and code`, async () => {
      const { fake, user } = setup(BOTH, (c, f) => c.m === 'removeFactor' ? Promise.reject(refusal(init)) : happy(c, f));
      await user.click(await screen.findByRole('button', { name: 'Remove laptop' }));
      await confirmPassword(user);
      const confirm = await screen.findByRole('dialog', { name: removeTitle('laptop') });
      await user.click(within(confirm).getByRole('button', { name: en['factorSettings.removeConfirm'] }));
      expect(await within(confirm).findByRole('alert')).toHaveTextContent(text);
      // Contract 1: the refusal and the end of busy are one update, so focus is on Cancel, never on the destructive button.
      await waitFor(() => expect(within(confirm).getByRole('button', { name: en['factorSettings.cancel'] })).toHaveFocus());
      // A change made elsewhere (404, E_FORBIDDEN) re-reads the factors.
      // The instance spent the token before it decided, so 409 re-reads as well.
      if (init.status !== 500) await waitFor(() => expect(fake.callsOf('refreshFactors')).toHaveLength(2));
      else expect(fake.callsOf('refreshFactors')).toHaveLength(1);
    });
  }
});

describe('new backup codes', () => {
  it('makes them after the confirm and a step-up, shows the list, announces once and returns focus to the make button', async () => {
    const { fake, user } = setup();
    const watch = watchLive();
    await user.click(await screen.findByRole('button', { name: en['factorSettings.codes.make'] }));
    const confirm = await screen.findByRole('dialog', { name: en['factorSettings.codes.makeTitle'] });
    expect(within(confirm).getByText(en['factorSettings.codes.makeBody'])).toBeInTheDocument();
    await user.click(within(confirm).getByRole('button', { name: en['factorSettings.codes.makeConfirm'] }));
    await confirmPassword(user);
    const shown = await screen.findByRole('dialog', { name: en['factorSettings.codes.newTitle'] });
    expect(fake.callsOf('regenerateBackupCodes')).toEqual([{ m: 'regenerateBackupCodes', stepup: 'tok-1' }]);
    expect(within(shown).getByText(en['factorSettings.codes.newBody'])).toBeInTheDocument();
    expect(regionHome()).toBe(shown);
    expect(within(shown).getAllByRole('listitem').map(li => li.textContent?.replace(/[-\s]/g, ''))).toEqual(CODES);
    await waitFor(() => expect(watch.history).toEqual([en['factorSettings.codesMade']]));
    await user.click(within(shown).getByRole('button', { name: en['dialog.close'] }));
    await waitFor(() => expect(button(en['factorSettings.codes.make'])).toHaveFocus());
    // The codes are gone from the page once the list is closed.
    expect(screen.queryByText(/AAAAA/)).toBeNull();
    watch.stop();
  });
  it('asks again when the token expired, then makes them without asking "are you sure" twice', async () => {
    let calls = 0;
    const { fake, user } = setup(BOTH, (c, f) => {
      if (c.m === 'regenerateBackupCodes') { calls += 1; if (calls === 1) return Promise.reject(refusal({ code: 'E_STEP_UP', status: 403 })); }
      return happy(c, f);
    });
    await user.click(await screen.findByRole('button', { name: en['factorSettings.codes.make'] }));
    await user.click(within(await screen.findByRole('dialog', { name: en['factorSettings.codes.makeTitle'] })).getByRole('button', { name: en['factorSettings.codes.makeConfirm'] }));
    await confirmPassword(user);
    expect(await within(await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] })).findByText(en['factorSettings.stepUp.expired'])).toBeInTheDocument();
    await confirmPassword(user, 'hunter3');
    expect(await screen.findByRole('dialog', { name: en['factorSettings.codes.newTitle'] })).toBeInTheDocument();
    expect(fake.callsOf('regenerateBackupCodes')).toHaveLength(2);
  });
});

describe('adding a factor', () => {
  it('adds the app with the token as stepup, shows the codes, and closes back to Settings', async () => {
    const { fake, user } = setup({ ...ONLY_PASSKEY });
    await user.click(await screen.findByRole('button', { name: en['factorSettings.totp.add'] }));
    await confirmPassword(user);
    await screen.findByRole('img', { name: /QR code/ });
    expect(fake.callsOf('factorTotpBegin')).toEqual([{ m: 'factorTotpBegin', stepup: 'tok-1' }]);
    await user.type(screen.getByRole('textbox', { name: en['factor.totp.code'] }), '123456{Enter}');
    const codes = await screen.findByRole('dialog', { name: en['factor.codes.title'] });
    expect(within(codes).getAllByRole('listitem')).toHaveLength(2);
    await user.click(within(codes).getByRole('checkbox', { name: en['factor.codes.ack'] }));
    await user.click(within(codes).getByRole('button', { name: en['factor.codes.next'] }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: en['factor.codes.title'] })).toBeNull());
    expect(screen.getByRole('dialog', { name: 'Settings' })).toBeInTheDocument();
    expect(fake.callsOf('factorTotpConfirm')).toEqual([{ m: 'factorTotpConfirm', code: '123456' }]);
  });
  it('closes the add dialog on Escape and leaves Settings open, focus on the opener', async () => {
    const { user } = setup({ ...ONLY_PASSKEY });
    await user.click(await screen.findByRole('button', { name: en['factorSettings.totp.add'] }));
    await confirmPassword(user);
    await screen.findByRole('img', { name: /QR code/ });
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('img', { name: /QR code/ })).toBeNull();
    expect(screen.getByRole('dialog', { name: 'Settings' })).toBeInTheDocument();
    await waitFor(() => expect(button(en['factorSettings.totp.add'])).toHaveFocus());
  });
  it('asks again when the begin is refused for a spent token', async () => {
    const { user } = setup({ ...ONLY_PASSKEY }, (c, f) => c.m === 'factorTotpBegin' ? Promise.reject(refusal({ code: 'E_STEP_UP', status: 403 })) : happy(c, f));
    await user.click(await screen.findByRole('button', { name: en['factorSettings.totp.add'] }));
    await confirmPassword(user);
    expect(await within(await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] })).findByText(en['factorSettings.stepUp.expired'])).toBeInTheDocument();
  });
  it('refuses a passkey name over 64 bytes before the begin (final review M3)', async () => {
    const { fake, user } = setup({ ...ONLY_TOTP });
    await user.click(await screen.findByRole('button', { name: en['factorSettings.passkeys.add'] }));
    await confirmPassword(user);
    const dialog = await screen.findByRole('dialog', { name: en['factor.passkey.title'] });
    const name = within(dialog).getByRole('textbox', { name: en['factor.passkey.name'] });
    await user.clear(name);
    await user.type(name, '山田'.repeat(11));
    await user.click(within(dialog).getByRole('button', { name: en['factor.passkey.submit'] }));
    expect(await within(dialog).findByText(en['factor.error.nameLong'])).toBeInTheDocument();
    expect(fake.callsOf('factorPasskeyBegin')).toEqual([]);
  });
  it('opens the passkey step after the step-up, without a choice or a Back', async () => {
    const { fake, user } = setup({ ...ONLY_TOTP });
    await user.click(await screen.findByRole('button', { name: en['factorSettings.passkeys.add'] }));
    await confirmPassword(user);
    const dialog = await screen.findByRole('dialog', { name: en['factor.passkey.title'] });
    expect(within(dialog).getByRole('textbox', { name: en['factor.passkey.name'] })).toHaveValue('Passkey 1');
    expect(within(dialog).queryByRole('button', { name: en['factor.back'] })).toBeNull();
    expect(fake.callsOf('factorPasskeyBegin')).toEqual([]);
  });
});

describe('review fixes', () => {
  it('drops a token that arrives after Escape: nothing opens, and the next action asks for the password again', async () => {
    let release: (v: unknown) => void = () => undefined;
    let first = true;
    const { fake, user } = setup(BOTH, (c, f) => {
      if (c.m === 'stepUp' && first) { first = false; return new Promise(r => { release = r; }); }
      return happy(c, f);
    });
    await user.click(await screen.findByRole('button', { name: 'Remove the authenticator app' }));
    await confirmPassword(user);
    await user.keyboard('{Escape}');
    // Another action opens its own step-up while the first answer is still on its way.
    await user.click(screen.getByRole('button', { name: 'Remove laptop' }));
    release({ token: 'late' });
    await new Promise(r => setTimeout(r, 20));
    expect(stepUp()).toBeInTheDocument();
    expect(screen.queryByRole('dialog', { name: removeTitle('laptop') })).toBeNull();
    expect(screen.queryByRole('dialog', { name: en['factorSettings.removeTitle.totp'] })).toBeNull();
    await confirmPassword(user, 'second');
    await user.click(within(await screen.findByRole('dialog', { name: removeTitle('laptop') })).getByRole('button', { name: en['factorSettings.removeConfirm'] }));
    await waitFor(() => expect(fake.callsOf('removeFactor')).toEqual([{ m: 'removeFactor', stepup: 'tok-1', kind: 'passkey', credId: PK }]));
    expect(fake.callsOf('stepUp').map(c => c.secret)).toEqual(['hunter2', 'second']);
  });
  it('after a 409 the token is gone: the second Remove asks for the password without the expired notice', async () => {
    let calls = 0;
    const { fake, user } = setup(BOTH, (c, f) => {
      if (c.m === 'removeFactor') { calls += 1; if (calls === 1) return Promise.reject(refusal({ code: 'E_INVALID_REQUEST', status: 409 })); }
      return happy(c, f);
    });
    await user.click(await screen.findByRole('button', { name: 'Remove laptop' }));
    await confirmPassword(user);
    const confirm = await screen.findByRole('dialog', { name: removeTitle('laptop') });
    await user.click(within(confirm).getByRole('button', { name: en['factorSettings.removeConfirm'] }));
    await within(confirm).findByRole('alert');
    await waitFor(() => expect(fake.callsOf('refreshFactors')).toHaveLength(2));
    await user.click(within(confirm).getByRole('button', { name: en['factorSettings.removeConfirm'] }));
    const again = await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] });
    expect(within(again).queryByText(en['factorSettings.stepUp.expired'])).toBeNull();
    expect(fake.callsOf('removeFactor')).toHaveLength(1);
  });
  it('shows no stale alert in the confirm that follows asking for the password again', async () => {
    let calls = 0;
    const { user } = setup(BOTH, (c, f) => {
      if (c.m === 'removeFactor') { calls += 1; if (calls === 1) return Promise.reject(refusal({ code: 'E_STEP_UP', status: 403 })); }
      return happy(c, f);
    });
    await user.click(await screen.findByRole('button', { name: 'Remove laptop' }));
    await confirmPassword(user);
    await user.click(within(await screen.findByRole('dialog', { name: removeTitle('laptop') })).getByRole('button', { name: en['factorSettings.removeConfirm'] }));
    await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] });
    await confirmPassword(user, 'again');
    expect(within(await screen.findByRole('dialog', { name: removeTitle('laptop') })).queryByRole('alert')).toBeNull();
  });
  const addPasskeyScript = (f: FakeClient, c: Command): Promise<unknown> | undefined => {
    if (c.m === 'factorPasskeyBegin') return Promise.resolve({ ceremonyId: '22'.repeat(16), options: '{}' });
    if (c.m === 'factorPasskeyFinish') { f.set('factors', { ...ONLY_TOTP, passkeys: BOTH.passkeys.slice(0, 1) }); return Promise.resolve({ backupCodes: [] }); }
    return undefined;
  };
  it('focuses the add-passkey button after the first passkey is added', async () => {
    vi.mocked(createPasskey).mockResolvedValueOnce('{"c":1}');
    const { user } = setup({ ...ONLY_TOTP }, (c, f) => addPasskeyScript(f, c) ?? happy(c, f));
    await user.click(await screen.findByRole('button', { name: en['factorSettings.passkeys.add'] }));
    await confirmPassword(user);
    const dialog = await screen.findByRole('dialog', { name: en['factor.passkey.title'] });
    await user.click(within(dialog).getByRole('button', { name: en['factor.passkey.submit'] }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: en['factor.passkey.title'] })).toBeNull());
    await waitFor(() => expect(button(en['factorSettings.passkeys.add'])).toHaveFocus());
  });
  it('announces an add with the passkey name, in the one live region', async () => {
    vi.mocked(createPasskey).mockResolvedValueOnce('{"c":1}');
    const watch = watchLive();
    const { user } = setup({ ...ONLY_TOTP }, (c, f) => addPasskeyScript(f, c) ?? happy(c, f));
    await user.click(await screen.findByRole('button', { name: en['factorSettings.passkeys.add'] }));
    await confirmPassword(user);
    const dialog = await screen.findByRole('dialog', { name: en['factor.passkey.title'] });
    expect(regionHome()).toBe(dialog);
    await user.click(within(dialog).getByRole('button', { name: en['factor.passkey.submit'] }));
    await waitFor(() => expect(watch.history).toEqual([en['factorSettings.added.passkey'].replace('{name}', 'Passkey 1')]));
    watch.stop();
  });
  it('keeps the one live region in the topmost open modal', async () => {
    const { user } = setup();
    await screen.findByRole('heading', { level: 3, name: en['factorSettings.password'] });
    expect(regionHome()).toBeNull();
    await user.click(button('Remove laptop'));
    await waitFor(() => expect(regionHome()).toBe(stepUp()));
    await confirmPassword(user);
    // The dialog first, then the region: the region moves into it once it is shown, not before.
    const made = await screen.findByRole('dialog', { name: removeTitle('laptop') });
    await waitFor(() => expect(regionHome()).toBe(made));
  });
  it('hands a failed app begin back to the password with the nothing-was-added notice', async () => {
    const { user } = setup({ ...ONLY_PASSKEY }, (c, f) => c.m === 'factorTotpBegin' ? Promise.reject(refusal({ code: 'E_NETWORK' })) : happy(c, f));
    await user.click(await screen.findByRole('button', { name: en['factorSettings.totp.add'] }));
    await confirmPassword(user);
    expect(within(await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] })).getByText(en['factorSettings.stepUp.again'])).toBeInTheDocument();
  });
  it('hands a passkey the instance refused back to the password with the nothing-was-added notice', async () => {
    vi.mocked(createPasskey).mockResolvedValueOnce('{"c":1}');
    const { user } = setup({ ...ONLY_TOTP }, (c, f) => c.m === 'factorPasskeyFinish' ? Promise.reject(refusal({ code: 'E_INVALID_REQUEST', status: 400 }))
      : addPasskeyScript(f, c) ?? happy(c, f));
    await user.click(await screen.findByRole('button', { name: en['factorSettings.passkeys.add'] }));
    await confirmPassword(user);
    const dialog = await screen.findByRole('dialog', { name: en['factor.passkey.title'] });
    await user.click(within(dialog).getByRole('button', { name: en['factor.passkey.submit'] }));
    expect(within(await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] })).getByText(en['factorSettings.stepUp.again'])).toBeInTheDocument();
  });
  it('walk I3: keeps the passkeys together, one row each, with the add row after them and before the backup codes', async () => {
    const { view } = setup();
    await screen.findByRole('heading', { level: 3, name: en['factorSettings.password'] });
    const rows = Array.from(view.container.querySelectorAll('.d-settings-row__label')).map(l => l.textContent);
    // Task 11 (Q7): each passkey row is the prototype's "Passkey" over its name.
    expect(rows).toEqual([en['factorSettings.password'], en['factorSettings.totp'], en['factorSettings.passkey'], en['factorSettings.passkey'], en['factorSettings.passkeys'], en['factorSettings.codes']]);
    const add = screen.getByRole('button', { name: en['factorSettings.passkeys.add'] });
    expect(button('Remove phone').compareDocumentPosition(add) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(add.compareDocumentPosition(screen.getByRole('button', { name: en['factorSettings.codes.make'] })) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    await expectNoAxeViolations(view.container);
  });
  it('walk M3: a wrong password puts focus back on the emptied password field', async () => {
    const { user } = setup(BOTH, (c, f) => c.m === 'stepUp' ? Promise.reject(refusal({ code: 'E_STEP_UP', status: 403 })) : happy(c, f));
    await user.click(await screen.findByRole('button', { name: 'Remove laptop' }));
    await confirmPassword(user);
    expect(await within(stepUp()).findByRole('alert')).toHaveTextContent(en['factorSettings.stepUp.failed']);
    const field = within(stepUp()).getByLabelText(en['factorSettings.stepUp.password']);
    await waitFor(() => expect(field).toHaveFocus());
    expect(field).toHaveValue('');
  });
  it('walk M6: make new codes asks "are you sure" first, then the password, then makes them without a second press', async () => {
    const { fake, user } = setup();
    await user.click(await screen.findByRole('button', { name: en['factorSettings.codes.make'] }));
    const confirm = await screen.findByRole('dialog', { name: en['factorSettings.codes.makeTitle'] });
    expect(screen.queryByRole('dialog', { name: en['factorSettings.stepUp.title'] })).toBeNull();
    await user.click(within(confirm).getByRole('button', { name: en['factorSettings.codes.makeConfirm'] }));
    expect(fake.callsOf('regenerateBackupCodes')).toEqual([]);
    await confirmPassword(user);
    expect(await screen.findByRole('dialog', { name: en['factorSettings.codes.newTitle'] })).toBeInTheDocument();
    expect(fake.callsOf('regenerateBackupCodes')).toEqual([{ m: 'regenerateBackupCodes', stepup: 'tok-1' }]);
  });
  it('walk M6: cancelling the password after the confirm makes nothing', async () => {
    const { fake, user } = setup();
    const opener = await screen.findByRole('button', { name: en['factorSettings.codes.make'] });
    await user.click(opener);
    await user.click(within(await screen.findByRole('dialog', { name: en['factorSettings.codes.makeTitle'] })).getByRole('button', { name: en['factorSettings.codes.makeConfirm'] }));
    await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] });
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: en['factorSettings.codes.makeTitle'] })).toBeNull();
    expect(fake.callsOf('regenerateBackupCodes')).toEqual([]);
    await waitFor(() => expect(opener).toHaveFocus());
  });
  it('walk M7: a cancelled passkey prompt shows a banner in the add dialog, focus on Add passkey, and the next try asks for the password', async () => {
    vi.mocked(createPasskey).mockRejectedValueOnce(new PasskeyError('cancelled', 'no'));
    const { fake, user } = setup({ ...ONLY_TOTP }, (c, f) => addPasskeyScript(f, c) ?? happy(c, f));
    await user.click(await screen.findByRole('button', { name: en['factorSettings.passkeys.add'] }));
    await confirmPassword(user);
    const dialog = await screen.findByRole('dialog', { name: en['factor.passkey.title'] });
    await user.click(within(dialog).getByRole('button', { name: en['factor.passkey.submit'] }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(en['factor.error.cancelled']);
    expect(screen.queryByRole('dialog', { name: en['factorSettings.stepUp.title'] })).toBeNull();
    await waitFor(() => expect(within(dialog).getByRole('button', { name: en['factor.passkey.submit'] })).toHaveFocus());
    // The begin spent the token: the next try asks for the password again, saying nothing was added.
    await user.click(within(dialog).getByRole('button', { name: en['factor.passkey.submit'] }));
    expect(within(await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] })).getByText(en['factorSettings.stepUp.again'])).toBeInTheDocument();
    expect(fake.callsOf('factorPasskeyBegin')).toHaveLength(1);
  });
  it('has no axe violations with the step-up, the remove confirm and the add dialog open', async () => {
    const { user, view } = setup({ ...BOTH });
    await user.click(await screen.findByRole('button', { name: en['factorSettings.passkeys.add'] }));
    await screen.findByRole('dialog', { name: en['factorSettings.stepUp.title'] });
    await expectNoAxeViolations(view.container);
    await confirmPassword(user);
    await screen.findByRole('dialog', { name: en['factor.passkey.title'] });
    await expectNoAxeViolations(view.container);
    await user.keyboard('{Escape}');
    await user.click(await screen.findByRole('button', { name: 'Remove laptop' }));
    await confirmPassword(user);
    await screen.findByRole('dialog', { name: removeTitle('laptop') });
    await expectNoAxeViolations(view.container);
  });
});
