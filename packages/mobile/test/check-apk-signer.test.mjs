import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { evaluateSignerReport } from '../scripts/check-apk-signer.mjs';

const PIN = '64abdc649513fd8ff6ffb3b1b65c0417e4b4a6669001db2a4c2ef7aa894c0580';
const OTHER = '1111111111111111111111111111111111111111111111111111111111111111';

// The shape of `apksigner verify -v --print-certs` (build-tools 35).
const report = ({ v2 = true, v3 = true, v1 = false, signers = [PIN] } = {}) => [
  'Verifies',
  `Verified using v1 scheme (JAR signing): ${v1}`,
  `Verified using v2 scheme (APK Signature Scheme v2): ${v2}`,
  `Verified using v3 scheme (APK Signature Scheme v3): ${v3}`,
  'Verified using v3.1 scheme (APK Signature Scheme v3.1): false',
  'Verified using v4 scheme (APK Signature Scheme v4): false',
  'Verified for SourceStamp: false',
  `Number of signers: ${signers.length}`,
  ...signers.flatMap((digest, index) => [
    `Signer #${index + 1} certificate DN: CN=Dilla`,
    `Signer #${index + 1} certificate SHA-256 digest: ${digest}`,
    `Signer #${index + 1} certificate SHA-1 digest: 0000000000000000000000000000000000000000`,
    `Signer #${index + 1} certificate MD5 digest: 00000000000000000000000000000000`,
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
