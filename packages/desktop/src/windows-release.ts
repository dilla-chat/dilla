import { verifyRelease, type Release } from './update.ts';
import { canonicalSHA512, strictWindowsJSON } from './windows-manifest.ts';
type ReleaseContext = Parameters<typeof verifyRelease>[2];
function canonicalFields(value: object, required: string[], optional: string[] = []): void {
 const names = new Map([...required, ...optional].map(key => [key.toLowerCase(), key]));
 if (required.some(key => !Object.hasOwn(value, key)) || Object.keys(value).some(key => names.has(key.toLowerCase()) && names.get(key.toLowerCase()) !== key)) throw new Error('Ambiguous Windows release fields');
}
function date(value: string): void {
 if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{3})?Z$/.test(value)) throw new Error('Invalid Windows release ISO date');
 const timestamp = Date.parse(value), canonical = Number.isFinite(timestamp) ? new Date(timestamp).toISOString() : '';
 if (value !== canonical && value !== canonical.replace('.000Z', 'Z')) throw new Error('Invalid Windows release calendar date');
}
/** Windows' immutable native bootstrap shares this stricter, content-only
 * descriptor grammar. Other platforms retain their existing verification path. */
export function verifyWindowsRelease(raw: Uint8Array, signature: string, context: ReleaseContext): Release {
 // Authenticate before either the duplicate-key scanner or JSON interpretation.
 const release = verifyRelease(raw, signature, context);
 if (context.platform !== 'win32' || !canonicalSHA512(signature)) throw new Error('Invalid Windows release context/signature');
 strictWindowsJSON(raw);
 canonicalFields(release, ['format', 'version', 'channel', 'publishedAt', 'expiresAt', 'artifacts']);
 date(release.publishedAt); date(release.expiresAt);
 for (const artifact of release.artifacts) {
  canonicalFields(artifact, ['platform', 'arch', 'url', 'size', 'sha512'], ['sparkleSignature', 'programManifest']);
  if (!canonicalSHA512(artifact.sha512) || (artifact.sparkleSignature !== undefined && !canonicalSHA512(artifact.sparkleSignature))) throw new Error('Noncanonical Windows descriptor digest/signature');
  if (artifact.platform !== 'win32') { if (artifact.programManifest !== undefined) throw new Error('Program manifest on unsupported platform'); continue; }
  const manifest = artifact.programManifest;
  if (!manifest || Object.keys(manifest).length !== 3) throw new Error('Missing Windows program manifest');
  canonicalFields(manifest, ['url', 'size', 'sha512']);
  if (typeof manifest.url !== 'string' || !/^https:\/\/github\.com\/jonasthim\/dilla\/releases\/download\/desktop-v[0-9A-Za-z.-]+\/[A-Za-z0-9_-][A-Za-z0-9_.-]*\.manifest\.json$/.test(manifest.url)
   || manifest.url.split('/').at(-1)!.includes('..') || new URL(manifest.url).pathname !== `/jonasthim/dilla/releases/download/desktop-v${release.version}/${manifest.url.split('/').at(-1)}`
   || !Number.isSafeInteger(manifest.size) || manifest.size < 1 || manifest.size > 1_048_576 || !canonicalSHA512(manifest.sha512)) throw new Error('Invalid authenticated Windows program manifest descriptor');
 }
 return release;
}
