import { encode } from './cbor.ts';
import { sha256 } from './hmac.ts';
import { hkdfSha256 } from './hkdf.ts';
import { concat, utf8 } from './bytes.ts';

/** Unsigned big-endian bytes → decimal string left-padded with zeros to 78 digits (2^256 has 78 digits). */
export function decimalDigits(bytes: Uint8Array): string {
  let v = 0n; for (const b of bytes) v = (v << 8n) | BigInt(b);
  return v.toString(10).padStart(78, '0');
}

function lessOrEqual(a: Uint8Array, b: Uint8Array): boolean {
  for (let i = 0; i < a.length; i++) { if (a[i] !== b[i]) return a[i] < b[i]; }
  return true;
}

/** 60 digits: SHA-256(min(umk_a, umk_b) || max(umk_a, umk_b)) as decimal, padded to 78, first 60. */
export async function safetyNumber(umkA: Uint8Array, umkB: Uint8Array): Promise<string> {
  const [lo, hi] = lessOrEqual(umkA, umkB) ? [umkA, umkB] : [umkB, umkA];
  return decimalDigits(await sha256(concat(lo, hi))).slice(0, 60);
}

/** 30 digits from the MLS epoch_authenticator. */
export function sas(epochAuthenticator: Uint8Array): string {
  if (epochAuthenticator.length !== 32) throw new Error('epoch_authenticator must be 32 bytes');
  return decimalDigits(epochAuthenticator).slice(0, 30);
}

const CROCKFORD = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
/** 256 bits → 52 Crockford base32 characters (the last character carries 1 payload bit and 4 zero bits). */
export function recoveryKeyBase32(rk: Uint8Array): string {
  if (rk.length !== 32) throw new Error('recovery key must be 32 bytes');
  let bits = 0, acc = 0, out = '';
  for (const b of rk) { acc = (acc << 8) | b; bits += 8; while (bits >= 5) { out += CROCKFORD[(acc >> (bits - 5)) & 31]; bits -= 5; } }
  if (bits > 0) out += CROCKFORD[(acc << (5 - bits)) & 31];
  return out;
}

export async function deriveRecoveryKeys(rk: Uint8Array): Promise<{ header: Uint8Array; archive: Uint8Array }> {
  const none = new Uint8Array(0);
  return { header: await hkdfSha256(none, rk, utf8('dilla header v1'), 32), archive: await hkdfSha256(none, rk, utf8('dilla archive v1'), 32) };
}

export type CredentialFields = { umkPub: Uint8Array; userId: Uint8Array; deviceId: Uint8Array; kind: 0 | 1; tier: 0 | 1; signerTier: 0 | 1 | 2; sskPub: Uint8Array; sigUmkSsk: Uint8Array; sigSskDev: Uint8Array };
export function credentialIdentity(f: CredentialFields): Uint8Array {
  return encode([1, f.umkPub, f.userId, f.deviceId, f.kind, f.tier, f.signerTier, f.sskPub, f.sigUmkSsk, f.sigSskDev]);
}

/** `"dilla ssk v1" || ssk_pub` - the message UMK_priv signs (03-identity.md "Keys"). */
export function sskMessage(sskPub: Uint8Array): Uint8Array {
  return concat(utf8('dilla ssk v1'), sskPub);
}

/**
 * `"dilla dsk v1" || device_id || dsk_pub || kind || tier || signer_tier` - the message SSK_priv
 * signs. The three trailing fields are one byte each, not CBOR.
 */
export function dskMessage(deviceId: Uint8Array, dskPub: Uint8Array, kind: number, tier: number, signerTier: number): Uint8Array {
  return concat(utf8('dilla dsk v1'), deviceId, dskPub, new Uint8Array([kind, tier, signerTier]));
}

/**
 * The instance-signed account-kind attestation (03-identity.md § Account kind attestation, wire 4):
 * `"dilla account kind v1" || CBOR([1, instance_id, user_id, kind])`. No key: an account's kind never changes.
 */
export function accountKindMessage(instanceId: Uint8Array, userId: Uint8Array, kind: 0 | 1): Uint8Array {
  return concat(utf8('dilla account kind v1'), encode([1, instanceId, userId, kind]));
}

/** The attestation's wire form: `[1, key_id(16), kind, sig(64)]`. */
export function accountKindAttestation(keyId: Uint8Array, kind: 0 | 1, signature: Uint8Array): Uint8Array {
  return encode([1, keyId, kind, signature]);
}

/**
 * `"dilla session v1" || instance_id(16) || device_id(16) || nonce(32) || purpose(1)` - the 81
 * bytes a device's DSK_priv signs to establish, renew or provision a session
 * (02-delivery-service.md "Device sessions" item 2). Every field sits at a fixed offset, so there
 * is nothing to parse and nothing to confuse; `purpose` is 0 session, 1 renew, 2 provisional, and
 * binding it into the signature is what stops a renew signature being replayed as an establish.
 */
export function sessionPreimage(instanceId: Uint8Array, deviceId: Uint8Array, nonce: Uint8Array, purpose: 0 | 1 | 2): Uint8Array {
  if (instanceId.length !== 16) throw new Error('instance_id must be 16 bytes');
  if (deviceId.length !== 16) throw new Error('device_id must be 16 bytes');
  if (nonce.length !== 32) throw new Error('nonce must be 32 bytes');
  return concat(utf8('dilla session v1'), instanceId, deviceId, nonce, new Uint8Array([purpose]));
}
