// Proves who signed a release APK: node scripts/check-apk-signer.mjs <apk>
// Passes only when `apksigner verify -v --print-certs` reports a v2 or newer signature by exactly one signer whose
// certificate SHA-256 is listed in release-trust.json (android.certSha256, a list so a key rotation can stage a second entry).
import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { homedir } from 'node:os';
import { fileURLToPath } from 'node:url';

/** Decide whether an `apksigner verify -v --print-certs` report is signed by a pinned key. */
export function evaluateSignerReport(output, pins) {
  const verified = scheme => new RegExp(`Verified using ${scheme} scheme[^\\n]*: true`).test(output);
  if (!verified('v2') && !verified('v3') && !verified('v3\\.1')) {
    return { ok: false, message: 'no v2/v3 signature (v1-only or unsigned APKs are not accepted)' };
  }
  const digests = [...output.matchAll(/^Signer #\d+ certificate SHA-256 digest: ([0-9a-fA-F]{64})\s*$/gm)].map(match => match[1].toLowerCase());
  const declared = /^Number of signers: (\d+)/m.exec(output);
  if (digests.length !== 1 || (declared && Number(declared[1]) !== 1)) {
    return { ok: false, message: `expected exactly one signer, found ${declared ? declared[1] : digests.length} signers (digests ${digests.join(', ') || 'none'})` };
  }
  const [digest] = digests;
  if (!pins.map(pin => pin.toLowerCase()).includes(digest)) {
    return { ok: false, message: `the signing certificate ${digest} is not in the pin list` };
  }
  return { ok: true, digest };
}

function apksigner() {
  const sdk = process.env.ANDROID_HOME ?? process.env.ANDROID_SDK_ROOT ?? join(homedir(), 'Android/Sdk');
  const tools = join(sdk, 'build-tools');
  const versions = existsSync(tools) ? readdirSync(tools).sort((a, b) => a.localeCompare(b, undefined, { numeric: true })) : [];
  const version = versions.at(-1);
  if (!version) throw new Error(`no Android build-tools under ${tools}`);
  return join(tools, version, 'apksigner');
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const apk = process.argv[2];
  if (!apk) { console.error('usage: check-apk-signer.mjs <apk>'); process.exit(2); }
  const trust = JSON.parse(readFileSync(new URL('../release-trust.json', import.meta.url), 'utf8'));
  let output;
  try {
    output = execFileSync(apksigner(), ['verify', '-v', '--print-certs', apk], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  } catch (error) {
    console.error(`${apk}: apksigner verify failed: ${String(error.stderr || error.stdout || error.message).split('\n').filter(line => line.trim() && !line.startsWith('WARNING:')).join(' ')}`);
    process.exit(1);
  }
  const result = evaluateSignerReport(output, trust.android.certSha256);
  if (!result.ok) { console.error(`${apk}: ${result.message}`); process.exit(1); }
  console.log(`${apk}: signed by the pinned release key ${result.digest}`);
}
