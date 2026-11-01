import { createHash, createPublicKey, verify, timingSafeEqual } from 'node:crypto';

export interface PublishedProgramManifest { url: string; size: number; sha512: string }
export interface Artifact { platform: string; arch: string; url: string; size: number; sha512: string; sparkleSignature?: string; programManifest?: PublishedProgramManifest }
export interface Release { format: 1; version: string; channel: 'stable' | 'beta'; publishedAt: string; expiresAt: string; artifacts: Artifact[] }
interface VerifyContext { publicKeys: readonly string[]; channel: 'stable' | 'beta'; platform: string; arch: string; currentVersion: string; now: number }
const versionPattern = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/;

function versionParts(value: string): number[] {
  const match = versionPattern.exec(value);
  if (!match) throw new Error('Invalid desktop version');
  const parts = [Number(match[1]), Number(match[2]), Number(match[3]), match[4] === undefined ? Number.MAX_SAFE_INTEGER : Number(match[4])];
  if (!parts.every(Number.isSafeInteger)) throw new Error('Invalid desktop version');
  return parts;
}

export function newerVersion(candidate: string, current: string): boolean {
  const a = versionParts(candidate), b = versionParts(current);
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return a[i] > b[i];
  return false;
}

/** Signature verification precedes parsing. The instance never supplies keys or update destinations. */
export function verifyRelease(bytes: Uint8Array, signature: string, ctx: VerifyContext): Release {
  if (ctx.publicKeys.length === 0) throw new Error('No desktop release signing key is installed');
  if (bytes.byteLength > 1_048_576 || !/^[A-Za-z0-9+/]{86}==$/.test(signature)) throw new Error('Invalid release signature');
  const valid = ctx.publicKeys.some(key => {
    try { const pub = createPublicKey(key); return pub.asymmetricKeyType === 'ed25519' && verify(null, bytes, pub, Buffer.from(signature, 'base64')); }
    catch { return false; }
  });
  if (!valid) throw new Error('Invalid release signature');
  const doc = JSON.parse(Buffer.from(bytes).toString('utf8')) as Release;
  if (!doc || doc.format !== 1 || doc.channel !== ctx.channel || typeof doc.version !== 'string'
    || !newerVersion(doc.version, ctx.currentVersion) || (doc.channel === 'stable' && doc.version.includes('-'))
    || (doc.channel === 'beta' && !doc.version.includes('-beta.'))) throw new Error('Invalid release version or channel');
  const published = Date.parse(doc.publishedAt), expires = Date.parse(doc.expiresAt);
  if (!Number.isFinite(published) || !Number.isFinite(expires) || published > ctx.now + 300_000 || expires <= ctx.now
    || expires <= published || expires - published > 93 * 86_400_000) throw new Error('Expired or invalid release dates');
  if (!Array.isArray(doc.artifacts) || doc.artifacts.length < 1 || doc.artifacts.length > 8) throw new Error('Invalid release artifacts');
  const platforms = new Set<string>();
  for (const artifact of doc.artifacts) {
    if (!artifact || !['linux', 'win32', 'darwin'].includes(artifact.platform) || !['x64', 'arm64'].includes(artifact.arch)) throw new Error('Invalid artifact target');
    const id = `${artifact.platform}-${artifact.arch}`;
    if (platforms.has(id)) throw new Error('Duplicate artifact target');
    platforms.add(id);
    if (typeof artifact.url !== 'string' || !/^https:\/\/github\.com\/jonasthim\/dilla\/releases\/download\/desktop-v[0-9A-Za-z.-]+\/[A-Za-z0-9_-][A-Za-z0-9_.-]*$/.test(artifact.url)
      || new URL(artifact.url).pathname !== `/jonasthim/dilla/releases/download/desktop-v${doc.version}/${artifact.url.split('/').at(-1)}`) throw new Error('Invalid artifact URL');
    if (artifact.platform === 'darwin' && (typeof artifact.sparkleSignature !== 'string' || !/^[A-Za-z0-9+/]{86}==$/.test(artifact.sparkleSignature))) throw new Error('Missing authenticated Sparkle archive signature');
    const extension = artifact.platform === 'linux' ? '.AppImage' : artifact.platform === 'win32' ? '.exe' : '.zip';
    if (!artifact.url.endsWith(extension) || artifact.url.split('/').at(-1)!.includes('..') || !Number.isSafeInteger(artifact.size)
      || artifact.size < 1 || artifact.size > 4 * 1024 ** 3 || typeof artifact.sha512 !== 'string'
      || !/^[A-Za-z0-9+/]{86}==$/.test(artifact.sha512)) throw new Error('Invalid artifact hash, type or size');
  }
  if (!platforms.has(`${ctx.platform}-${ctx.arch}`)) throw new Error('No update for this platform');
  return doc;
}

export function verifyArtifact(bytes: Uint8Array, artifact: Artifact): boolean {
  const hash = createHash('sha512').update(bytes).digest();
  const expected = Buffer.from(artifact.sha512, 'base64');
  return bytes.byteLength === artifact.size && expected.length === hash.length && timingSafeEqual(hash, expected);
}

export interface QuitGuard { activeCall: boolean; unsavedDraft: boolean }
interface UpdateDriver { cancelledQuit?(): void; check(): Promise<Release | null>; download(release: Release): Promise<void>; install(): void | Promise<void> }
export type UpdateState = { phase: 'idle' | 'checking' | 'downloading' | 'ready' | 'installing' | 'error'; message?: string };

export class UpdateCoordinator {
  state: UpdateState = { phase: 'idle' };
  readonly guards = new Map<string, QuitGuard | null>();
  private driver: UpdateDriver;
  private changed: (state: UpdateState) => void;
  constructor(driver: UpdateDriver, changed: (state: UpdateState) => void = () => {}) { this.driver = driver; this.changed = changed; }
  private set(state: UpdateState): void { this.state = state; this.changed(state); }
  expectGuard(instance: string): void { this.guards.set(instance, null); }
  setGuard(instance: string, value: QuitGuard): void { this.guards.set(instance, value); }
  removeGuard(instance: string): void { this.guards.delete(instance); }
  installationFailed(error: unknown): void {
    this.set({ phase: 'error', message: error instanceof Error ? error.message : 'Installation failed; try again' });
  }
  cancelledQuit(): void {
    if (this.state.phase === 'installing') this.driver.cancelledQuit?.();
    if (this.state.phase === 'installing') this.set({ phase: 'error', message: 'Restart was cancelled. The app is still running; retry after saving your drafts.' });
  }
  protected(): boolean { return [...this.guards.values()].some(guard => guard === null || guard.activeCall || guard.unsavedDraft); }
  async check(): Promise<void> {
    if (['checking', 'downloading', 'ready', 'installing'].includes(this.state.phase)) return;
    this.set({ phase: 'checking' });
    try {
      const release = await this.driver.check();
      if (!release) { this.set({ phase: 'idle' }); return; }
      this.set({ phase: 'downloading' });
      await this.driver.download(release);
      this.set({ phase: 'ready' });
    } catch (error) { this.set({ phase: 'error', message: error instanceof Error ? error.message : 'Update failed; try again' }); }
  }
  async quit(actualQuit: boolean): Promise<boolean> {
    if (!actualQuit || this.protected() || this.state.phase !== 'ready') return false;
    this.set({ phase: 'installing' });
    try { await this.driver.install(); return true; }
    catch (error) { this.set({ phase: 'error', message: error instanceof Error ? error.message : 'Installation failed; try again' }); return false; }
  }
}
