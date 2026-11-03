import { createHash } from 'node:crypto';
import { appendFileSync } from 'node:fs';
import { lstat, readdir, readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { publicReleaseRecords } from './verify-release.mjs';

/** The signed release travels from `sign` to `publish` as an artifact; the
 * digest of its SHA256SUMS travels as a job output only `sign` can set. */
const SUMS = 'SHA256SUMS';
const MAX_FILE = 64 * 1024 ** 2;
const VERSION = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/;
const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');

export const EXPECTED_FILES = (channel, version, { staged }) => {
 if (typeof staged !== 'boolean') throw new Error('staged must be a boolean');
 if (typeof version !== 'string' || !VERSION.test(version)) throw new Error('Invalid desktop version');
 return [...publicReleaseRecords(channel), `dilla-${version}-linux-x64.sandbox.sig`, ...(staged ? ['rotation-proof.json'] : [])].sort();
};

async function exactFiles(directory, expected) {
 const present = (await readdir(directory)).sort();
 const missing = expected.filter(name => !present.includes(name)), unexpected = present.filter(name => !expected.includes(name));
 if (missing.length || unexpected.length) throw new Error(`The signed output must hold the exact file set (missing: ${missing.join(', ') || 'none'}; unexpected: ${unexpected.join(', ') || 'none'})`);
 const bytes = new Map();
 for (const name of expected) {
  const info = await lstat(join(directory, name));
  if (info.isSymbolicLink() || !info.isFile()) throw new Error('Signed output must be a regular file, not a symlink: ' + name);
  if (info.size > MAX_FILE) throw new Error('Signed output file is too large: ' + name);
  const content = await readFile(join(directory, name));
  if (content.length !== info.size) throw new Error('Signed output file changed: ' + name);
  bytes.set(name, content);
 }
 return bytes;
}
function sums(files) {
 return Buffer.from([...files.keys()].sort().map(name => `${sha256(files.get(name))}  ${name}\n`).join(''));
}

export async function writeDigest(directory, options) {
 const files = await exactFiles(directory, EXPECTED_FILES(options.channel, options.version, options));
 const content = sums(files);
 await writeFile(join(directory, SUMS), content, { flag: 'wx' });
 return sha256(content);
}

export async function verifyDigest(directory, expectedHex, options) {
 if (typeof expectedHex !== 'string' || !/^[a-f0-9]{64}$/.test(expectedHex)) throw new Error('Invalid signed output digest');
 const expected = EXPECTED_FILES(options.channel, options.version, options);
 const files = await exactFiles(directory, [...expected, SUMS].sort());
 const recorded = files.get(SUMS);
 if (sha256(recorded) !== expectedHex) throw new Error('SHA256SUMS does not match the digest the sign job reported');
 files.delete(SUMS);
 if (!sums(files).equals(recorded)) throw new Error('Signed output digest mismatch');
}

function stagedFlag(value) {
 if (value !== 'true' && value !== 'false') throw new Error('STAGED must be true or false');
 return value === 'true';
}
export async function cli(argv, env) {
 const [command, directory, channel, version, staged, digest] = argv;
 if (command === 'write' && argv.length === 5) {
  const hex = await writeDigest(resolve(directory), { channel, version, staged: stagedFlag(staged) });
  if (!/^[a-f0-9]{64}$/.test(hex) || !env.GITHUB_OUTPUT) throw new Error('Cannot write the digest output');
  appendFileSync(env.GITHUB_OUTPUT, `digest=${hex}\n`);
  return 'Digested the signed release output.';
 }
 if (command === 'verify' && argv.length === 6) {
  await verifyDigest(resolve(directory), digest, { channel, version, staged: stagedFlag(staged) });
  return 'The signed release output matches the sign job’s digest.';
 }
 throw new Error('Usage: candidate-digest.mjs write DIR CHANNEL VERSION STAGED | verify DIR CHANNEL VERSION STAGED DIGEST');
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) console.log(await cli(process.argv.slice(2), process.env));
