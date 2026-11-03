import { createHash } from 'node:crypto';
import { constants } from 'node:fs';
import { copyFile, lstat, mkdir, readdir, readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { BUILD_TARGETS, TARGETS, inventoryDigest } from './verify-release.mjs';
import { buildArtifactNames, buildProvenanceName, readBuildProvenanceFile } from './build-provenance.mjs';

/** Turns the accepted CI run's downloaded artifacts into a signing candidate.
 * Each artifact is downloaded into its own directory; nothing is ever merged,
 * overwritten or accepted outside the exact per-artifact file lists. */
const COMMIT = /^[a-f0-9]{40}$/;
const GATE_FILES = Object.freeze(['ci-run.json', 'ci-jobs.json']);
const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');

/** artifact name -> the only file names it may carry (all required). */
export function expectedArtifactFiles(version) {
 return new Map([
  ...BUILD_TARGETS.map(target => [`desktop-installers-${target}`, [...buildArtifactNames(target, version), buildProvenanceName(target)]]),
  ...TARGETS.map(target => [`desktop-performance-${target}`, [`performance-${target}.json`]]),
  ...TARGETS.map(target => [`desktop-installed-account-${target}`, [`installed-account-${target}.json`]]),
 ]);
}

async function regularFile(file, label) {
 const info = await lstat(file);
 if (info.isSymbolicLink() || !info.isFile()) throw new Error('Expected a regular file, not a symlink: ' + label);
 return info;
}

export async function collectArtifacts({ downloadRoot, candidateDirectory, version }) {
 const expected = expectedArtifactFiles(version);
 const root = await lstat(downloadRoot);
 if (root.isSymbolicLink() || !root.isDirectory()) throw new Error('Invalid artifact download directory');
 const present = await readdir(downloadRoot, { withFileTypes: true });
 for (const entry of present) {
  if (!expected.has(entry.name)) throw new Error('Unexpected downloaded artifact: ' + entry.name);
  if (entry.isSymbolicLink() || !entry.isDirectory()) throw new Error('Downloaded artifact must be a directory, not a symlink: ' + entry.name);
 }
 // Validate every artifact before copying anything.
 const copies = [], seen = new Set();
 for (const [artifact, names] of expected) {
  if (!present.some(entry => entry.name === artifact)) throw new Error('Missing downloaded artifact: ' + artifact);
  const directory = join(downloadRoot, artifact), info = await lstat(directory);
  if (info.isSymbolicLink() || !info.isDirectory()) throw new Error('Downloaded artifact must be a directory, not a symlink: ' + artifact);
  const files = await readdir(directory, { withFileTypes: true });
  for (const file of files) {
   if (!names.includes(file.name)) throw new Error(`Unexpected file in artifact ${artifact}: ${file.name}`);
   if (file.isSymbolicLink() || !file.isFile()) throw new Error(`Expected a regular file, not a symlink: ${artifact}/${file.name}`);
  }
  for (const name of names) {
   if (!files.some(file => file.name === name)) throw new Error(`Missing file in artifact ${artifact}: ${name}`);
   if (seen.has(name)) throw new Error('File name collision across artifacts: ' + name);
   seen.add(name);
   await regularFile(join(directory, name), `${artifact}/${name}`);
   copies.push([join(directory, name), name]);
  }
 }
 await mkdir(candidateDirectory, { recursive: true, mode: 0o700 });
 for (const [from, name] of copies) await copyFile(from, join(candidateDirectory, name), constants.COPYFILE_EXCL);
}

async function digestOf(candidateDirectory, name) {
 await regularFile(join(candidateDirectory, name), name);
 return { sha256: sha256(await readFile(join(candidateDirectory, name))) };
}

export async function assembleAcceptance({ candidateDirectory, run, jobs, version, commitTree }) {
 buildArtifactNames(BUILD_TARGETS[0], version);
 if (!run || !Number.isSafeInteger(run.id) || run.id < 1 || typeof run.head_sha !== 'string' || !COMMIT.test(run.head_sha)) throw new Error('Invalid accepted CI run');
 if (typeof commitTree !== 'string' || !COMMIT.test(commitTree)) throw new Error('Invalid commit tree');
 const inventory = { sha256: inventoryDigest(jobs, version) };
 const builds = {}, performance = {}, installedAccounts = {};
 for (const target of BUILD_TARGETS) builds[target] = { sha256: sha256(await readBuildProvenanceFile(join(candidateDirectory, buildProvenanceName(target)))) };
 for (const target of TARGETS) performance[target] = await digestOf(candidateDirectory, `performance-${target}.json`);
 for (const target of TARGETS) installedAccounts[target] = await digestOf(candidateDirectory, `installed-account-${target}.json`);
 const acceptance = {
  format: 1, ciRun: run.id, commit: run.head_sha, head: run.head_sha, tree: commitTree,
  review: { status: 'automated', ciRun: run.id, tree: commitTree, inventory },
  notes: await digestOf(candidateDirectory, 'release-notes.md'),
  builds, performance, installedAccounts,
 };
 return { acceptance };
}

async function readJson(file) { await regularFile(file, file); return JSON.parse(await readFile(file, 'utf8')); }

export async function cli(argv) {
 const [command, ...args] = argv;
 if (command === 'collect' && args.length === 4) {
  const [version, downloadRoot, candidateDirectory, gateDirectory] = args.map((value, i) => i === 0 ? value : resolve(value));
  await collectArtifacts({ downloadRoot, candidateDirectory, version });
  // After the artifacts: an artifact carrying either name has already failed, and nothing replaces the gate's copy.
  for (const name of GATE_FILES) {
   await regularFile(join(gateDirectory, name), name);
   await copyFile(join(gateDirectory, name), join(candidateDirectory, name), constants.COPYFILE_EXCL);
  }
  return 'Collected the accepted CI artifacts without merging.';
 }
 if (command === 'assemble' && args.length === 3) {
  const [version, directory, commitTree] = args, candidateDirectory = resolve(directory);
  const { acceptance } = await assembleAcceptance({ candidateDirectory, run: await readJson(join(candidateDirectory, 'ci-run.json')), jobs: await readJson(join(candidateDirectory, 'ci-jobs.json')), version, commitTree });
  await writeFile(join(candidateDirectory, 'acceptance.json'), JSON.stringify(acceptance, null, 2) + '\n', { flag: 'wx' });
  return 'Derived acceptance.json with the automated CI attestation.';
 }
 throw new Error('Usage: release-assemble.mjs collect VERSION DOWNLOAD_ROOT CANDIDATE_DIR GATE_DIR | assemble VERSION CANDIDATE_DIR COMMIT_TREE');
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) console.log(await cli(process.argv.slice(2)));
