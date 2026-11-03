import { createHash, createPublicKey, verify, timingSafeEqual } from 'node:crypto';

export interface PublishedAsset { url: string; size: number; sha512: string }
export interface LinuxSandboxDescriptor { compatibilityEpoch: 1; minimumGeneration: number; bundle: PublishedAsset }
export interface Artifact { platform: string; arch: string; url: string; size: number; sha512: string; sparkleSignature?: string; linuxSandbox?: LinuxSandboxDescriptor }
export interface ReleaseAsset { name: string; size: number; sha512: string }
export interface Release { assets?: ReleaseAsset[]; format: 1; version: string; channel: 'stable' | 'beta'; publishedAt: string; expiresAt: string; artifacts: Artifact[] }
interface VerifyContext { publicKeys: readonly string[]; channel: 'stable' | 'beta'; platform: string; arch: string; currentVersion: string; now: number }
const versionPattern = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/;

const invalidReleaseJSON = (): never => { throw new Error('Invalid release JSON'); };
function strictReleaseJSON(raw: Uint8Array): unknown {
 const text = new TextDecoder('utf-8', { fatal: true }).decode(raw), parsed = JSON.parse(text); let i = 0;
 const ws = () => { while (i < text.length && /[ \t\r\n]/.test(text[i])) i++; };
 const str = () => { const start = i++; while (i < text.length) { const c = text[i++]; if (c === '\\') i++; else if (c === '"') return JSON.parse(text.slice(start, i)) as string; } return invalidReleaseJSON(); };
 const value = (depth: number): void => {
  if (depth > 16) invalidReleaseJSON(); ws();
  if (text[i] === '{') { i++; ws(); const seen = new Set<string>(); if (text[i] === '}') { i++; return; } for (;;) { ws(); if (text[i] !== '"') invalidReleaseJSON(); const key = str(); if (seen.has(key)) throw new Error('Duplicate release JSON field'); seen.add(key); ws(); if (text[i++] !== ':') invalidReleaseJSON(); value(depth + 1); ws(); const c = text[i++]; if (c === '}') return; if (c !== ',') invalidReleaseJSON(); } }
  if (text[i] === '[') { i++; ws(); if (text[i] === ']') { i++; return; } for (;;) { value(depth + 1); ws(); const c = text[i++]; if (c === ']') return; if (c !== ',') invalidReleaseJSON(); } }
  if (text[i] === '"') { str(); return; }
  const start = i; while (i < text.length && !/[ \t\r\n,\]}]/.test(text[i])) i++; const token = text.slice(start, i);
  if (!['true', 'false', 'null'].includes(token) && (!/^(0|[1-9][0-9]*)$/.test(token) || !Number.isSafeInteger(Number(token)))) invalidReleaseJSON();
 };
 value(0); ws(); if (i !== text.length) invalidReleaseJSON(); return parsed;
}

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
  const doc = strictReleaseJSON(bytes) as Release;
  if (!doc || doc.format !== 1 || doc.channel !== ctx.channel || typeof doc.version !== 'string'
    || !newerVersion(doc.version, ctx.currentVersion) || (doc.channel === 'stable' && doc.version.includes('-'))
    || (doc.channel === 'beta' && !doc.version.includes('-beta.'))) throw new Error('Invalid release version or channel');
  const published = Date.parse(doc.publishedAt), expires = Date.parse(doc.expiresAt);
  const validUTC = (value: unknown, time: number) => typeof value === 'string' && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/.test(value) && Number.isFinite(time) && new Date(time).toISOString().slice(0,19) === value.slice(0,19);
  if (!validUTC(doc.publishedAt, published) || !validUTC(doc.expiresAt, expires) || !Number.isFinite(published) || !Number.isFinite(expires) || published > ctx.now + 300_000 || expires <= ctx.now
    || expires <= published || expires - published > 93 * 86_400_000) throw new Error('Expired or invalid release dates');
  if (!Array.isArray(doc.artifacts) || doc.artifacts.length < 1 || doc.artifacts.length > 8) throw new Error('Invalid release artifacts');
  const platforms = new Set<string>();
  for (const artifact of doc.artifacts) {
    if (!artifact || !['linux', 'win32', 'darwin'].includes(artifact.platform) || !['x64', 'arm64'].includes(artifact.arch)) throw new Error('Invalid artifact target');
    const id = `${artifact.platform}-${artifact.arch}`;
    if (platforms.has(id)) throw new Error('Duplicate artifact target');
    platforms.add(id);
    if (typeof artifact.url !== 'string' || !/^https:\/\/github\.com\/dilla-chat\/dilla\/releases\/download\/desktop-v[0-9A-Za-z.-]+\/[A-Za-z0-9_-][A-Za-z0-9_.-]*$/.test(artifact.url)
      || new URL(artifact.url).pathname !== `/dilla-chat/dilla/releases/download/desktop-v${doc.version}/${artifact.url.split('/').at(-1)}`) throw new Error('Invalid artifact URL');
    if (artifact.platform === 'linux') {
      const sandbox = artifact.linuxSandbox, bundle = sandbox?.bundle;
      const exact = (v: object, keys: string[]) => !!v && typeof v === 'object' && !Array.isArray(v) && Object.keys(v).length === keys.length && keys.every(k => Object.hasOwn(v,k));
      if (!exact(artifact, ['platform','arch','url','size','sha512','linuxSandbox']) || artifact.arch !== 'x64' || artifact.url !== `https://github.com/dilla-chat/dilla/releases/download/desktop-v${doc.version}/dilla-${doc.version}-linux-x64.AppImage` || !sandbox || !exact(sandbox, ['compatibilityEpoch','minimumGeneration','bundle']) || sandbox.compatibilityEpoch !== 1 || !Number.isSafeInteger(sandbox.minimumGeneration) || sandbox.minimumGeneration < 1 || !bundle || !exact(bundle, ['url','size','sha512']) || bundle.url !== `https://github.com/dilla-chat/dilla/releases/download/desktop-v${doc.version}/dilla-${doc.version}-linux-x64.sandbox` || !Number.isSafeInteger(bundle.size) || bundle.size < 1 || bundle.size > 48 * 1024 ** 2 || typeof bundle.sha512 !== 'string' || Buffer.from(bundle.sha512, 'base64').length !== 64 || Buffer.from(bundle.sha512, 'base64').toString('base64') !== bundle.sha512) throw new Error('Invalid Linux sandbox descriptor');
      const names = [`dilla-${doc.version}-linux-x64.sandbox`, `dilla-${doc.version}-linux-x64.sandbox.json`, `dilla-${doc.version}-linux-x64.sandbox.sig`];
      if (!Array.isArray(doc.assets)) throw new Error('Missing authenticated Linux sandbox assets');
      const records = names.map(name => doc.assets!.filter(a => a?.name === name));
      if (records.some(entries => entries.length !== 1)) throw new Error('Missing or duplicate authenticated Linux sandbox assets');
      for (let index = 0; index < records.length; index++) { const a = records[index][0], limit = [48*1024**2,65536,89][index];
       if (!exact(a,['name','size','sha512']) || !Number.isSafeInteger(a.size) || a.size < 1 || a.size > limit || (index === 2 && a.size < 88) || typeof a.sha512 !== 'string' || Buffer.from(a.sha512,'base64').length !== 64 || Buffer.from(a.sha512,'base64').toString('base64') !== a.sha512) throw new Error('Invalid authenticated Linux sandbox asset');
      }
      if (records[0][0].size !== bundle.size || records[0][0].sha512 !== bundle.sha512) throw new Error('Linux sandbox descriptor and asset differ');
    } else if (artifact.linuxSandbox !== undefined) throw new Error('Invalid Linux sandbox descriptor target');
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
interface UpdateDriver { ownership?: 'package-manager'; cancelledQuit?(): void; check(): Promise<Release | null>; download(release: Release): Promise<void>; preparePrerequisites?(): Promise<void>; install(relaunch: boolean): void | Promise<void> }
export type UpdateState = { phase: 'idle' | 'checking' | 'downloading' | 'ready' | 'installing' | 'error'; message?: string }
 | { phase: 'available'; version: string; message: string };

export function desktopReleasePage(version: string): string {
 if (typeof version !== 'string' || versionPattern.exec(version)?.[0] !== version) throw new Error('Invalid desktop release version');
 versionParts(version);
 return `https://github.com/dilla-chat/dilla/releases/tag/desktop-v${version}`;
}

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
  async preparePrerequisites(): Promise<boolean> {
    if (this.driver.ownership === 'package-manager' || this.state.phase !== 'ready' || this.protected()) return false;
    try { await this.driver.preparePrerequisites?.(); return this.state.phase === 'ready'; }
    catch (error) { this.set({ phase: 'error', message: error instanceof Error ? error.message : 'Update preparation failed; try again' }); return false; }
  }
  async check(): Promise<void> {
    if (['checking', 'downloading', 'ready', 'installing'].includes(this.state.phase)) return;
    this.set({ phase: 'checking' });
    try {
      const release = await this.driver.check();
      if (!release) { this.set({ phase: 'idle' }); return; }
      if (this.driver.ownership === 'package-manager') {
        desktopReleasePage(release.version);
        this.set({ phase: 'available', version: release.version, message: `Dilla ${release.version} is available. Update using your package manager.` });
        return;
      }
      this.set({ phase: 'downloading' });
      await this.driver.download(release);
      this.set({ phase: 'ready' });
    } catch (error) { this.set({ phase: 'error', message: error instanceof Error ? error.message : 'Update failed; try again' }); }
  }
  async quit(actualQuit: boolean, relaunch = false): Promise<boolean> {
    if (this.driver.ownership === 'package-manager' || !actualQuit || this.protected() || this.state.phase !== 'ready') return false;
    this.set({ phase: 'installing' });
    try { await this.driver.install(relaunch); return true; }
    catch (error) { this.set({ phase: 'error', message: error instanceof Error ? error.message : 'Installation failed; try again' }); return false; }
  }
}
