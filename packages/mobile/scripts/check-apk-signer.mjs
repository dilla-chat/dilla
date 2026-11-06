// Proves who signed a release APK: node scripts/check-apk-signer.mjs <apk>
// Passes only when `apksigner verify -v --print-certs` reports a v2 or newer signature by exactly one signer whose
// certificate SHA-256 is listed in release-trust.json (android.certSha256, a list so a key rotation can stage a second entry).
import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { homedir } from 'node:os';
import { fileURLToPath } from 'node:url';

/** Decide whether an `apksigner verify -v --print-certs` report is signed by a pinned key. Every match is a whole line. */
export function evaluateSignerReport(output, pins) {
  // apksigner up to 36 prints "Signer #1 certificate SHA-256 digest: …"; 37 prints "V2 Signer:" / "V3.0 Signer: certificate SHA-256
  // digest: …" (one line per signature scheme block). The same certificate named by several blocks is one signer.
  const digests = [...new Set([...output.matchAll(/^(?:Signer #\d+|V\d+(?:\.\d+)? Signer:) certificate SHA-256 digest: ([0-9a-fA-F]{64})$/gm)]
    .map(match => match[1].toLowerCase()))];
  const found = digests.length ? `certificate digest ${digests.join(', ')}` : 'no certificate digest';
  if (!/^Verifies$/m.test(output)) return { ok: false, message: `apksigner did not report "Verifies" (${found})` };
  const verified = scheme => new RegExp(`^Verified using ${scheme} scheme \\(.*\\): true$`, 'm').test(output);
  if (!verified('v2') && !verified('v3') && !verified('v3\\.1') && !verified('v3\\.2')) {
    return { ok: false, message: `no v2/v3 signature, v1-only is not accepted (${found})` };
  }
  const declared = /^Number of signers: (\d+)$/m.exec(output);
  if (digests.length !== 1 || !declared || Number(declared[1]) !== 1) {
    return { ok: false, message: `expected exactly one signer, but the report says ${declared ? `Number of signers: ${declared[1]}` : 'no "Number of signers" line'} with ${digests.length} certificate digests (${found})` };
  }
  const [digest] = digests;
  if (!pins.map(pin => pin.toLowerCase()).includes(digest)) {
    return { ok: false, message: `the signing certificate ${digest} is not in the pin list` };
  }
  return { ok: true, digest };
}

/** Numeric version order where a release beats its own -rc (36.0.0 > 36.0.0-rc1). */
function compareVersions(a, b) {
  const [aCore, aPre] = a.split('-', 2), [bCore, bPre] = b.split('-', 2);
  const numeric = aCore.localeCompare(bCore, undefined, { numeric: true });
  if (numeric) return numeric;
  if (!aPre !== !bPre) return aPre ? -1 : 1;
  return (aPre ?? '').localeCompare(bPre ?? '', undefined, { numeric: true });
}

function apksigner() {
  const sdk = process.env.ANDROID_HOME || process.env.ANDROID_SDK_ROOT || join(homedir(), 'Android/Sdk');
  const tools = join(sdk, 'build-tools');
  const versions = existsSync(tools) ? readdirSync(tools).sort(compareVersions) : [];
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
