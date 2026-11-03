import { glob, lstat, readFile, writeFile, mkdir, appendFile } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { join, resolve, basename } from 'node:path';
import { pathToFileURL } from 'node:url';

export const ACCOUNT_CHECKS = Object.freeze(['productionFuses', 'actualSignup', 'sameDeviceAfterUpgrade',
 'wrappedRecordUnchanged', 'encryptedHistoryReadable', 'draftBlockedInstallation', 'selectedFileBlockedInstallation',
 'selectedFileVerifiedByPeer', 'expiredSessionReauthenticated', 'recoveryEnrolledNewDevice']);
const PLATFORMS = { 'linux-x64': ['linux', 'x64'], 'win-x64': ['win32', 'x64'], 'mac-arm64': ['darwin', 'arm64'] };
function validate(proof, target, report) {
 const expected = PLATFORMS[target];
 if (!expected || proof?.platform !== expected[0] || proof.arch !== expected[1]) throw new Error('Installed-account platform mismatch');
 const fields = ['format', 'platform', 'arch', 'from', 'to', ...ACCOUNT_CHECKS,
  ...(report ? ['target', 'sourceCommit', 'sourceDirty', 'status'] : [])];
 if (Object.keys(proof).length !== fields.length || Object.keys(proof).some(key => !fields.includes(key))) throw new Error('Installed-account proof contains unexpected fields');
 if (proof.format !== 1 || proof.from !== '1.0.0' || proof.to !== '1.0.1' || ACCOUNT_CHECKS.some(key => proof[key] !== true)) throw new Error('Installed-account proof is incomplete');
}
export function verifyInstalledAccountReport(report, target, sourceCommit) {
 validate(report, target, true);
 if (report.target !== target || report.sourceCommit !== sourceCommit || !/^[a-f0-9]{40}$/.test(sourceCommit)
  || report.sourceDirty !== false || report.status !== 'passed') throw new Error('Installed-account source mismatch or unclean checkout');
}
/** Non-owning shards return null; an owning shard must produce exactly one
 * complete result. Uploads contain only these allowlisted, source-bound facts. */
export async function collectInstalledAccountProof(input, target, output, source) {
 const files = [];
 for await (const file of glob('**/installed-account-proof.json', { cwd: input })) files.push(join(input, file));
 if (!files.length) return null;
 if (files.length !== 1) throw new Error('Installed-account proof is ambiguous across cases');
 const info = await lstat(files[0]);
 if (!info.isFile() || info.isSymbolicLink() || info.size < 1 || info.size > 8192) throw new Error('Invalid installed-account proof file');
 const proof = JSON.parse(await readFile(files[0], 'utf8')); validate(proof, target, false);
 if (source.dirty !== false || !/^[a-f0-9]{40}$/.test(source.commit)) throw new Error('Installed-account proof requires a clean source checkout');
 const report = { ...proof, target, sourceCommit: source.commit, sourceDirty: false, status: 'passed' };
 verifyInstalledAccountReport(report, target, source.commit);
 await mkdir(output, { recursive: true });
 const file = join(output, `installed-account-${target}.json`);
 await writeFile(file, JSON.stringify(report) + '\n'); return { file, report };
}
if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
 const [input, target, output] = process.argv.slice(2);
 if (!output || process.argv.length !== 5 || !process.env.GITHUB_OUTPUT) throw new Error('Usage: installed-account-proof.mjs RESULTS TARGET OUTPUT (GitHub Actions only)');
 const repo = resolve(import.meta.dirname, '../../..');
 const git = args => execFileSync('git', args, { cwd: repo, encoding: 'utf8' }).trim();
 const result = await collectInstalledAccountProof(resolve(input), target, resolve(output), {
  commit: git(['rev-parse', 'HEAD']), dirty: git(['status', '--porcelain']) !== '',
 });
 await appendFile(process.env.GITHUB_OUTPUT, `present=${result !== null}\n`);
 console.log(result ? `Verified ${basename(result.file)}` : 'This shard does not own the installed-account case.');
}
