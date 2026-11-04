import { afterEach, describe, expect, it, vi } from 'vitest';
import { PasskeyError, createPasskey, desktopAuthKind, getPasskey, passkeysUsable, passkeyErrorKind } from './webauthn.ts';

// A go-webauthn options document: every binary member is base64url without padding, as the library marshals it.
const CHALLENGE = 'Zm9vYmFyLWNoYWxsZW5nZS1fLQ';
const USER_ID = 'AQIDBAUGBwgJCgsMDQ4PEA';
const CRED = '-_-_AAECAw';
const CREATE = JSON.stringify({ publicKey: {
  challenge: CHALLENGE, rp: { name: 'dilla.test', id: 'dilla.test' }, user: { id: USER_ID, name: 'ada', displayName: 'ada' },
  pubKeyCredParams: [{ type: 'public-key', alg: -7 }], timeout: 60000,
  excludeCredentials: [{ type: 'public-key', id: CRED, transports: ['internal'] }],
  authenticatorSelection: { residentKey: 'preferred', userVerification: 'preferred' }, attestation: 'none',
} });
const GET = JSON.stringify({ publicKey: {
  challenge: CHALLENGE, timeout: 60000, rpId: 'dilla.test', userVerification: 'preferred',
  allowCredentials: [{ type: 'public-key', id: CRED }],
} });

const bytes = (...n: number[]): ArrayBuffer => new Uint8Array(n).buffer;
const view = (b: BufferSource | null | undefined): number[] => {
  if (b === null || b === undefined) return [];
  return [...new Uint8Array(b instanceof ArrayBuffer ? b : b.buffer)];
};

/** The publicKey of the n-th call the browser was asked to make. */
function publicKeyOf<T>(call: readonly [{ publicKey?: T }?] | undefined): T {
  const pk = call?.[0]?.publicKey;
  if (pk === undefined) throw new Error('the browser was not asked');
  return pk;
}

function install(credentials: Partial<CredentialsContainer>): void {
  Object.defineProperty(navigator, 'credentials', { value: credentials, configurable: true });
}
afterEach(() => { Reflect.deleteProperty(navigator, 'credentials'); vi.restoreAllMocks(); });

function created(extra: Record<string, unknown> = {}) {
  return { id: CRED, rawId: bytes(251, 255, 191, 0, 1, 2, 3), type: 'public-key', authenticatorAttachment: 'platform',
    response: { clientDataJSON: bytes(1, 2, 3), attestationObject: bytes(255, 254, 253, 252), getTransports: () => ['internal', 'hybrid'] },
    getClientExtensionResults: () => ({ credProps: { rk: true } }), ...extra };
}

describe('createPasskey', () => {
  it('decodes the options by hand, forces user verification and serialises the response in base64url', async () => {
    const create = vi.fn<CredentialsContainer['create']>(() => Promise.resolve(created() as unknown as Credential));
    install({ create });
    const out = JSON.parse(await createPasskey(CREATE)) as Record<string, unknown>;
    const pk = publicKeyOf(create.mock.calls[0]);
    expect(new TextDecoder().decode(pk.challenge as ArrayBuffer)).toBe('foobar-challenge-_-');
    expect(view(pk.user.id)).toEqual([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16]);
    expect(view(pk.excludeCredentials?.[0]?.id)).toEqual([251, 255, 191, 0, 1, 2, 3]);
    expect(pk.rp.id).toBe('dilla.test');
    expect(pk.authenticatorSelection?.userVerification).toBe('required');
    expect(pk.authenticatorSelection?.residentKey).toBe('preferred');
    expect(out).toEqual({ id: CRED, rawId: '-_-_AAECAw', type: 'public-key', authenticatorAttachment: 'platform',
      response: { clientDataJSON: 'AQID', attestationObject: '__79_A', transports: ['internal', 'hybrid'] }, clientExtensionResults: {} });
  });
  it('forces required even when the document says discouraged or omits the selection', async () => {
    const create = vi.fn<CredentialsContainer['create']>(() => Promise.resolve(created() as unknown as Credential));
    install({ create });
    const doc = JSON.parse(CREATE) as { publicKey: Record<string, unknown> };
    delete doc.publicKey.authenticatorSelection;
    await createPasskey(JSON.stringify(doc));
    expect(publicKeyOf(create.mock.calls[0]).authenticatorSelection?.userVerification).toBe('required');
    doc.publicKey.authenticatorSelection = { userVerification: 'discouraged' };
    await createPasskey(JSON.stringify(doc));
    expect(publicKeyOf(create.mock.calls[1]).authenticatorSelection?.userVerification).toBe('required');
  });
  it('leaves rp.id to the document: it never fills one from the page', async () => {
    const create = vi.fn<CredentialsContainer['create']>(() => Promise.resolve(created() as unknown as Credential));
    install({ create });
    const doc = JSON.parse(CREATE) as { publicKey: { rp: Record<string, unknown> } };
    delete doc.publicKey.rp.id;
    await createPasskey(JSON.stringify(doc));
    expect(publicKeyOf(create.mock.calls[0]).rp.id).toBeUndefined();
  });
  it('never makes a challenge of its own: a document without one is refused before the browser is asked', async () => {
    const create = vi.fn();
    install({ create });
    const doc = JSON.parse(CREATE) as { publicKey: Record<string, unknown> };
    delete doc.publicKey.challenge;
    await expect(createPasskey(JSON.stringify(doc))).rejects.toMatchObject({ kind: 'other' });
    expect(create).not.toHaveBeenCalled();
  });
  it.each([['standard alphabet', 'ab+/ab+/ab+/ab+/ab+/ab'], ['length mod 4 is 1', 'AAAAAAAAAAAAAAAAAAAAA'], ['empty', ''],
    ['too short', 'Zm9vYg'], ['over-padded', 'Zm9vYmFyLWNoYWxsZW5nZS1fLQ==='], ['wrong padding', 'Zm9vYmFyLWNoYWxsZW5nZS1fLQ=']])(
    'refuses a challenge that is %s before the browser is asked', async (...row) => {
    const challenge = row[1];
    const create = vi.fn();
    install({ create });
    const doc = JSON.parse(CREATE) as { publicKey: Record<string, unknown> };
    doc.publicKey.challenge = challenge;
    await expect(createPasskey(JSON.stringify(doc))).rejects.toMatchObject({ kind: 'other' });
    expect(create).not.toHaveBeenCalled();
    const get = vi.fn();
    install({ get });
    const login = JSON.parse(GET) as { publicKey: Record<string, unknown> };
    login.publicKey.challenge = challenge;
    await expect(getPasskey(JSON.stringify(login))).rejects.toMatchObject({ kind: 'other' });
    expect(get).not.toHaveBeenCalled();
  });
  it('encodes a buffer longer than one chunk without losing a byte', async () => {
    const long = new Uint8Array(0x8000 * 2 + 5).map((_, i) => i % 251);
    install({ create: () => Promise.resolve(created({ rawId: long.buffer }) as unknown as Credential) });
    const out = JSON.parse(await createPasskey(CREATE)) as { rawId: string };
    const back = Uint8Array.from(atob(out.rawId.replace(/-/g, '+').replace(/_/g, '/')), c => c.charCodeAt(0));
    expect(back).toEqual(long);
  });
  it.each([['not json'], ['{}'], ['{"publicKey":null}'], ['{"publicKey":{"challenge":"!!"}}']])('refuses the malformed document %s', async (doc) => {
    const create = vi.fn();
    install({ create });
    await expect(createPasskey(doc)).rejects.toBeInstanceOf(PasskeyError);
    expect(create).not.toHaveBeenCalled();
  });
  it('maps what the browser throws and refuses a null or foreign credential', async () => {
    for (const [name, kind] of [['NotAllowedError', 'cancelled'], ['AbortError', 'cancelled'], ['SecurityError', 'wrongAddress'],
      ['InvalidStateError', 'exists'], ['NotSupportedError', 'other']] as const) {
      install({ create: () => Promise.reject(new DOMException('x', name)) });
      await expect(createPasskey(CREATE)).rejects.toMatchObject({ kind });
    }
    install({ create: () => Promise.resolve(null) });
    await expect(createPasskey(CREATE)).rejects.toMatchObject({ kind: 'other' });
    install({ create: () => Promise.resolve(created({ response: { clientDataJSON: bytes(1) } }) as unknown as Credential) });
    await expect(createPasskey(CREATE)).rejects.toMatchObject({ kind: 'other' });
  });
  it('is other without navigator.credentials', async () => {
    await expect(createPasskey(CREATE)).rejects.toMatchObject({ kind: 'other' });
  });
});

describe('getPasskey', () => {
  const assertion = (userHandle: ArrayBuffer | null) => ({ id: CRED, rawId: bytes(251, 255, 191, 0, 1, 2, 3), type: 'public-key', authenticatorAttachment: null,
    response: { clientDataJSON: bytes(1, 2, 3), authenticatorData: bytes(255, 0), signature: bytes(9, 8, 7), userHandle },
    getClientExtensionResults: () => ({}) });
  it('keeps the document rpId and challenge, requires user verification and serialises the assertion', async () => {
    const get = vi.fn<CredentialsContainer['get']>(() => Promise.resolve(assertion(bytes(1, 2, 3)) as unknown as Credential));
    install({ get });
    const out = JSON.parse(await getPasskey(GET)) as Record<string, unknown>;
    const pk = publicKeyOf(get.mock.calls[0]);
    expect(pk.rpId).toBe('dilla.test');
    expect(pk.userVerification).toBe('required');
    expect(new TextDecoder().decode(pk.challenge as ArrayBuffer)).toBe('foobar-challenge-_-');
    expect(view(pk.allowCredentials?.[0]?.id)).toEqual([251, 255, 191, 0, 1, 2, 3]);
    expect(out).toEqual({ id: CRED, rawId: '-_-_AAECAw', type: 'public-key', authenticatorAttachment: null,
      response: { clientDataJSON: 'AQID', authenticatorData: '_wA', signature: 'CQgH', userHandle: 'AQID' }, clientExtensionResults: {} });
  });
  it('sends a null userHandle as null, and an absent allowCredentials as absent', async () => {
    const get = vi.fn<CredentialsContainer['get']>(() => Promise.resolve(assertion(null) as unknown as Credential));
    install({ get });
    const doc = JSON.parse(GET) as { publicKey: Record<string, unknown> };
    delete doc.publicKey.allowCredentials;
    const out = JSON.parse(await getPasskey(JSON.stringify(doc))) as { response: { userHandle: unknown } };
    expect(out.response.userHandle).toBeNull();
    expect(publicKeyOf(get.mock.calls[0]).allowCredentials).toBeUndefined();
  });
  it('maps what the browser throws', async () => {
    for (const [name, kind] of [['NotAllowedError', 'cancelled'], ['SecurityError', 'wrongAddress'], ['InvalidStateError', 'other']] as const) {
      install({ get: () => Promise.reject(new DOMException('x', name)) });
      await expect(getPasskey(GET)).rejects.toMatchObject({ kind });
    }
  });
});

describe('passkeyErrorKind', () => {
  it.each([['NotAllowedError', 'cancelled'], ['AbortError', 'cancelled'], ['SecurityError', 'wrongAddress'],
    ['InvalidStateError', 'exists'], ['ConstraintError', 'other'], ['UnknownError', 'other']] as const)('%s is %s', (name, kind) => {
    expect(passkeyErrorKind(new DOMException('x', name))).toBe(kind);
  });
  it('is other for anything that is not a DOMException', () => {
    expect(passkeyErrorKind(new Error('NotAllowedError'))).toBe('other');
    expect(passkeyErrorKind('NotAllowedError')).toBe('other');
  });
});

describe('the desktop app’s browser sign-in errors', () => {
  afterEach(() => { Reflect.deleteProperty(globalThis, 'dillaDesktop'); });
  const via = (message: string) => {
    (globalThis as unknown as { dillaDesktop?: unknown }).dillaDesktop = { browserAuth: () => Promise.reject(new Error(`Error invoking remote method 'dilla:browser-auth': Error: ${message}`)) };
  };
  it.each([['desktop-auth-unsupported', 'unsupported'], ['desktop-auth-rate-limited', 'rateLimited'], ['Browser sign-in failed', 'other']] as const)('%s is %s', async (message, kind) => {
    via(message);
    for (const call of [() => getPasskey(GET), () => createPasskey(CREATE)]) {
      await expect(call()).rejects.toMatchObject({ name: 'PasskeyError', kind });
    }
  });
  it('desktopAuthKind reads the code out of the wrapped message', () => {
    expect(desktopAuthKind(new Error('x desktop-auth-unsupported'))).toBe('unsupported');
    expect(desktopAuthKind(new Error('desktop-auth-rate-limited'))).toBe('rateLimited');
    expect(desktopAuthKind(new Error('nope'))).toBeNull();
    expect(desktopAuthKind('desktop-auth-unsupported')).toBeNull();
  });
});

describe('passkeysUsable', () => {
  const secure = (on: boolean) => Object.defineProperty(window, 'isSecureContext', { value: on, configurable: true });
  afterEach(() => { Reflect.deleteProperty(window, 'PublicKeyCredential'); secure(true); });
  it('needs the instance, the API and a secure context together', () => {
    secure(true);
    expect(passkeysUsable({ passkeys: true })).toBe(false);
    Object.defineProperty(window, 'PublicKeyCredential', { value: class {}, configurable: true });
    expect(passkeysUsable({ passkeys: true })).toBe(true);
    expect(passkeysUsable({ passkeys: false })).toBe(false);
    expect(passkeysUsable(null)).toBe(false);
    secure(false);
    expect(passkeysUsable({ passkeys: true })).toBe(false);
  });
});
