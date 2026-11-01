import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { Command, FactorsState } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { browser } from '../../browser.ts';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { account, INSTANCE } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { en } from '../../strings/index.ts';
import { FactorSetup, type FactorSetupProps } from './FactorSetup.tsx';

const CEREMONY = 'ab'.repeat(16);
const SECRET = 'JBSWY3DPEHPK3PXP';
const URI = `otpauth://totp/dilla.test:ada?secret=${SECRET}&issuer=dilla.test`;
const CODES = ['AAAAABBBBBCCCCCDDDDDEEEEEF', 'GGGGGHHHHHIIIIIJJJJJKKKKKL'];
const OPTIONS = JSON.stringify({ publicKey: {
  challenge: 'Zm9vYmFyLWNoYWxsZW5nZS1fLQ', rp: { name: 'dilla.test', id: 'dilla.test' }, user: { id: 'AQID', name: 'ada', displayName: 'ada' },
  pubKeyCredParams: [{ type: 'public-key', alg: -7 }], authenticatorSelection: { userVerification: 'preferred' },
} });

const NO_FACTORS: FactorsState = { owed: true, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true };

function credential() {
  return { id: 'AQID', rawId: new Uint8Array([1, 2, 3]).buffer, type: 'public-key', authenticatorAttachment: 'platform',
    response: { clientDataJSON: new Uint8Array([4]).buffer, attestationObject: new Uint8Array([5]).buffer, getTransports: () => ['internal'] },
    getClientExtensionResults: () => ({}) };
}
const create = vi.fn<(o: { publicKey: { authenticatorSelection: { userVerification: string } } }) => Promise<unknown>>();
function webauthn(on: boolean): void {
  if (on) {
    Object.defineProperty(window, 'PublicKeyCredential', { value: class {}, configurable: true });
    Object.defineProperty(navigator, 'credentials', { value: { create }, configurable: true });
  } else {
    Reflect.deleteProperty(window, 'PublicKeyCredential');
    Reflect.deleteProperty(navigator, 'credentials');
  }
}
Object.defineProperty(window, 'isSecureContext', { value: true, configurable: true });

type Handler = (c: Command, fake: FakeClient) => Promise<unknown>;
const happy: Handler = (c) => {
  switch (c.m) {
    case 'factorTotpBegin': return Promise.resolve({ secret: SECRET, uri: URI });
    case 'factorTotpConfirm': return Promise.resolve({ backupCodes: CODES });
    case 'factorPasskeyBegin': return Promise.resolve({ ceremonyId: CEREMONY, options: OPTIONS });
    case 'factorPasskeyFinish': return Promise.resolve({ backupCodes: CODES });
    default: return Promise.resolve(null);
  }
};

function setup(over: Partial<FactorSetupProps> = {}, opts: { handler?: Handler; factors?: FactorsState | undefined; passkeys?: boolean; webauthn?: boolean } = {}) {
  const fake = new FakeClient();
  fake.set('account', account({ phase: 'factor-setup', instance: { ...INSTANCE, passkeys: opts.passkeys ?? true }, factorSetup: { origin: 'required' } }));
  if (opts.factors !== undefined || !('factors' in opts)) fake.set('factors', opts.factors ?? NO_FACTORS);
  const handler = opts.handler ?? happy;
  fake.handler = (c) => handler(c, fake);
  webauthn(opts.webauthn ?? true);
  const announce = vi.fn();
  const onDone = vi.fn();
  const user = userEvent.setup();
  const props: FactorSetupProps = { origin: 'signup', stepLabel: 'New account · step 5 of 6', instanceName: 'dilla.test', username: 'ada', announce, onDone, ...over };
  const view = render(<div className="d-root"><CoreProvider client={fake}><FactorSetup {...props} /></CoreProvider></div>);
  return { fake, announce, onDone, user, view };
}
afterEach(() => { webauthn(false); create.mockReset(); vi.restoreAllMocks(); });

const h1 = () => screen.getByRole('heading', { level: 1 });
const button = (name: string) => screen.getByRole('button', { name });
const codeField = () => screen.getByRole('textbox', { name: en['factor.totp.code'] });

async function toTotp(user: ReturnType<typeof userEvent.setup>) {
  await user.click(button('Continue'));
  await screen.findByRole('img', { name: /QR code/ });
}
async function toPasskey(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('radio', { name: en['factor.choose.passkey'] }));
  await user.click(button('Continue'));
}

describe('choose', () => {
  it('shows both cards with the app preselected, the step line and the body for a sign-up', () => {
    setup();
    expect(h1()).toHaveTextContent(en['factor.choose.title']);
    expect(h1()).toHaveFocus();
    expect(screen.getByText('New account · step 5 of 6')).toBeInTheDocument();
    expect(screen.getByText(/Every account on dilla.test needs a second factor/)).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: en['factor.choose.app'] })).toBeChecked();
    expect(screen.getByRole('radio', { name: en['factor.choose.passkey'] })).toBeEnabled();
  });
  it('says the account owes a factor when mounted for a required set-up', () => {
    setup({ origin: 'required', stepLabel: '' });
    expect(screen.getByText('Your account needs a second factor before you can continue. dilla.test asks every account for one.')).toBeInTheDocument();
    expect(screen.queryByText(/Every account on dilla.test needs/)).toBeNull();
  });
  it.each(['signup', 'signin', 'required'] as const)('has no skip or cancel control for origin %s', (origin) => {
    setup({ origin });
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual(['Continue']);
    expect(screen.queryByRole('link')).toBeNull();
  });
  it('disables the passkey card with its reason when passkeys cannot be used, and the app stays selectable', async () => {
    const { user } = setup({}, { webauthn: false });
    const card = screen.getByRole('radio', { name: en['factor.choose.passkey'] });
    expect(card).toBeDisabled();
    expect(card).toHaveAccessibleDescription(expect.stringContaining(en['factor.choose.passkeyOff']));
    await toTotp(user);
  });
  it('disables the passkey card when the instance does not offer passkeys', () => {
    setup({}, { passkeys: false });
    expect(screen.getByRole('radio', { name: en['factor.choose.passkey'] })).toBeDisabled();
  });
  it('offers only the passkey, preselected, to an account without a password', () => {
    setup({}, { factors: { ...NO_FACTORS, hasPassword: false } });
    expect(screen.queryByRole('radio', { name: en['factor.choose.app'] })).toBeNull();
    expect(screen.getByRole('radio', { name: en['factor.choose.passkey'] })).toBeChecked();
    expect(screen.getByText(en['factor.choose.noPassword'])).toBeInTheDocument();
  });
  it('reads the factors when the slice is not there yet', async () => {
    const { fake } = setup({}, { factors: undefined });
    await waitFor(() => expect(fake.callsOf('refreshFactors')).toHaveLength(1));
  });
  it('has no axe violations', async () => {
    const { view } = setup();
    await expectNoAxeViolations(view.container);
  });
});

describe('authenticator app', () => {
  it('begins without a step-up, shows the loading line, then the QR code, the setup key and the code field', async () => {
    let release: (v: unknown) => void = () => undefined;
    const { fake, user } = setup({}, { handler: (c) => c.m === 'factorTotpBegin' ? new Promise(r => { release = r; }) : happy(c, fake) });
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent(en['factor.totp.title']);
    expect(screen.getByText(en['factor.totp.loading'])).toBeInTheDocument();
    expect(fake.callsOf('factorTotpBegin')).toEqual([{ m: 'factorTotpBegin', stepup: null }]);
    release({ secret: SECRET, uri: URI });
    expect(await screen.findByRole('img', { name: 'QR code to add ada on dilla.test to an authenticator app' })).toBeInTheDocument();
    const key = screen.getByRole('textbox', { name: en['factor.totp.key'] });
    expect(key).toHaveValue(SECRET);
    expect(key).toHaveAttribute('readonly');
    expect(codeField()).toHaveAttribute('autocomplete', 'one-time-code');
    expect(codeField()).toHaveAttribute('inputmode', 'numeric');
    expect(screen.queryByText(en['factor.totp.loading'])).toBeNull();
  });
  it('selects the whole setup key when its field takes focus', async () => {
    const { user } = setup();
    await toTotp(user);
    const key = screen.getByRole<HTMLInputElement>('textbox', { name: en['factor.totp.key'] });
    key.focus();
    expect(key).toHaveFocus();
    expect(key.selectionStart).toBe(0);
    expect(key.selectionEnd).toBe(SECRET.length);
  });
  it('copies the setup key and announces it', async () => {
    const { user, announce } = setup();
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    await toTotp(user);
    await user.click(button(en['factor.totp.copyKey']));
    await waitFor(() => expect(announce).toHaveBeenCalledWith(en['factor.totp.keyCopied']));
    expect(writeText).toHaveBeenCalledWith(SECRET);
  });
  it('does not announce a copy the browser refused', async () => {
    const { user, announce } = setup();
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: () => Promise.reject(new Error('denied')) }, configurable: true });
    await toTotp(user);
    await user.click(button(en['factor.totp.copyKey']));
    await Promise.resolve();
    expect(announce).not.toHaveBeenCalled();
  });
  it('checks the field before asking the instance: empty, then not six digits', async () => {
    const { fake, user } = setup();
    await toTotp(user);
    await user.click(button('Continue'));
    expect(await screen.findByText(en['factor.error.required'])).toBeInTheDocument();
    expect(codeField()).toHaveFocus();
    expect(codeField()).toHaveAttribute('aria-invalid', 'true');
    await user.type(codeField(), '12345');
    await user.click(button('Continue'));
    expect(await screen.findByText(en['factor.error.codeFormat'])).toBeInTheDocument();
    expect(codeField()).toHaveFocus();
    expect(fake.callsOf('factorTotpConfirm')).toEqual([]);
  });
  it('sends the digits typed with spaces dropped and moves to the backup codes', async () => {
    const { fake, user } = setup();
    await toTotp(user);
    await user.type(codeField(), '123 456');
    await user.click(button('Continue'));
    await screen.findByRole('heading', { level: 1, name: en['factor.codes.title'] });
    expect(fake.callsOf('factorTotpConfirm')).toEqual([{ m: 'factorTotpConfirm', code: '123456' }]);
  });
  it('shows a wrong code on the field, clears it and focuses it', async () => {
    const { user } = setup({}, { handler: (c) => c.m === 'factorTotpConfirm' ? Promise.reject(refusal({ code: 'E_INVALID_REQUEST', status: 400 })) : happy(c, new FakeClient()) });
    await toTotp(user);
    await user.type(codeField(), '123456');
    await user.click(button('Continue'));
    expect(await screen.findByText(en['factor.error.code'])).toBeInTheDocument();
    expect(codeField()).toHaveValue('');
    expect(codeField()).toHaveFocus();
  });
  it('marks Continue busy while the code is checked', async () => {
    let release: (v: unknown) => void = () => undefined;
    const { user } = setup({}, { handler: (c) => c.m === 'factorTotpConfirm' ? new Promise(r => { release = r; }) : happy(c, new FakeClient()) });
    await toTotp(user);
    await user.type(codeField(), '123456');
    await user.click(button('Continue'));
    expect(button(en['factor.totp.working'])).toHaveAttribute('aria-disabled', 'true');
    release({ backupCodes: CODES });
    await screen.findByRole('heading', { level: 1, name: en['factor.codes.title'] });
  });
  const banners: [string, Parameters<typeof refusal>[0], string][] = [
    ['429', { code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 12_400 }, 'Too many attempts. Try again in 13 s.'],
    ['a dropped connection', { code: 'E_NETWORK' }, en['factor.error.network']],
    ['another refusal', { code: 'E_INTERNAL', status: 500 }, 'That did not work (E_INTERNAL). Try again.'],
    ['a 400 with another code', { code: 'E_BAD_INPUT', status: 400 }, 'That did not work (E_BAD_INPUT). Try again.'],
  ];
  for (const [label, init, text] of banners) {
    it(`shows ${label} as a banner, never on the field`, async () => {
      const { user } = setup({}, { handler: (c) => c.m === 'factorTotpConfirm' ? Promise.reject(refusal(init)) : happy(c, new FakeClient()) });
      await toTotp(user);
      await user.type(codeField(), '123456');
      await user.click(button('Continue'));
      expect(await screen.findByRole('alert')).toHaveTextContent(text);
      expect(codeField()).not.toHaveAttribute('aria-invalid');
    });
  }
  it('maps the 400 by status and code, not by detail', async () => {
    const { user } = setup({}, { handler: (c) => c.m === 'factorTotpConfirm'
      ? Promise.reject(refusal({ code: 'E_INTERNAL', status: 500, detail: 'invalid code' })) : happy(c, new FakeClient()) });
    await toTotp(user);
    await user.type(codeField(), '123456');
    await user.click(button('Continue'));
    expect(await screen.findByRole('alert')).toHaveTextContent('That did not work (E_INTERNAL). Try again.');
  });
  it('shows a refused begin as a banner and leaves Back', async () => {
    const { user } = setup({}, { handler: (c) => c.m === 'factorTotpBegin' ? Promise.reject(refusal({ code: 'E_NETWORK' })) : happy(c, new FakeClient()) });
    await user.click(button('Continue'));
    expect(await screen.findByRole('alert')).toHaveTextContent(en['factor.error.network']);
    await user.click(button('Back'));
    expect(h1()).toHaveTextContent(en['factor.choose.title']);
  });
  it('returns to the choice with Back and drops the secret', async () => {
    const { user } = setup();
    await toTotp(user);
    await user.click(button('Back'));
    expect(h1()).toHaveTextContent(en['factor.choose.title']);
    expect(document.body.textContent).not.toContain(SECRET);
    expect(screen.queryByRole('img')).toBeNull();
  });
  it('keeps the secret out of the URL, the storage and the console', async () => {
    const consoles = (['log', 'info', 'warn', 'error', 'debug'] as const).map(m => vi.spyOn(console, m));
    const setItem = vi.spyOn(Storage.prototype, 'setItem');
    const { user } = setup();
    await toTotp(user);
    expect(window.location.href).not.toContain(SECRET);
    expect(setItem).not.toHaveBeenCalled();
    expect(consoles.map(c => c.mock.calls.length)).toEqual([0, 0, 0, 0, 0]);
    await user.type(codeField(), '123456');
    await user.click(button('Continue'));
    await screen.findByRole('heading', { level: 1, name: en['factor.codes.title'] });
    expect(document.body.textContent).toContain('AAAAA-BBBBB');
    expect(window.location.href).not.toContain('AAAAA');
    expect(setItem).not.toHaveBeenCalled();
    expect(consoles.map(c => c.mock.calls.length)).toEqual([0, 0, 0, 0, 0]);
  });
  it('has no axe violations on the code step', async () => {
    const { user, view } = setup();
    await toTotp(user);
    await expectNoAxeViolations(view.container);
  });
});

describe('passkey', () => {
  it('names the passkey after the ones held, begins without a step-up and finishes with the document the browser made', async () => {
    create.mockResolvedValue(credential());
    const { fake, user } = setup({}, { factors: { ...NO_FACTORS, passkeys: [{ id: 'aa', name: 'x', created: 1, lastUsed: null }] } });
    await toPasskey(user);
    expect(h1()).toHaveTextContent(en['factor.passkey.title']);
    expect(screen.getByRole('textbox', { name: en['factor.passkey.name'] })).toHaveValue('Passkey 2');
    await user.click(button(en['factor.passkey.submit']));
    await screen.findByRole('heading', { level: 1, name: en['factor.codes.title'] });
    expect(fake.callsOf('factorPasskeyBegin')).toEqual([{ m: 'factorPasskeyBegin', stepup: null }]);
    const finish = fake.callsOf('factorPasskeyFinish');
    expect(finish).toHaveLength(1);
    expect(finish[0]).toMatchObject({ ceremonyId: CEREMONY, name: 'Passkey 2' });
    expect(JSON.parse(finish[0].response)).toMatchObject({ id: 'AQID', type: 'public-key', response: { transports: ['internal'] } });
    expect(create.mock.calls[0][0].publicKey.authenticatorSelection.userVerification).toBe('required');
  });
  it('sends the name that was typed', async () => {
    create.mockResolvedValue(credential());
    const { fake, user } = setup();
    await toPasskey(user);
    const name = screen.getByRole('textbox', { name: en['factor.passkey.name'] });
    await user.clear(name);
    await user.type(name, 'Work laptop');
    await user.click(button(en['factor.passkey.submit']));
    await screen.findByRole('heading', { level: 1, name: en['factor.codes.title'] });
    expect(fake.callsOf('factorPasskeyFinish')[0]).toMatchObject({ name: 'Work laptop' });
  });
  it('refuses an empty name and one over 64 characters without opening the browser', async () => {
    const { fake, user } = setup();
    await toPasskey(user);
    const name = screen.getByRole('textbox', { name: en['factor.passkey.name'] });
    await user.clear(name);
    await user.click(button(en['factor.passkey.submit']));
    expect(await screen.findByText(en['factor.error.required'])).toBeInTheDocument();
    expect(name).toHaveFocus();
    await user.type(name, 'x'.repeat(65));
    await user.click(button(en['factor.passkey.submit']));
    expect(await screen.findByText(en['factor.error.nameLong'])).toBeInTheDocument();
    expect(create).not.toHaveBeenCalled();
    expect(fake.callsOf('factorPasskeyBegin')).toEqual([]);
  });
  it('refuses a name of wide characters that is over 64 bytes before the browser or the instance is asked (final review M3)', async () => {
    const { fake, user } = setup();
    await toPasskey(user);
    const name = screen.getByRole('textbox', { name: en['factor.passkey.name'] });
    await user.clear(name);
    // 22 characters, 66 bytes.
    await user.type(name, '山田'.repeat(11));
    await user.click(button(en['factor.passkey.submit']));
    expect(await screen.findByText(en['factor.error.nameLong'])).toBeInTheDocument();
    expect(name).toHaveFocus();
    expect(create).not.toHaveBeenCalled();
    expect(fake.callsOf('factorPasskeyBegin')).toEqual([]);
  });
  it('does not call a worker-local refusal a dropped connection (final review M3)', async () => {
    create.mockResolvedValue(credential());
    const { user } = setup({}, { handler: (c) => c.m === 'factorPasskeyFinish' ? Promise.reject(refusal({ code: 'E_BAD_INPUT' })) : happy(c, new FakeClient()) });
    await toPasskey(user);
    await user.click(button(en['factor.passkey.submit']));
    expect(await screen.findByRole('alert')).not.toHaveTextContent(en['factor.error.network']);
  });
  it('shows Add passkey as busy while the browser asks', async () => {
    let finish: (v: unknown) => void = () => undefined;
    create.mockReturnValue(new Promise(r => { finish = r; }));
    const { user } = setup();
    await toPasskey(user);
    await user.click(button(en['factor.passkey.submit']));
    expect(await screen.findByRole('button', { name: en['factor.passkey.working'] })).toHaveAttribute('aria-disabled', 'true');
    finish(credential());
    await screen.findByRole('heading', { level: 1, name: en['factor.codes.title'] });
  });
  const ceremonies: [string, DOMException, string][] = [
    ['cancelled', new DOMException('x', 'NotAllowedError'), en['factor.error.cancelled']],
    ['aborted', new DOMException('x', 'AbortError'), en['factor.error.cancelled']],
    ['at the wrong address', new DOMException('x', 'SecurityError'), 'Passkeys do not work at this address of dilla.test. Use an authenticator app, and tell the operator.'],
    ['already held', new DOMException('x', 'InvalidStateError'), en['factor.error.exists']],
    ['failed otherwise', new DOMException('x', 'NotSupportedError'), 'That did not work (E_PASSKEY). Try again.'],
  ];
  for (const [label, error, text] of ceremonies) {
    it(`maps a ceremony ${label} to its banner and returns focus to Add passkey`, async () => {
      create.mockRejectedValue(error);
      const { fake, user } = setup();
      await toPasskey(user);
      await user.click(button(en['factor.passkey.submit']));
      expect(await screen.findByRole('alert')).toHaveTextContent(text);
      expect(button(en['factor.passkey.submit'])).toHaveFocus();
      expect(fake.callsOf('factorPasskeyFinish')).toEqual([]);
      // The app path stays available.
      await user.click(button('Back'));
      expect(screen.getByRole('radio', { name: en['factor.choose.app'] })).toBeEnabled();
    });
  }
  it('maps a refused begin to the refused banner and focuses Add passkey', async () => {
    const { user } = setup({}, { handler: (c) => c.m === 'factorPasskeyBegin' ? Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 })) : happy(c, new FakeClient()) });
    await toPasskey(user);
    await user.click(button(en['factor.passkey.submit']));
    expect(await screen.findByRole('alert')).toHaveTextContent('dilla.test did not accept the passkey. Try again, or use an authenticator app.');
    expect(button(en['factor.passkey.submit'])).toHaveFocus();
    expect(create).not.toHaveBeenCalled();
  });
  it('maps a refused finish to the refused banner', async () => {
    create.mockResolvedValue(credential());
    const { user } = setup({}, { handler: (c) => c.m === 'factorPasskeyFinish' ? Promise.reject(refusal({ code: 'E_INVALID_REQUEST', status: 400 })) : happy(c, new FakeClient()) });
    await toPasskey(user);
    await user.click(button(en['factor.passkey.submit']));
    expect(await screen.findByRole('alert')).toHaveTextContent('dilla.test did not accept the passkey.');
    expect(button(en['factor.passkey.submit'])).toHaveFocus();
  });
  it('maps 429 and a dropped connection before the passkey rows', async () => {
    const { user } = setup({}, { handler: (c) => c.m === 'factorPasskeyBegin' ? Promise.reject(refusal({ code: 'E_RATE_LIMITED', status: 429, retryAfterMs: 5000 })) : happy(c, new FakeClient()) });
    await toPasskey(user);
    await user.click(button(en['factor.passkey.submit']));
    expect(await screen.findByRole('alert')).toHaveTextContent('Too many attempts. Try again in 5 s.');
  });
  it('maps a dropped connection on finish to the network banner', async () => {
    create.mockResolvedValue(credential());
    const { user } = setup({}, { handler: (c) => c.m === 'factorPasskeyFinish' ? Promise.reject(refusal({ code: 'E_NETWORK' })) : happy(c, new FakeClient()) });
    await toPasskey(user);
    await user.click(button(en['factor.passkey.submit']));
    expect(await screen.findByRole('alert')).toHaveTextContent(en['factor.error.network']);
  });
  it('has no axe violations on the naming step', async () => {
    const { user, view } = setup();
    await toPasskey(user);
    await expectNoAxeViolations(view.container);
  });
});

describe('backup codes', () => {
  async function toCodes(user: ReturnType<typeof userEvent.setup>) {
    await toTotp(user);
    await user.type(codeField(), '123456');
    await user.click(button('Continue'));
    await screen.findByRole('heading', { level: 1, name: en['factor.codes.title'] });
  }
  it('lists the codes in an ordered list, says they are not the recovery key and gates Continue on the tick', async () => {
    const { user, onDone } = setup();
    await toCodes(user);
    const list = screen.getByRole('list', { name: en['factor.codes.label'] });
    expect(list.tagName).toBe('OL');
    expect(within(list).getAllByRole('listitem').map(li => li.textContent)).toEqual(['AAAAA-BBBBB-CCCCC-DDDDD-EEEEE-F', 'GGGGG-HHHHH-IIIII-JJJJJ-KKKKK-L']);
    expect(screen.getByText(en['factor.codes.notKey'])).toBeInTheDocument();
    const next = button('Continue');
    expect(next).toHaveAttribute('aria-disabled', 'true');
    expect(next).toHaveAccessibleDescription(en['factor.codes.ackHint']);
    await user.click(next);
    expect(onDone).not.toHaveBeenCalled();
    await user.click(screen.getByRole('checkbox', { name: en['factor.codes.ack'] }));
    expect(button('Continue')).not.toHaveAttribute('aria-disabled');
    await user.click(button('Continue'));
    expect(onDone).toHaveBeenCalledWith({ kind: 'totp' });
  });
  it('copies the codes as plain lines and announces it', async () => {
    const { user, announce } = setup();
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    await toCodes(user);
    await user.click(button(en['factor.codes.copy']));
    await waitFor(() => expect(announce).toHaveBeenCalledWith(en['factor.codes.copied']));
    expect(writeText).toHaveBeenCalledWith(CODES.join('\n'));
  });
  it('prints through the browser object', async () => {
    const print = vi.spyOn(browser, 'print').mockImplementation(() => undefined);
    const { user } = setup();
    await toCodes(user);
    await user.click(button(en['factor.codes.print']));
    expect(print).toHaveBeenCalledTimes(1);
  });
  it('reports a passkey set-up as kind passkey', async () => {
    create.mockResolvedValue(credential());
    const { user, onDone } = setup();
    await toPasskey(user);
    await user.click(button(en['factor.passkey.submit']));
    await screen.findByRole('heading', { level: 1, name: en['factor.codes.title'] });
    await user.click(screen.getByRole('checkbox', { name: en['factor.codes.ack'] }));
    await user.click(button('Continue'));
    expect(onDone).toHaveBeenCalledWith({ kind: 'passkey', name: 'Passkey 1' });
  });
  it('skips the step when the account already holds codes, and ends at once for a sign-up', async () => {
    const { user, onDone } = setup({}, { handler: (c) => c.m === 'factorTotpConfirm' ? Promise.resolve({ backupCodes: [] }) : happy(c, new FakeClient()) });
    await toTotp(user);
    await user.type(codeField(), '123456');
    await user.click(button('Continue'));
    await waitFor(() => expect(onDone).toHaveBeenCalledWith({ kind: 'totp' }));
    expect(screen.queryByRole('heading', { name: en['factor.codes.title'] })).toBeNull();
  });
  it('has no axe violations', async () => {
    const { user, view } = setup();
    await toCodes(user);
    await expectNoAxeViolations(view.container);
  });
});

describe('forced set-up ends with a done step', () => {
  it('announces the factor, says what changed for an app and opens dilla on the button', async () => {
    const { user, onDone, announce } = setup({ origin: 'required', stepLabel: '' });
    await toTotp(user);
    await user.type(codeField(), '123456');
    await user.click(button('Continue'));
    await screen.findByRole('heading', { level: 1, name: en['factor.codes.title'] });
    await user.click(screen.getByRole('checkbox', { name: en['factor.codes.ack'] }));
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent(en['factor.done.title']);
    expect(screen.getByText(en['factor.done.totp'])).toBeInTheDocument();
    expect(announce).toHaveBeenCalledWith(en['factor.added']);
    expect(onDone).not.toHaveBeenCalled();
    await user.click(button(en['factor.done.next']));
    expect(onDone).toHaveBeenCalledWith({ kind: 'totp' });
  });
  it('says the passkey line for a passkey, and goes straight to done when there are no codes to show', async () => {
    create.mockResolvedValue(credential());
    const { user, announce } = setup({ origin: 'required', stepLabel: '' }, { handler: (c) => c.m === 'factorPasskeyFinish' ? Promise.resolve({ backupCodes: [] }) : happy(c, new FakeClient()) });
    await toPasskey(user);
    await user.click(button(en['factor.passkey.submit']));
    expect(await screen.findByText(en['factor.done.passkey'])).toBeInTheDocument();
    expect(announce).toHaveBeenCalledWith(en['factor.added']);
  });
  it('does not announce a done step for a sign-up', async () => {
    const { user, announce } = setup({ origin: 'signup' });
    await toTotp(user);
    await user.type(codeField(), '123456');
    await user.click(button('Continue'));
    await screen.findByRole('heading', { level: 1, name: en['factor.codes.title'] });
    expect(announce).not.toHaveBeenCalledWith(en['factor.added']);
  });
  it('has no axe violations on the done step', async () => {
    create.mockResolvedValue(credential());
    const { user, view } = setup({ origin: 'required', stepLabel: '' }, { handler: (c) => c.m === 'factorPasskeyFinish' ? Promise.resolve({ backupCodes: [] }) : happy(c, new FakeClient()) });
    await toPasskey(user);
    await user.click(button(en['factor.passkey.submit']));
    await screen.findByText(en['factor.done.passkey']);
    await expectNoAxeViolations(view.container);
  });
});

describe('what the page sends', () => {
  it('never sends a step-up from the factor-setup phase', async () => {
    create.mockResolvedValue(credential());
    const { fake, user } = setup();
    await toTotp(user);
    await user.click(button('Back'));
    await toPasskey(user);
    await user.click(button(en['factor.passkey.submit']));
    await screen.findByRole('heading', { level: 1, name: en['factor.codes.title'] });
    const begins = fake.calls.filter(c => c.m === 'factorTotpBegin' || c.m === 'factorPasskeyBegin');
    expect(begins).toHaveLength(2);
    for (const c of begins) expect(c).toMatchObject({ stepup: null });
    expect(fake.calls.some(c => c.m === 'stepUp' || c.m === 'removeFactor' || c.m === 'regenerateBackupCodes')).toBe(false);
  });
  it('does not call onDone after it has been unmounted mid-request', async () => {
    let release: (v: unknown) => void = () => undefined;
    const { user, onDone, view } = setup({}, { handler: (c) => c.m === 'factorTotpConfirm' ? new Promise(r => { release = r; }) : happy(c, new FakeClient()) });
    await toTotp(user);
    await user.type(codeField(), '123456');
    await user.click(button('Continue'));
    view.unmount();
    release({ backupCodes: [] });
    await Promise.resolve();
    expect(onDone).not.toHaveBeenCalled();
  });
});

describe('focus stays on each step\'s heading', () => {
  it('keeps the heading focused once the QR code and the setup key have rendered', async () => {
    const { user } = setup();
    await toTotp(user);
    expect(h1()).toHaveTextContent(en['factor.totp.title']);
    expect(h1()).toHaveFocus();
    expect(codeField()).not.toHaveFocus();
  });
  it('focuses the heading on every step change, not only on first mount', async () => {
    create.mockResolvedValue(credential());
    const { user } = setup({ origin: 'required', stepLabel: '' });
    expect(h1()).toHaveFocus();
    await toTotp(user);
    expect(h1()).toHaveFocus();
    await user.click(button('Back'));
    expect(h1()).toHaveTextContent(en['factor.choose.title']);
    expect(h1()).toHaveFocus();
    await toPasskey(user);
    expect(h1()).toHaveTextContent(en['factor.passkey.title']);
    expect(h1()).toHaveFocus();
    await user.click(button(en['factor.passkey.submit']));
    await screen.findByRole('heading', { level: 1, name: en['factor.codes.title'] });
    expect(h1()).toHaveFocus();
    await user.click(screen.getByRole('checkbox', { name: en['factor.codes.ack'] }));
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent(en['factor.done.title']);
    expect(h1()).toHaveFocus();
  });
});

describe('a reload after the factor was confirmed', () => {
  const HELD: FactorsState = { owed: false, totp: { confirmedAt: 5 }, passkeys: [], backupCodesLeft: 8, hasPassword: true };
  it('ends a sign-up set-up at once, without offering another factor', async () => {
    const { onDone, fake } = setup({}, { factors: HELD });
    await waitFor(() => expect(onDone).toHaveBeenCalledWith({ kind: 'totp' }));
    expect(onDone).toHaveBeenCalledTimes(1);
    expect(fake.calls).toEqual([]);
  });
  it('goes to the done step for the forced screen, and says passkey for a passkey', async () => {
    const { announce, onDone } = setup({ origin: 'required', stepLabel: '' },
      { factors: { ...HELD, totp: null, passkeys: [{ id: 'aa', name: 'x', created: 1, lastUsed: null }] } });
    expect(await screen.findByText(en['factor.done.passkey'])).toBeInTheDocument();
    expect(announce).toHaveBeenCalledWith(en['factor.added']);
    expect(onDone).not.toHaveBeenCalled();
    expect(screen.queryByRole('radio')).toBeNull();
  });
  it('never takes a slice that says owed for a factor in place: the choice shows (an operator reset, a stale slice)', async () => {
    const { announce, onDone } = setup({ origin: 'required', stepLabel: '' }, { factors: { ...HELD, owed: true } });
    await Promise.resolve();
    expect(h1()).toHaveTextContent(en['factor.choose.title']);
    expect(screen.getAllByRole('radio').length).toBeGreaterThan(0);
    expect(announce).not.toHaveBeenCalled();
    expect(onDone).not.toHaveBeenCalled();
  });
  it('a done set-up the worker refuses as still owed goes back to the choice, and the next one finishes', async () => {
    const onDone = vi.fn<FactorSetupProps['onDone']>()
      .mockImplementationOnce(() => Promise.reject(refusal({ code: 'E_FACTOR_REQUIRED' })))
      .mockImplementation(() => Promise.resolve());
    const { fake, user } = setup({ origin: 'required', stepLabel: '', onDone }, { factors: HELD });
    expect(await screen.findByText(en['factor.done.totp'])).toBeInTheDocument();
    // The worker reads the factors again before it refuses: the account owes one and holds none.
    fake.publish('factors', { ...NO_FACTORS });
    await user.click(button(en['factor.done.next']));
    await waitFor(() => { expect(h1()).toHaveTextContent(en['factor.choose.title']); });
    expect(onDone).toHaveBeenCalledTimes(1);
    await toTotp(user);
    await user.type(codeField(), '123456');
    await user.click(button('Continue'));
    await user.click(await screen.findByRole('checkbox', { name: en['factor.codes.ack'] }));
    await user.click(button('Continue'));
    await user.click(await screen.findByRole('button', { name: en['factor.done.next'] }));
    expect(onDone).toHaveBeenCalledTimes(2);
  });
  it('acts when the factors arrive after mount, but never for a factor this mount added itself', async () => {
    const { fake, user, onDone } = setup({}, { factors: undefined });
    fake.publish('factors', { ...NO_FACTORS });
    expect(h1()).toHaveTextContent(en['factor.choose.title']);
    await toTotp(user);
    fake.publish('factors', HELD);
    expect(onDone).not.toHaveBeenCalled();
    expect(h1()).toHaveTextContent(en['factor.totp.title']);
    fake.publish('factors', { ...HELD, totp: null });
    expect(onDone).not.toHaveBeenCalled();
  });
});

describe('another device added a factor first (final review M4)', () => {
  const HELD: FactorsState = { owed: false, totp: null, passkeys: [{ id: 'aa', name: 'x', created: 1, lastUsed: null }], backupCodesLeft: 8, hasPassword: true };
  const stale = (fake: FakeClient, c: Command, after: FactorsState): Promise<unknown> => {
    if (c.m === 'factorTotpBegin') return Promise.reject(refusal({ code: 'E_STEP_UP', status: 403 }));
    if (c.m === 'refreshFactors') { fake.publish('factors', after); return Promise.resolve(null); }
    return Promise.resolve(null);
  };
  it('ends a forced set-up as already in place, with no raw code', async () => {
    const { fake, user } = setup({ origin: 'required', stepLabel: '' }, { handler: (c, f) => stale(f, c, HELD) });
    await user.click(button('Continue'));
    expect(await screen.findByText(en['factor.done.passkey'])).toBeInTheDocument();
    expect(screen.queryByText(/E_STEP_UP/)).toBeNull();
    expect(fake.callsOf('refreshFactors').length).toBeGreaterThan(0);
  });
  it('goes back to the choice, with no raw code, when the account still owes', async () => {
    const { user } = setup({ origin: 'required', stepLabel: '' }, { handler: (c, f) => stale(f, c, { ...NO_FACTORS }) });
    await user.click(button('Continue'));
    await waitFor(() => { expect(h1()).toHaveTextContent(en['factor.choose.title']); });
    expect(screen.queryByText(/E_STEP_UP/)).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
  });
});

describe('walk I1: a forced screen left open while another browser added the factor', () => {
  const HELD: FactorsState = { owed: false, totp: null, passkeys: [{ id: 'aa', name: 'x', created: 1, lastUsed: null }], backupCodesLeft: 8, hasPassword: true };
  it('finishes as already in place when the worker re-read the factors before it refused the begin', async () => {
    // The worker's guarded call reads the factors again before it rethrows the 403, so the slice changes first.
    const { fake, user } = setup({ origin: 'required', stepLabel: '' }, { handler: (c, f) => {
      if (c.m === 'factorTotpBegin') { f.publish('factors', HELD); return Promise.reject(refusal({ code: 'E_STEP_UP', status: 403 })); }
      if (c.m === 'refreshFactors') { f.publish('factors', HELD); return Promise.resolve(null); }
      return Promise.resolve(null);
    } });
    await user.click(button('Continue'));
    expect(await screen.findByText(en['factor.done.passkey'])).toBeInTheDocument();
    expect(screen.queryByText(/E_STEP_UP/)).toBeNull();
    expect(fake.callsOf('factorTotpBegin')).toHaveLength(1);
  });
  it('Back returns to the choice even while a request is still out', async () => {
    const { user } = setup({ origin: 'required', stepLabel: '' }, { handler: (c) => c.m === 'factorTotpBegin' ? new Promise(() => undefined) : happy(c, new FakeClient()) });
    await user.click(button('Continue'));
    expect(h1()).toHaveTextContent(en['factor.totp.title']);
    await user.click(button('Back'));
    expect(h1()).toHaveTextContent(en['factor.choose.title']);
  });
  it('Back returns to the choice while a passkey is being added, and a late answer does not move the screen', async () => {
    let finish: (v: unknown) => void = () => undefined;
    create.mockReturnValue(new Promise(r => { finish = r; }));
    const { user } = setup({ origin: 'required', stepLabel: '' });
    await toPasskey(user);
    await user.click(button(en['factor.passkey.submit']));
    await user.click(button('Back'));
    expect(h1()).toHaveTextContent(en['factor.choose.title']);
    finish(credential());
    await Promise.resolve();
    expect(h1()).toHaveTextContent(en['factor.choose.title']);
  });
  it('reads the factors again when the window gets focus on the choice, and ends as already in place', async () => {
    const { fake } = setup({ origin: 'required', stepLabel: '' }, { handler: (c, f) => {
      if (c.m === 'refreshFactors') { f.publish('factors', HELD); return Promise.resolve(null); }
      return Promise.resolve(null);
    } });
    expect(fake.callsOf('refreshFactors')).toHaveLength(0);
    window.dispatchEvent(new Event('focus'));
    expect(await screen.findByText(en['factor.done.passkey'])).toBeInTheDocument();
    expect(fake.callsOf('refreshFactors')).toHaveLength(1);
  });
  it('reads them once when the tab comes back, however many focus events come with it', async () => {
    let release: (v: unknown) => void = () => undefined;
    const { fake } = setup({ origin: 'required', stepLabel: '' }, { handler: (c) => c.m === 'refreshFactors' ? new Promise(r => { release = r; }) : Promise.resolve(null) });
    document.dispatchEvent(new Event('visibilitychange'));
    window.dispatchEvent(new Event('focus'));
    window.dispatchEvent(new Event('focus'));
    expect(fake.callsOf('refreshFactors')).toHaveLength(1);
    release(null);
    await waitFor(() => { window.dispatchEvent(new Event('focus')); expect(fake.callsOf('refreshFactors')).toHaveLength(2); });
  });
  it('does not read on focus away from the choice, nor outside the forced screen', async () => {
    const forced = setup({ origin: 'required', stepLabel: '' });
    await toTotp(forced.user);
    window.dispatchEvent(new Event('focus'));
    expect(forced.fake.callsOf('refreshFactors')).toHaveLength(0);
    forced.view.unmount();
    const signup = setup();
    window.dispatchEvent(new Event('focus'));
    expect(signup.fake.callsOf('refreshFactors')).toHaveLength(0);
  });
});

describe('finishing', () => {
  it('calls onDone once however often Continue and Open dilla are pressed', async () => {
    const { user, onDone } = setup({ origin: 'required', stepLabel: '' }, { handler: (c) => c.m === 'factorTotpConfirm' ? Promise.resolve({ backupCodes: [] }) : happy(c, new FakeClient()) });
    await toTotp(user);
    await user.type(codeField(), '123456');
    await user.click(button('Continue'));
    await user.dblClick(await screen.findByRole('button', { name: en['factor.done.next'] }));
    expect(onDone).toHaveBeenCalledTimes(1);
  });
});
