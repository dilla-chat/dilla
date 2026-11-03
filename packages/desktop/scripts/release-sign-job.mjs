import { createHash, createPrivateKey, createPublicKey, sign as ed25519Sign, verify } from 'node:crypto';
import { appendFileSync } from 'node:fs';
import { readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { signDesktopRelease, sparkleKeyFromSeed } from './release-feed.mjs';
import { parsePublicConfiguration } from './linux-sandbox-package.mjs';
import { macRotationOf, macUpdateKind } from './feed-format.mjs';
import { isMain } from './is-main.mjs';

/**
 * The Sign step of the keyed workflow: the only process that holds the private
 * keys. They arrive as single-line base64 environment variables, live only as
 * in-memory KeyObjects and Buffers, are removed from process.env before
 * signing, and are never written, printed or logged, nor any length or derived
 * form of them. Buffers are zeroed in `finally`; KeyObjects and V8 strings
 * cannot be zeroed, and /proc/<pid>/environ keeps the original values for the
 * process lifetime on the ephemeral runner.
 *
 *   DESKTOP_METADATA_KEY_A / _B   base64 of the PKCS8 DER of the Ed25519 metadata key
 *   DESKTOP_SPARKLE_KEY_A  / _B   base64 of the 32-byte Sparkle seed
 *   DESKTOP_SANDBOX_KEY_A  / _B   base64 of the PKCS8 DER of the Ed25519 sandbox key
 *
 * MAC_UPDATE (the gate's mac_update output) is 'dmg' only for a channel's rotation
 * release of a pending macOS key change: the slot the rotation starts from
 * (signing.macRotation.fromSlot) then also loads, and only its Sparkle seed is
 * used, to sign the appcast the installed Macs check (feed-format.mjs).
 */
const SLOTS = Object.freeze(['A', 'B']);
const KINDS = Object.freeze({ metadata: 'DESKTOP_METADATA_KEY', sparkle: 'DESKTOP_SPARKLE_KEY', sandbox: 'DESKTOP_SANDBOX_KEY' });
export const KEY_VARIABLES = Object.freeze(SLOTS.flatMap(slot => Object.values(KINDS).map(prefix => `${prefix}_${slot}`)));
const PKCS8_ED25519 = /^[A-Za-z0-9+/]{64}$/; // 48 DER bytes
const SEED = /^[A-Za-z0-9+/]{43}=$/; // 32 bytes
const SANDBOX_CONTEXT = 'Dilla Linux sandbox capsule v1\0';

function decode(env, name, shape, size) {
 const value = env[name];
 if (typeof value !== 'string' || !shape.test(value)) throw new Error(`${name} is missing or is not single-line base64 of the expected shape`);
 const bytes = Buffer.from(value, 'base64');
 if (bytes.length !== size) { bytes.fill(0); throw new Error(`${name} does not decode to the expected key size`); }
 return bytes;
}
function ed25519Private(der, name) {
 let key;
 try { key = createPrivateKey({ key: der, format: 'der', type: 'pkcs8' }); } catch { throw new Error(`${name} is not a PKCS8 private key`); }
 if (key.asymmetricKeyType !== 'ed25519') throw new Error(`${name} is not an Ed25519 key`);
 return key;
}

export function loadKeys(env, slot) {
 if (!SLOTS.includes(slot)) throw new Error('Invalid signing slot');
 const buffers = [];
 try {
  const metadataDer = decode(env, `${KINDS.metadata}_${slot}`, PKCS8_ED25519, 48); buffers.push(metadataDer);
  const sparkleSeed = decode(env, `${KINDS.sparkle}_${slot}`, SEED, 32); buffers.push(sparkleSeed);
  const sandboxDer = decode(env, `${KINDS.sandbox}_${slot}`, PKCS8_ED25519, 48); buffers.push(sandboxDer);
  const metadata = ed25519Private(metadataDer, `${KINDS.metadata}_${slot}`);
  const sandbox = ed25519Private(sandboxDer, `${KINDS.sandbox}_${slot}`);
  metadataDer.fill(0); sandboxDer.fill(0);
  return { metadata, sparkleSeed, sandbox, zero() { sparkleSeed.fill(0); } };
 } catch (error) {
  for (const bytes of buffers) bytes.fill(0);
  throw error;
 }
}

function signingSlots(trust) {
 const signing = trust?.signing;
 if (!signing || typeof signing !== 'object' || !SLOTS.includes(signing.active) || !(signing.staged === null || SLOTS.includes(signing.staged)) || signing.active === signing.staged) {
  throw new Error('release-trust.json must name the active signing slot (A or B) and a different staged slot or null');
 }
 return signing;
}

const rawEd25519Public = raw => createPublicKey({ key: { kty: 'OKP', crv: 'Ed25519', x: Buffer.from(raw, 'base64').toString('base64url') }, format: 'jwk' });
const rawOf = key => createPublicKey(key).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
const pemOf = key => createPublicKey(key).export({ type: 'spki', format: 'pem' }).toString();

/** The staged keys must already be pinned (and differ from the active ones) before anything is signed. */
function stagedPins(staged, active, trust, component) {
 const metadata = pemOf(staged.metadata);
 if (metadata === pemOf(active.metadata) || !Array.isArray(trust.metadataPublicKeys) || !trust.metadataPublicKeys.includes(metadata)) throw new Error('The staged metadata key is not in the pins');
 const sandbox = rawOf(staged.sandbox);
 if (sandbox === rawOf(active.sandbox) || !component.publicKeys.includes(sandbox)) throw new Error('The staged sandbox key is not in the pins');
 const sparkle = trust.sparkleNextPublicKey;
 if (typeof sparkle !== 'string' || !/^[A-Za-z0-9+/]{43}=$/.test(sparkle) || sparkle === trust.sparklePublicKey) throw new Error('The staged Sparkle key is not in the pins');
 // Dedicated keys: the staged Sparkle key is none of the metadata or sandbox keys, staged or pinned.
 const others = new Set([rawOf(staged.metadata), sandbox, ...component.publicKeys, ...trust.metadataPublicKeys.map(pem => rawOf(pem))]);
 if (others.has(sparkle)) throw new Error('The staged Sparkle key is not a dedicated key in the pins');
 const sparkleKey = sparkleKeyFromSeed(staged.sparkleSeed);
 if (rawOf(sparkleKey) !== sparkle) throw new Error('The staged Sparkle key is not in the pins');
 return { metadata, sandbox, sparkle, sparkleKey };
}

/** The staged keys sign the exact released bytes; each signature must verify under the pinned staged public key. */
async function writeRotationProof({ staged, slot, pins, version, channel, candidateDirectory, outputDirectory }) {
 const metadataFile = `desktop-${channel}.json`, manifestFile = `dilla-${version}-linux-x64.sandbox.json`, archiveFile = `dilla-${version}-mac-arm64.zip`;
 const metadata = await readFile(join(outputDirectory, metadataFile));
 const manifest = await readFile(join(candidateDirectory, manifestFile));
 const archive = await readFile(join(candidateDirectory, archiveFile));
 const message = Buffer.concat([Buffer.from(SANDBOX_CONTEXT), manifest]);
 const entries = [
  ['metadata', metadataFile, metadata, metadata, staged.metadata, createPublicKey(pins.metadata), pins.metadata],
  ['sandbox', manifestFile, manifest, message, staged.sandbox, rawEd25519Public(pins.sandbox), pins.sandbox],
  ['sparkle', archiveFile, archive, archive, pins.sparkleKey, rawEd25519Public(pins.sparkle), pins.sparkle],
 ];
 const proof = { format: 1, kind: 'dilla-desktop-rotation-proof', version, channel, stagedSlot: slot };
 for (const [kind, file, bytes, signed, privateKey, publicKey, published] of entries) {
  const signature = ed25519Sign(null, signed, privateKey);
  if (!verify(null, signed, publicKey, signature)) throw new Error(`The staged ${kind} signature does not verify under its pinned key`);
  proof[kind] = { file, sha256: createHash('sha256').update(bytes).digest('hex'), publicKey: published, signature: signature.toString('base64') };
 }
 proof.sandbox.context = 'Dilla Linux sandbox capsule v1\\0';
 await writeFile(join(outputDirectory, 'rotation-proof.json'), JSON.stringify(proof, null, 2) + '\n', { flag: 'wx' });
}

async function readComponent() {
 return parsePublicConfiguration(await readFile(new URL('../linux-sandbox-component.json', import.meta.url), 'utf8'));
}

export async function signInJob({ env, version, candidateDirectory, outputDirectory, trust, sign = signDesktopRelease, now }) {
 const loaded = [];
 try {
  if (env.GITHUB_ACTIONS !== 'true' || env.GITHUB_REF !== 'refs/heads/main' || env.GITHUB_EVENT_NAME !== 'workflow_run') throw new Error('Signing runs only in the main-branch workflow_run');
  const signing = signingSlots(trust);
  const macUpdate = macUpdateKind(env.MAC_UPDATE);
  const rotation = macUpdate === 'dmg' ? macRotationOf(trust) : null;
  if (macUpdate === 'dmg' && !rotation) throw new Error('A macOS disk image update is only allowed for the rotation release of a pending macOS key change');
  const active = loadKeys(env, signing.active); loaded.push(active);
  const staged = signing.staged === null ? null : loadKeys(env, signing.staged);
  if (staged) loaded.push(staged);
  const previous = rotation ? loadKeys(env, rotation.fromSlot) : null;
  if (previous) loaded.push(previous);
  removeKeyVariables(env);
  const pins = staged ? stagedPins(staged, active, trust, await readComponent()) : null;
  await sign({ version, artifactsDirectory: candidateDirectory, outputDirectory, macUpdate,
   keys: { metadata: active.metadata, sparkleSeed: active.sparkleSeed, sandbox: active.sandbox, ...(previous ? { feedSparkleSeed: previous.sparkleSeed } : {}) }, ...(now === undefined ? {} : { now }) });
  if (staged) await writeRotationProof({ staged, slot: signing.staged, pins, version, channel: version.includes('-beta.') ? 'beta' : 'stable', candidateDirectory, outputDirectory });
  return { staged: staged !== null };
 } finally {
  removeKeyVariables(env);
  for (const keys of loaded) keys.zero();
 }
}
function removeKeyVariables(env) {
 for (const name of KEY_VARIABLES) { delete env[name]; delete process.env[name]; }
}

if (isMain(import.meta.url)) {
 if (process.argv.length !== 5) throw new Error('Usage: release-sign-job.mjs VERSION CANDIDATE_DIR OUTPUT_DIR');
 const [version, candidateDirectory, outputDirectory] = process.argv.slice(2);
 const trust = JSON.parse(await readFile(new URL('../release-trust.json', import.meta.url), 'utf8'));
 const { staged } = await signInJob({ env: process.env, version, candidateDirectory: resolve(candidateDirectory), outputDirectory: resolve(outputDirectory), trust });
 if (!process.env.GITHUB_OUTPUT) throw new Error('Missing Actions output file');
 appendFileSync(process.env.GITHUB_OUTPUT, `staged=${staged ? 'true' : 'false'}\n`);
 console.log(`Signed desktop ${version} with the active slot${staged ? ' and wrote the staged-slot rotation proof' : ''}.`);
}
