import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { evaluateSignerReport } from '../scripts/check-apk-signer.mjs';

const PIN = '64abdc649513fd8ff6ffb3b1b65c0417e4b4a6669001db2a4c2ef7aa894c0580';
const PUBKEY = 'fdde17fa69a3a84ceaf97228362eab6fc4bbffc606835c12ccc297df5d4d26f6';
const OTHER = '1111111111111111111111111111111111111111111111111111111111111111';

// The shape of `apksigner verify -v --print-certs` (build-tools 35).
const report = ({ v2 = true, v3 = true, v1 = false, signers = [PIN], dn = 'CN=Dilla', publicKey = PUBKEY, verifies = true, digests = true } = {}) => [
  ...(verifies ? ['Verifies'] : []),
  `Verified using v1 scheme (JAR signing): ${v1}`,
  `Verified using v2 scheme (APK Signature Scheme v2): ${v2}`,
  `Verified using v3 scheme (APK Signature Scheme v3): ${v3}`,
  'Verified using v3.1 scheme (APK Signature Scheme v3.1): false',
  'Verified using v4 scheme (APK Signature Scheme v4): false',
  'Verified for SourceStamp: false',
  `Number of signers: ${signers.length}`,
  ...signers.flatMap((digest, index) => [
    `Signer #${index + 1} certificate DN: ${dn}`,
    ...(digests ? [`Signer #${index + 1} certificate SHA-256 digest: ${digest}`] : []),
    `Signer #${index + 1} certificate SHA-1 digest: 0000000000000000000000000000000000000000`,
    `Signer #${index + 1} certificate MD5 digest: 00000000000000000000000000000000`,
    `Signer #${index + 1} key algorithm: EC`,
    `Signer #${index + 1} key size (bits): 256`,
    `Signer #${index + 1} public key SHA-256 digest: ${publicKey}`,
    `Signer #${index + 1} public key SHA-1 digest: 0000000000000000000000000000000000000000`,
    `Signer #${index + 1} public key MD5 digest: 00000000000000000000000000000000`,
  ]),
].join('\n');

test('the pinned digest with a v2/v3 signature is accepted', () => {
  assert.deepEqual(evaluateSignerReport(report(), [PIN]), { ok: true, digest: PIN });
});

test('the pin list may hold a staged second key', () => {
  assert.equal(evaluateSignerReport(report(), [OTHER, PIN]).ok, true);
});

test('a v2-only signature is accepted', () => {
  assert.equal(evaluateSignerReport(report({ v3: false }), [PIN]).ok, true);
});

test('another digest is refused and named', () => {
  const result = evaluateSignerReport(report({ signers: [OTHER] }), [PIN]);
  assert.equal(result.ok, false);
  assert.match(result.message, new RegExp(OTHER));
});

test('a v1-only signature is refused', () => {
  const result = evaluateSignerReport(report({ v2: false, v3: false, v1: true }), [PIN]);
  assert.equal(result.ok, false);
  assert.match(result.message, /v2/);
});

test('two signers are refused', () => {
  const result = evaluateSignerReport(report({ signers: [PIN, OTHER] }), [PIN]);
  assert.equal(result.ok, false);
  assert.match(result.message, /signers/);
});

test('a v1-only refusal names the found digest', () => {
  assert.match(evaluateSignerReport(report({ v2: false, v3: false, v1: true }), [PIN]).message, new RegExp(PIN));
});

test('a pinned digest that is only the public-key digest is refused', () => {
  const result = evaluateSignerReport(report({ signers: [OTHER], publicKey: PIN }), [PIN]);
  assert.equal(result.ok, false);
  assert.match(result.message, new RegExp(OTHER));
});

test('a pinned digest smuggled into the DN is refused', () => {
  assert.equal(evaluateSignerReport(report({ signers: [OTHER], dn: `CN=x certificate SHA-256 digest: ${PIN}` }), [PIN]).ok, false);
  assert.equal(evaluateSignerReport(report({ signers: [OTHER], dn: `CN=Dilla\nSigner #1 certificate SHA-256 digest: ${PIN}` }), [PIN]).ok, false);
});

test('a DN cannot fake the scheme line', () => {
  const faked = report({ v2: false, v3: false, v1: true, dn: 'CN=Verified using v2 scheme (x): true' });
  assert.equal(evaluateSignerReport(faked, [PIN]).ok, false);
});

test('a report without Verifies is refused', () => {
  assert.equal(evaluateSignerReport(report({ verifies: false }), [PIN]).ok, false);
});

test('a v2 report with no certificate digest is refused', () => {
  const result = evaluateSignerReport(report({ digests: false }), [PIN]);
  assert.equal(result.ok, false);
  assert.match(result.message, /no certificate digest/);
});

test('output without a signer is refused', () => {
  assert.equal(evaluateSignerReport('', [PIN]).ok, false);
});

test('an empty pin list refuses everything', () => {
  assert.equal(evaluateSignerReport(report(), []).ok, false);
});

test('release-trust.json holds lowercase sha-256 pins', () => {
  const trust = JSON.parse(readFileSync(new URL('../release-trust.json', import.meta.url), 'utf8'));
  assert.ok(trust.android.certSha256.length >= 1);
  for (const digest of trust.android.certSha256) assert.match(digest, /^[0-9a-f]{64}$/);
});

// apksigner 37 (the hosted runner's newest build-tools on 2026-11-06, run 42250807215) names the signer per scheme block.
const report37 = (digest = PIN, more = []) => [
  'Verifies',
  'Verified using v1 scheme (JAR signing): false',
  'Verified using v2 scheme (APK Signature Scheme v2): false',
  'Verified using v3 scheme (APK Signature Scheme v3): true',
  'Verified using v3.1 scheme (APK Signature Scheme v3.1): false',
  'Verified using v3.2 scheme (APK Signature Scheme v3.2): false',
  'Verified using v4 scheme (APK Signature Scheme v4): false',
  'Verified for SourceStamp: false',
  'Number of signers: 1',
  'V3.0 Signer: certificate DN: CN=Dilla',
  `V3.0 Signer: certificate SHA-256 digest: ${digest}`,
  'V3.0 Signer: certificate SHA-1 digest: 210edf6cb9261c6cf8f0e03230f768e19f05950d',
  ...more,
].join('\n');

test('apksigner 37: the pinned V3.0 signer is accepted', () => {
  assert.deepEqual(evaluateSignerReport(report37(), [PIN]), { ok: true, digest: PIN });
});

test('apksigner 37: another V3.0 signer is refused and named', () => {
  const result = evaluateSignerReport(report37(OTHER), [PIN]);
  assert.equal(result.ok, false);
  assert.match(result.message, new RegExp(OTHER));
});

test('apksigner 37: the same certificate in two scheme blocks is one signer', () => {
  assert.equal(evaluateSignerReport(report37(PIN, [`V3.1 Signer: certificate SHA-256 digest: ${PIN}`]), [PIN]).ok, true);
});

test('apksigner 37: two different certificates are refused', () => {
  assert.equal(evaluateSignerReport(report37(PIN, [`V3.1 Signer: certificate SHA-256 digest: ${OTHER}`]), [PIN]).ok, false);
});

test('apksigner 37: a V2 signer (no minor version) is read, so the debug key is refused by name', () => {
  const debug = '061aa51997de8eab2ea1f8e7db0ccbc7cc043e4d829607c06c7596b12aba317a';
  const result = evaluateSignerReport(report37(PIN).replace('V3.0 Signer: certificate SHA-256 digest: ' + PIN, 'V2 Signer: certificate SHA-256 digest: ' + debug), [PIN]);
  assert.equal(result.ok, false);
  assert.match(result.message, new RegExp(debug));
});
