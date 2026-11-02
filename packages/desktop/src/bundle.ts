import { createHash } from 'node:crypto';
import { lstat, readFile, readdir, realpath } from 'node:fs/promises';
import { extname, join, relative, resolve } from 'node:path';
import { isAppNavigation } from './policy.ts';

type BundleFile = { bytes: Buffer; type: string };
export type Bundle = ReadonlyMap<string, BundleFile>;
const types: Record<string, string> = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.wasm': 'application/wasm', '.json': 'application/json', '.svg': 'image/svg+xml', '.png': 'image/png', '.ico': 'image/x-icon', '.woff': 'font/woff', '.woff2': 'font/woff2', '.bin': 'application/octet-stream', '.txt': 'text/plain' };

export async function loadBundle(root: string): Promise<Bundle> {
  const manifest = await readFile(join(root, 'dilla-manifest.json'));
  if (manifest.length > 1_048_576) throw new Error('Bundle manifest too large');
  const doc = JSON.parse(manifest.toString()) as { v?: unknown; files?: unknown };
  if (doc.v !== 1 || !Array.isArray(doc.files) || doc.files.length > 4096) throw new Error('Invalid bundle manifest');
  const files = new Map<string, BundleFile>();
  for (const entry of doc.files) {
    const { path, size, sha256 } = entry as Record<string, unknown>;
    if (typeof path !== 'string' || !/^[A-Za-z0-9_./-]+$/.test(path) || path.split('/').some(p => !p || p === '.' || p === '..')
      || files.has(path) || !types[extname(path)] || (extname(path) === '.html' && path !== 'index.html')
      || path === 'dilla-manifest.json' || typeof sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(sha256)
      || typeof size !== 'number' || !Number.isSafeInteger(size) || size < 0 || size > 64 * 1024 * 1024) throw new Error('Invalid bundle entry');
    const filePath = join(root, path);
    if (!(await lstat(filePath)).isFile() || relative(await realpath(root), await realpath(filePath)).replaceAll('\\', '/') !== path) throw new Error('Bundle path escapes root');
    const bytes = await readFile(filePath);
    if (bytes.length !== size || createHash('sha256').update(bytes).digest('hex') !== sha256) throw new Error(`Bundle integrity failed: ${path}`);
    files.set(path, { bytes, type: types[extname(path)] });
  }
  if (!files.has('index.html')) throw new Error('Missing bundled entry');
  // Refuse unlisted files as well as missing ones; there is no network fallback for assets.
  async function walk(dir: string): Promise<void> {
    for (const entry of await readdir(dir, { withFileTypes: true })) {
      const path = join(dir, entry.name);
      const rel = relative(resolve(root), path).replaceAll('\\', '/');
      if (entry.isDirectory()) await walk(path);
      else if (!entry.isFile() || (rel !== 'dilla-manifest.json' && !files.has(rel))) throw new Error(`Unlisted bundle file: ${rel}`);
    }
  }
  await walk(resolve(root));
  return files;
}

export function bundledResponse(bundle: Bundle, request: Request, instance: string): Response | null {
  const url = new URL(request.url);
  const origin = new URL(instance).origin;
  if (url.origin !== origin) return new Response('Forbidden', { status: 403 });
  // Only data/signalling paths may reach the server. Main-frame navigation is checked separately.
  const base = new URL(instance).pathname;
  const dataPath = /^\/(v1\/|i\/|gateway(?:\/|$)|rtc(?:\/|$))/;
  if (!url.pathname.startsWith('/assets/') && (dataPath.test(url.pathname) || (base !== '/' && url.pathname.startsWith(base) && dataPath.test(url.pathname.slice(base.length - 1))))) return null;
  if (request.method !== 'GET' && request.method !== 'HEAD') return new Response('Method not allowed', { status: 405 });
  if (url.pathname === '/favicon.ico') return new Response(null, { status: 204 });
  const path = isAppNavigation(request.url, instance) ? 'index.html' : url.pathname.slice(1);
  const file = bundle.get(path);
  if (!file) return new Response('Not found in installed client', { status: 404 });
  const assets = `${origin}/assets/`;
  const csp = `default-src 'none'; script-src ${assets} 'wasm-unsafe-eval'; style-src ${assets}; font-src ${assets}; worker-src ${assets}; img-src 'self' blob:; media-src blob:; connect-src 'self' https: wss:; object-src 'none'; base-uri 'none'; form-action 'none'; frame-src 'none'; frame-ancestors 'none'`;
  return new Response(request.method === 'HEAD' ? null : new Uint8Array(file.bytes), { headers: {
    'content-type': file.type, 'content-security-policy': csp, 'x-content-type-options': 'nosniff',
    'referrer-policy': 'no-referrer', 'cache-control': 'no-store',
    'permissions-policy': 'camera=(self), microphone=(self), display-capture=(self), geolocation=()',
  } });
}
