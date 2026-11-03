import { createHash, generateKeyPairSync, sign } from 'node:crypto';
import { copyFile, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildReleaseFixture, isolatedReleaseTools } from './release-fixture.mjs';
import { fakeGithub } from './fake-github.mjs';
import { PUBLIC_ACCEPTANCE_RECORDS } from '../../scripts/verify-release.mjs';

export const SHA = 'a'.repeat(40);
export const TREE = 'b'.repeat(40);

/**
 * A signed release as the sign job hands it over (DESKTOP_RELEASE_CANDIDATE), the
 * collected CI files (DESKTOP_INSTALLERS), fixture trust and an in-memory GitHub
 * whose tag points at SHA and whose ci run is the fixture's accepted run.
 */
/** A staged-rotation proof as release-sign-job.mjs writes it, signed by `keys`. */
export function rotationProof({ keys, version, channel, metadata, manifest, archive, slot = 'B' }) {
 const raw = key => key.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
 const entry = (file, bytes, signed, pair, publicKey) => ({ file, sha256: createHash('sha256').update(bytes).digest('hex'), publicKey, signature: sign(null, signed, pair.privateKey).toString('base64') });
 const proof = { format: 1, kind: 'dilla-desktop-rotation-proof', version, channel, stagedSlot: slot,
  metadata: entry(`desktop-${channel}.json`, metadata, metadata, keys.metadata, keys.metadata.publicKey.export({ type: 'spki', format: 'pem' }).toString()),
  sandbox: entry(`dilla-${version}-linux-x64.sandbox.json`, manifest, Buffer.concat([Buffer.from('Dilla Linux sandbox capsule v1\0'), manifest]), keys.sandbox, raw(keys.sandbox.publicKey)),
  sparkle: entry(`dilla-${version}-mac-arm64.zip`, archive, archive, keys.sparkle, raw(keys.sparkle.publicKey)) };
 proof.sandbox.context = 'Dilla Linux sandbox capsule v1\\0';
 return Buffer.from(JSON.stringify(proof, null, 2) + '\n');
}

export async function publishFixture({ sha = SHA, commit = sha, tree = TREE, staged = false, ...github } = {}) {
 const root = await mkdtemp(join(tmpdir(), 'dilla-publisher-'));
 const fixtureDirectory = join(root, 'fixture'); await mkdir(fixtureDirectory);
 const fixture = await buildReleaseFixture({ directory: fixtureDirectory, commit, head: commit, tree });
 const { version, channel } = fixture, tag = `desktop-v${version}`;
 const candidate = join(root, 'signed'), installers = join(root, 'installers');
 await mkdir(candidate); await mkdir(installers);
 await writeFile(join(candidate, `desktop-${channel}.json`), fixture.metadata);
 await writeFile(join(candidate, `desktop-${channel}.json.sig`), fixture.signature + '\n');
 for (const name of [...PUBLIC_ACCEPTANCE_RECORDS, 'appcast-arm64.xml', `dilla-${version}-linux-x64.sandbox.sig`]) await copyFile(join(fixtureDirectory, name), join(candidate, name));
 for (const name of Object.values(fixture.files).flatMap(byTarget => Object.values(byTarget).flat())) await copyFile(join(fixtureDirectory, name), join(installers, name));
 // Staged rotation: slot B's keys are pinned next to slot A's, and the candidate carries their proof.
 let sandboxConfig = fixture.sandbox.config, stagedKeys;
 if (staged) {
  stagedKeys = { metadata: generateKeyPairSync('ed25519'), sandbox: generateKeyPairSync('ed25519'), sparkle: generateKeyPairSync('ed25519') };
  const raw = key => key.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
  fixture.trust.metadataPublicKeys.push(stagedKeys.metadata.publicKey.export({ type: 'spki', format: 'pem' }).toString());
  fixture.trust.sparkleNextPublicKey = raw(stagedKeys.sparkle.publicKey);
  fixture.trust.signing = { active: 'A', staged: 'B' };
  sandboxConfig = { ...sandboxConfig, publicKeys: [...sandboxConfig.publicKeys, raw(stagedKeys.sandbox.publicKey)] };
  await writeFile(join(candidate, 'rotation-proof.json'), rotationProof({ keys: stagedKeys, version, channel, metadata: fixture.metadata,
   manifest: await readFile(join(fixtureDirectory, `dilla-${version}-linux-x64.sandbox.json`)), archive: await readFile(join(fixtureDirectory, `dilla-${version}-mac-arm64.zip`)) }));
 }
 const tools = await isolatedReleaseTools(join(root, 'tools'), { trust: fixture.trust, sandboxConfig });
 const gh = fakeGithub({ tags: { [tag]: sha }, runs: { [fixture.run.id]: fixture.run }, jobs: { [fixture.run.id]: fixture.jobs }, ...github });
 const env = {
  GITHUB_ACTIONS: 'true', GITHUB_REPOSITORY: 'dilla-chat/dilla', GITHUB_EVENT_NAME: 'workflow_run', GITHUB_REF: 'refs/heads/main',
  GH_TOKEN: 'workflow-token', RUNNER_TEMP: root, DESKTOP_TAG: tag, DESKTOP_SHA: sha, DESKTOP_STAGED: staged ? 'true' : 'false',
  DESKTOP_RELEASE_CANDIDATE: candidate, DESKTOP_INSTALLERS: installers,
 };
 const ancestors = new Set([sha]);
 const git = args => {
  if (args[0] === 'merge-base' && args[1] === '--is-ancestor' && args[3] === 'origin/main') { if (!ancestors.has(args[2])) throw new Error('not an ancestor'); return ''; }
  if (args[0] === 'rev-parse' && args[1] === `${commit}^{tree}`) return tree;
  throw new Error('Unexpected git call: ' + args.join(' '));
 };
 return { root, fixture, version, channel, tag, sha, tree, candidate, installers, tools, github: gh, env, git, ancestors, now: fixture.context.now, stagedKeys, cleanup: () => rm(root, { recursive: true, force: true }) };
}
