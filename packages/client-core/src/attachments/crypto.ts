// Browser attachment encryption and vector-compatible authentication.
export const AAD_BLOB = 'dilla attachment v1';
export const AAD_THUMB = 'dilla thumb v1';
export const MAX_THUMB_PLAINTEXT = 8176;
export const BROWSER_ATTACHMENT_CAP = 25 * 1024 * 1024;
export const MAX_ATTACHMENTS = 10;

export class AttachmentError extends Error {
  readonly code: 'E_BLOB_HASH' | 'E_BLOB_OPEN' | 'E_ATTACHMENT_TOO_LARGE' | 'E_ATTACHMENT_COUNT' | 'E_ATTACHMENT_MISSING';
  constructor(code: AttachmentError['code'], detail?: string) {
    super(detail === undefined ? code : `${code}: ${detail}`);
    this.name = 'AttachmentError';
    this.code = code;
  }
}

function checkLength(name: string, value: Uint8Array, length: number): void {
  if (value.length !== length) throw new RangeError(`E_ATTACHMENT_INPUT: ${name} must be ${length} bytes`);
}
function ab(u: Uint8Array): Uint8Array<ArrayBuffer> {
  return u.buffer instanceof ArrayBuffer ? u as Uint8Array<ArrayBuffer> : new Uint8Array(u);
}
async function aesKey(key: Uint8Array, usage: KeyUsage): Promise<CryptoKey> {
  checkLength('key', key, 32);
  return crypto.subtle.importKey('raw', ab(key), 'AES-GCM', false, [usage]);
}
async function sha256(bytes: Uint8Array): Promise<Uint8Array> {
  return new Uint8Array(await crypto.subtle.digest('SHA-256', ab(bytes)));
}
function same(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i];
  return diff === 0;
}
export function thumbNonce(nonce: Uint8Array): Uint8Array {
  checkLength('nonce', nonce, 12);
  const out = nonce.slice();
  out[11] ^= 0x01;
  return out;
}
export function newKeyAndNonce(random: (n: number) => Uint8Array): { key: Uint8Array; nonce: Uint8Array } {
  const key = random(32);
  const nonce = random(12);
  checkLength('key', key, 32);
  checkLength('nonce', nonce, 12);
  return { key, nonce };
}
export async function sealBlob(key: Uint8Array, nonce: Uint8Array, plaintext: Uint8Array): Promise<{ stored: Uint8Array; blobId: Uint8Array }> {
  checkLength('nonce', nonce, 12);
  const stored = new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv: ab(nonce), tagLength: 128,
    additionalData: new TextEncoder().encode(AAD_BLOB) }, await aesKey(key, 'encrypt'), ab(plaintext)));
  return { stored, blobId: await sha256(stored) };
}
export async function openBlob(a: { key: Uint8Array; nonce: Uint8Array; blobId: Uint8Array; size: number }, stored: Uint8Array): Promise<Uint8Array> {
  checkLength('key', a.key, 32);
  checkLength('nonce', a.nonce, 12);
  checkLength('blobId', a.blobId, 32);
  if (!same(await sha256(stored), a.blobId)) throw new AttachmentError('E_BLOB_HASH');
  if (stored.length < 16) throw new AttachmentError('E_BLOB_OPEN');
  const opened = await decrypt(a.key, a.nonce, AAD_BLOB, stored);
  if (opened.length !== a.size) throw new AttachmentError('E_BLOB_OPEN');
  return opened;
}
/** AES-GCM open; only the AEAD failure (WebCrypto's OperationError) becomes E_BLOB_OPEN, anything else propagates. */
async function decrypt(key: Uint8Array, iv: Uint8Array, aad: string, sealed: Uint8Array): Promise<Uint8Array> {
  const k = await aesKey(key, 'decrypt');
  try {
    return new Uint8Array(await crypto.subtle.decrypt({ name: 'AES-GCM', iv: ab(iv), tagLength: 128,
      additionalData: new TextEncoder().encode(aad) }, k, ab(sealed)));
  } catch (e) {
    if (e instanceof DOMException && e.name === 'OperationError') throw new AttachmentError('E_BLOB_OPEN');
    throw e;
  }
}
export async function sealThumb(key: Uint8Array, nonce: Uint8Array, plaintext: Uint8Array): Promise<Uint8Array> {
  if (plaintext.length > MAX_THUMB_PLAINTEXT) throw new RangeError('E_ENVELOPE_LIMIT: thumbnail plaintext over 8176 bytes');
  return new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv: ab(thumbNonce(nonce)), tagLength: 128,
    additionalData: new TextEncoder().encode(AAD_THUMB) }, await aesKey(key, 'encrypt'), ab(plaintext)));
}
export async function openThumb(key: Uint8Array, nonce: Uint8Array, thumb: Uint8Array): Promise<Uint8Array> {
  checkLength('key', key, 32);
  checkLength('nonce', nonce, 12);
  if (thumb.length < 16) throw new AttachmentError('E_BLOB_OPEN');
  return decrypt(key, thumbNonce(nonce), AAD_THUMB, thumb);
}
