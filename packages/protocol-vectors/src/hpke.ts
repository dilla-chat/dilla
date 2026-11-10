/**
 * RFC 9180 HPKE, base mode only, for the one suite dilla uses: DHKEM(X25519, HKDF-SHA256), HKDF-SHA256,
 * AES-128-GCM (0x0020/0x0001/0x0001). X25519 and AES-GCM come from WebCrypto; HKDF Extract and Expand are written
 * out here over HMAC because LabeledExtract and LabeledExpand need the two steps apart.
 *
 * This is the vector generator's own implementation, written from the RFC and not from the Rust core, so the
 * committed vectors are an independent cross-check of `core/dilla-core/src/bot/command.rs`.
 */
import { hmacSha256 } from './hmac.ts';
import { concat, be16, utf8 } from './bytes.ts';

const KEM_ID = 0x0020, KDF_ID = 0x0001, AEAD_ID = 0x0001;
const NSK = 32, NH = 32, NK = 16, NN = 12;
const KEM_SUITE = concat(utf8('KEM'), be16(KEM_ID));
const HPKE_SUITE = concat(utf8('HPKE'), be16(KEM_ID), be16(KDF_ID), be16(AEAD_ID));
const V1 = utf8('HPKE-v1');

/** RFC 5869 Extract: HMAC(salt, ikm); an empty salt is HashLen zero bytes. */
async function extract(salt: Uint8Array, ikm: Uint8Array): Promise<Uint8Array> {
  return hmacSha256(salt.length === 0 ? new Uint8Array(NH) : salt, ikm);
}

/** RFC 5869 Expand. */
async function expand(prk: Uint8Array, info: Uint8Array, length: number): Promise<Uint8Array> {
  if (length > 255 * NH) throw new Error('HKDF expand: length too large');
  const out = new Uint8Array(length);
  let t = new Uint8Array(0);
  for (let i = 1, o = 0; o < length; i++) {
    t = await hmacSha256(prk, concat(t, info, new Uint8Array([i])));
    out.set(t.subarray(0, Math.min(NH, length - o)), o);
    o += NH;
  }
  return out;
}

export async function labeledExtract(salt: Uint8Array, suite: Uint8Array, label: string, ikm: Uint8Array): Promise<Uint8Array> {
  return extract(salt, concat(V1, suite, utf8(label), ikm));
}

export async function labeledExpand(prk: Uint8Array, suite: Uint8Array, label: string, info: Uint8Array, length: number): Promise<Uint8Array> {
  return expand(prk, concat(be16(length), V1, suite, utf8(label), info), length);
}

/** SEQUENCE { INTEGER 0, SEQUENCE { OID 1.3.101.110 }, OCTET STRING { OCTET STRING (32) } } */
const PKCS8_X25519_PREFIX = new Uint8Array([
  0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x6e, 0x04, 0x22, 0x04, 0x20,
]);

async function privateKey(sk: Uint8Array): Promise<CryptoKey> {
  return crypto.subtle.importKey('pkcs8', concat(PKCS8_X25519_PREFIX, sk), { name: 'X25519' }, true, ['deriveBits']);
}

async function publicOf(sk: Uint8Array): Promise<Uint8Array> {
  const jwk = await crypto.subtle.exportKey('jwk', await privateKey(sk));
  if (typeof jwk.x !== 'string') throw new Error('X25519 JWK has no public component');
  return Uint8Array.from(Buffer.from(jwk.x.replace(/-/g, '+').replace(/_/g, '/'), 'base64'));
}

async function dh(sk: Uint8Array, pk: Uint8Array): Promise<Uint8Array> {
  const pub = await crypto.subtle.importKey('raw', pk, { name: 'X25519' }, true, []);
  const out = new Uint8Array(await crypto.subtle.deriveBits({ name: 'X25519', public: pub }, await privateKey(sk), 256));
  if (out.every((b) => b === 0)) throw new Error('X25519: all-zero shared secret');
  return out;
}

export interface KeyPair { sk: Uint8Array; pk: Uint8Array }

/** RFC 9180 section 7.1.3 DeriveKeyPair for X25519. */
export async function deriveKeyPair(ikm: Uint8Array): Promise<KeyPair> {
  const prk = await labeledExtract(new Uint8Array(0), KEM_SUITE, 'dkp_prk', ikm);
  const sk = await labeledExpand(prk, KEM_SUITE, 'sk', new Uint8Array(0), NSK);
  return { sk, pk: await publicOf(sk) };
}

async function extractAndExpand(dhOut: Uint8Array, kemContext: Uint8Array): Promise<Uint8Array> {
  const prk = await labeledExtract(new Uint8Array(0), KEM_SUITE, 'eae_prk', dhOut);
  return labeledExpand(prk, KEM_SUITE, 'shared_secret', kemContext, 32);
}

async function keySchedule(shared: Uint8Array, info: Uint8Array): Promise<{ key: Uint8Array; nonce: Uint8Array }> {
  const empty = new Uint8Array(0);
  const pskIdHash = await labeledExtract(empty, HPKE_SUITE, 'psk_id_hash', empty);
  const infoHash = await labeledExtract(empty, HPKE_SUITE, 'info_hash', info);
  const context = concat(new Uint8Array([0]), pskIdHash, infoHash);
  const secret = await labeledExtract(shared, HPKE_SUITE, 'secret', empty);
  return {
    key: await labeledExpand(secret, HPKE_SUITE, 'key', context, NK),
    nonce: await labeledExpand(secret, HPKE_SUITE, 'base_nonce', context, NN),
  };
}

async function aesKey(key: Uint8Array, usage: KeyUsage): Promise<CryptoKey> {
  return crypto.subtle.importKey('raw', key, { name: 'AES-GCM' }, false, [usage]);
}

/** Single-shot base-mode seal (sequence number 0) with the ephemeral key DeriveKeyPair(ikmE). */
export async function sealBase(pkR: Uint8Array, info: Uint8Array, aad: Uint8Array, pt: Uint8Array, ikmE: Uint8Array): Promise<{ enc: Uint8Array; ct: Uint8Array }> {
  const e = await deriveKeyPair(ikmE);
  const shared = await extractAndExpand(await dh(e.sk, pkR), concat(e.pk, pkR));
  const { key, nonce } = await keySchedule(shared, info);
  const ct = new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv: nonce, additionalData: aad }, await aesKey(key, 'encrypt'), pt));
  return { enc: e.pk, ct };
}

/** Single-shot base-mode open (sequence number 0). */
export async function openBase(skR: Uint8Array, enc: Uint8Array, info: Uint8Array, aad: Uint8Array, ct: Uint8Array): Promise<Uint8Array> {
  const pkR = await publicOf(skR);
  const shared = await extractAndExpand(await dh(skR, enc), concat(enc, pkR));
  const { key, nonce } = await keySchedule(shared, info);
  return new Uint8Array(await crypto.subtle.decrypt({ name: 'AES-GCM', iv: nonce, additionalData: aad }, await aesKey(key, 'decrypt'), ct));
}
