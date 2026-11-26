import { desktopBridge } from './desktop.ts';
// The browser's passkey glue. The instance (go-webauthn) sends its options as JSON in which every binary member is
// base64url, and wants the browser's answer the same way. This file converts both ways by hand: it uses neither
// PublicKeyCredential.parseCreationOptionsFromJSON nor toJSON(), which not every supported browser has.
//
// Three rules, each pinned in webauthn.test.ts:
//  - The challenge, and every credential id, comes from the instance's document and nowhere else. Nothing here makes one,
//    and a document without a challenge is refused before the browser is asked.
//  - User verification is always `required` (ruling D2), whatever the document says: an authenticator that cannot check the
//    person is not a second factor. The instance checks the flag again; this keeps the browser from offering such a key.
//  - The relying party id is the document's. It is never filled in from the page's address: when the address does not suit
//    the id the browser throws SecurityError, which the page tells the person (`wrongAddress`).

/** Why a ceremony ended without a passkey. `cancelled`: the person or the browser stopped it; `wrongAddress`: the page's
 *  address is not valid for the instance's relying party id; `exists`: this authenticator already holds a passkey for the
 *  account; `unsupported`: the instance is too old for the desktop app's browser sign-in; `rateLimited`: the instance
 *  answered too many attempts; `other`: anything else, including a document or an answer the browser cannot use. */
export type PasskeyErrorKind = 'cancelled' | 'wrongAddress' | 'exists' | 'unsupported' | 'rateLimited' | 'other';

export class PasskeyError extends Error {
  readonly kind: PasskeyErrorKind;
  constructor(kind: PasskeyErrorKind, message: string) {
    super(message);
    this.name = 'PasskeyError';
    this.kind = kind;
  }
}

/** The kind for what the browser threw: by the DOMException name only. */
export function passkeyErrorKind(e: unknown): PasskeyErrorKind {
  if (!(e instanceof DOMException)) return 'other';
  switch (e.name) {
    case 'NotAllowedError': case 'AbortError': return 'cancelled';
    case 'SecurityError': return 'wrongAddress';
    case 'InvalidStateError': return 'exists';
    default: return 'other';
  }
}

/** What the desktop shell's browser sign-in says when the instance cannot take the request. Electron wraps the shell's
 *  message ("Error invoking remote method …"), so the code is read out of it. */
export function desktopAuthKind(e: unknown): 'unsupported' | 'rateLimited' | null {
  if (!(e instanceof Error)) return null;
  if (e.message.includes('desktop-auth-unsupported')) return 'unsupported';
  if (e.message.includes('desktop-auth-rate-limited')) return 'rateLimited';
  return null;
}

/** Whether this page can run a passkey ceremony at all: the instance offers passkeys, the browser has the API and the
 *  page is a secure context. */
export function passkeysUsable(instance: { passkeys: boolean } | null | undefined): boolean {
  return instance?.passkeys === true && 'PublicKeyCredential' in window && window.isSecureContext;
}

const bad = (what: string): PasskeyError => new PasskeyError('other', `passkey options: ${what}`);

// base64url (RFC 4648 section 5). The decoder accepts the unpadded form go-webauthn sends and the padded one, and nothing else.
const URL_ALPHABET = /^[A-Za-z0-9_-]*={0,2}$/;

function fromBase64Url(s: unknown, what: string): ArrayBuffer {
  if (typeof s !== 'string' || !URL_ALPHABET.test(s)) throw bad(`${what} is not base64url`);
  const body = s.replace(/=+$/, '');
  // Padding, when present, must complete the last group of four.
  if (body.length !== s.length && s.length % 4 !== 0) throw bad(`${what} is not base64url`);
  if (body.length % 4 === 1) throw bad(`${what} is not base64url`);
  const standard = body.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (body.length % 4)) % 4);
  let binary: string;
  try { binary = atob(standard); } catch { throw bad(`${what} is not base64url`); }
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out.buffer;
}

function toBase64Url(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  let binary = '';
  // In chunks, so a large attestation object cannot overflow the argument list.
  for (let i = 0; i < bytes.length; i += 0x8000) binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

type Json = Record<string, unknown>;
const isObject = (v: unknown): v is Json => typeof v === 'object' && v !== null && !Array.isArray(v);

/** `{publicKey:{…}}` from the instance, as an object; anything else is refused. */
function publicKeyOf(optionsJson: string): Json {
  let doc: unknown;
  try { doc = JSON.parse(optionsJson); } catch { throw bad('not JSON'); }
  if (!isObject(doc) || !isObject(doc.publicKey)) throw bad('no publicKey');
  return doc.publicKey;
}

/** A credential descriptor list with its ids decoded; absent stays absent. */
function descriptors(list: unknown, what: string): PublicKeyCredentialDescriptor[] | undefined {
  if (list === undefined || list === null) return undefined;
  if (!Array.isArray(list)) throw bad(`${what} is not a list`);
  return list.map((d: unknown): PublicKeyCredentialDescriptor => {
    if (!isObject(d)) throw bad(`${what} holds a non-object`);
    const out: PublicKeyCredentialDescriptor = { type: 'public-key', id: fromBase64Url(d.id, `${what} id`) };
    if (Array.isArray(d.transports)) out.transports = d.transports.filter((x): x is AuthenticatorTransport => typeof x === 'string');
    return out;
  });
}

/** The challenge, decoded. WebAuthn asks for at least 16 random bytes; a shorter one is not an instance's challenge. */
function challengeOf(pk: Json): ArrayBuffer {
  const challenge = fromBase64Url(pk.challenge, 'challenge');
  if (challenge.byteLength < 16) throw bad('challenge is shorter than 16 bytes');
  return challenge;
}

function credentialsApi(): CredentialsContainer {
  const api = typeof navigator === 'undefined' ? undefined : (navigator as { credentials?: CredentialsContainer }).credentials;
  if (api === undefined) throw new PasskeyError('other', 'navigator.credentials is not available');
  return api;
}

/** Runs one ceremony and maps what the browser throws. `exists` is meaningful only when creating. */
async function ceremony(creating: boolean, run: () => Promise<Credential | null>): Promise<Credential> {
  let credential: Credential | null;
  try { credential = await run(); } catch (e) { const kind = passkeyErrorKind(e);
    throw new PasskeyError(kind === 'exists' && !creating ? 'other' : kind, e instanceof Error ? e.message : 'ceremony failed'); }
  if (credential === null) throw new PasskeyError('other', 'the browser returned no credential');
  return credential;
}

const isBuffer = (v: unknown): v is ArrayBuffer => v instanceof ArrayBuffer || Object.prototype.toString.call(v) === '[object ArrayBuffer]';

function buffer(v: unknown, what: string): ArrayBuffer {
  if (!isBuffer(v)) throw new PasskeyError('other', `the credential has no ${what}`);
  return v;
}

/** The members both answers share, in the shape go-webauthn parses. */
function envelope(credential: Credential): { head: Json; response: Json; raw: Json } {
  const c = credential as unknown as Json;
  if (c.type !== 'public-key' || typeof c.id !== 'string' || !isObject(c.response)) throw new PasskeyError('other', 'not a public-key credential');
  return {
    head: { id: c.id, rawId: toBase64Url(buffer(c.rawId, 'rawId')), type: 'public-key',
      authenticatorAttachment: typeof c.authenticatorAttachment === 'string' ? c.authenticatorAttachment : null },
    response: { clientDataJSON: toBase64Url(buffer(c.response.clientDataJSON, 'clientDataJSON')) },
    raw: c.response,
  };
}

/** Creates a passkey from the instance's registration options (`{publicKey:{…}}`, base64url members). Resolves with the
 *  credential as JSON for the finish call; rejects with a PasskeyError. */
export async function createPasskey(optionsJson: string): Promise<string> {
  const bridge = desktopBridge();
  if (bridge?.browserAuth) {
    try {
      const result = await bridge.browserAuth({ kind: 'create', options: optionsJson });
      if (typeof result.response !== 'string') throw new Error('Missing browser credential');
      return result.response;
    } catch (error) { throw new PasskeyError(desktopAuthKind(error) ?? 'other', error instanceof Error ? error.message : 'Browser sign-in failed'); }
  }
  const pk = publicKeyOf(optionsJson);
  if (!isObject(pk.rp) || !isObject(pk.user)) throw bad('no rp or user');
  const publicKey = {
    ...(pk as unknown as Partial<PublicKeyCredentialCreationOptions>),
    challenge: challengeOf(pk),
    user: { ...(pk.user as unknown as PublicKeyCredentialUserEntity), id: fromBase64Url(pk.user.id, 'user id') },
    rp: pk.rp as unknown as PublicKeyCredentialRpEntity,
    authenticatorSelection: { ...(isObject(pk.authenticatorSelection) ? pk.authenticatorSelection : {}), userVerification: 'required' },
  } as PublicKeyCredentialCreationOptions;
  const exclude = descriptors(pk.excludeCredentials, 'excludeCredentials');
  if (exclude === undefined) delete publicKey.excludeCredentials;
  else publicKey.excludeCredentials = exclude;
  const api = credentialsApi();
  const credential = await ceremony(true, () => api.create({ publicKey }));
  const { head, response, raw } = envelope(credential);
  response.attestationObject = toBase64Url(buffer(raw.attestationObject, 'attestationObject'));
  const transports = typeof raw.getTransports === 'function' ? (raw.getTransports as () => unknown).call(raw) : [];
  response.transports = Array.isArray(transports) ? transports.filter((x): x is string => typeof x === 'string') : [];
  return JSON.stringify({ ...head, response, clientExtensionResults: {} });
}

/** Wave B task 3 (Jonas, fix round 1): whether the browser can offer passkeys in the username field's autofill (WebAuthn
 *  conditional mediation). False where the API is missing or the check throws. */
export async function conditionalMediationAvailable(): Promise<boolean> {
  if (desktopBridge()?.browserAuth) return false; // The native shell uses an explicit system-browser handoff.
  if (typeof window === 'undefined' || !('PublicKeyCredential' in window) || !window.isSecureContext) return false;
  const check = (window.PublicKeyCredential as unknown as { isConditionalMediationAvailable?: () => Promise<boolean> }).isConditionalMediationAvailable;
  if (typeof check !== 'function') return false;
  try { return await check.call(window.PublicKeyCredential) === true; } catch { return false; }
}

/** Signs a challenge with a passkey from the instance's login options (`{publicKey:{…}}`, base64url members). Resolves
 *  with the assertion as JSON; rejects with a PasskeyError. `conditional` makes it the username field's autofill request
 *  (mediation 'conditional'), ended by aborting its signal; the same options and the same rules apply. */
export async function getPasskey(optionsJson: string, conditional?: { signal: AbortSignal }): Promise<string> {
  const bridge = desktopBridge();
  if (bridge?.browserAuth) {
    // The shell has no autofill: a conditional request would open the system browser on a field's focus and spend a
    // ceremony, so it is refused here whatever the caller checked (desktop merge review M1).
    if (conditional !== undefined) throw new PasskeyError('cancelled', 'Passkey autofill is not available in the desktop app');
    try {
      const result = await bridge.browserAuth({ kind: 'get', options: optionsJson });
      if (typeof result.response !== 'string') throw new Error('Missing browser credential');
      return result.response;
    } catch (error) { throw new PasskeyError(desktopAuthKind(error) ?? 'other', error instanceof Error ? error.message : 'Browser sign-in failed'); }
  }
  const pk = publicKeyOf(optionsJson);
  const publicKey = {
    ...(pk as unknown as Partial<PublicKeyCredentialRequestOptions>),
    challenge: challengeOf(pk),
    userVerification: 'required',
  } as PublicKeyCredentialRequestOptions;
  const allow = descriptors(pk.allowCredentials, 'allowCredentials');
  if (allow === undefined) delete publicKey.allowCredentials;
  else publicKey.allowCredentials = allow;
  const api = credentialsApi();
  const credential = await ceremony(false, () => (conditional === undefined ? api.get({ publicKey })
    : api.get({ publicKey, mediation: 'conditional', signal: conditional.signal })));
  const { head, response, raw } = envelope(credential);
  response.authenticatorData = toBase64Url(buffer(raw.authenticatorData, 'authenticatorData'));
  response.signature = toBase64Url(buffer(raw.signature, 'signature'));
  response.userHandle = raw.userHandle === null || raw.userHandle === undefined ? null : toBase64Url(buffer(raw.userHandle, 'userHandle'));
  return JSON.stringify({ ...head, response, clientExtensionResults: {} });
}
