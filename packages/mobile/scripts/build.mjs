// Builds packages/mobile/dist, the webDir Capacitor copies into the iOS and Android apps:
// the @dilla/web build (as packages/desktop/scripts/build.mjs copies it), plus the mobile CSP, the bridge and the picker.
//   --client-dir=<dir>  use an existing mobile build of @dilla/web instead of building one (tests). packages/web/dist
//                       is the web/desktop client (__DILLA_MOBILE__ false) and cannot run in the app.
//   --out-dir=<dir>     write somewhere other than packages/mobile/dist (tests)
import { build } from 'esbuild';
import { execFileSync } from 'node:child_process';
import { cp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { readdirSync, readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// Desktop's bundle.ts CSP with 'self' for the asset origin, since the assets are local. frame-ancestors is
// ignored in a <meta> policy, so it is left out. Capacitor's native bridge is injected as a document-start
// script (WKUserScript on iOS, addDocumentStartJavaScript on Android), which page CSP does not govern.
export const MOBILE_CSP = "default-src 'none'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; font-src 'self'; worker-src 'self'; img-src 'self' blob:; media-src blob:; connect-src 'self' https: wss:; object-src 'none'; base-uri 'none'; form-action 'none'; frame-src 'none'";

const option = name => {
  const values = process.argv.filter(value => value.startsWith(`--${name}=`));
  if (values.length > 1) throw new Error(`--${name} given twice`);
  return values[0]?.slice(name.length + 3);
};
const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const repo = resolve(root, '../..');
const out = resolve(option('out-dir') ?? resolve(root, 'dist'));
const clientDir = option('client-dir');

// The mobile client is its own build of @dilla/web, with __DILLA_MOBILE__ true (packages/web/vite.config.ts), written to
// .web-build/ so packages/web/dist (the web and desktop client, flag false) is never replaced by it.
const webBuild = resolve(root, '.web-build');
if (!clientDir) {
  const web = resolve(repo, 'packages/web');
  execFileSync(process.execPath, [resolve(repo, 'packages/media/scripts/extract-rnnoise-wasm.mjs')], { cwd: web, stdio: 'inherit' });
  execFileSync(process.execPath, [resolve(repo, 'node_modules/vite/bin/vite.js'), 'build', '--config', 'vite.config.ts', '--outDir', webBuild, '--emptyOutDir'],
    { cwd: web, stdio: 'inherit', env: { ...process.env, DILLA_MOBILE_BUILD: '1' } });
  execFileSync(process.execPath, [resolve(web, 'scripts/check-entry-chunk.mjs'), webBuild], { cwd: web, stdio: 'inherit', env: { ...process.env, DILLA_MOBILE_BUILD: '1' } });
  assertMobileWorker(webBuild);
}
const client = resolve(clientDir ?? webBuild);

/** The core worker in a mobile client must carry the mobile branch (the define reached the worker bundle). */
export function assertMobileWorker(dir) {
  const assets = resolve(dir, 'assets');
  const workers = readdirSync(assets).filter(name => /\.js$/.test(name) && readFileSync(resolve(assets, name), 'utf8').includes('dilla-core:'));
  if (!workers.some(name => readFileSync(resolve(assets, name), 'utf8').includes('capacitor://localhost'))) {
    throw new Error(`${dir}: the core worker has no mobile branch; was __DILLA_MOBILE__ defined for the worker build?`);
  }
}

await rm(out, { recursive: true, force: true });
await mkdir(out, { recursive: true });
await cp(client, out, { recursive: true });

const cspMeta = `<meta http-equiv="Content-Security-Policy" content="${MOBILE_CSP}">`;
function withCsp(html, name) {
  if (html.includes('http-equiv="Content-Security-Policy"')) throw new Error(`${name} already has a CSP meta`);
  const charset = /<meta charset="utf-8"\s*\/?>/i.exec(html);
  if (!charset) throw new Error(`${name} has no <meta charset> to anchor the CSP after`);
  const at = charset.index + charset[0].length;
  return `${html.slice(0, at)}\n    ${cspMeta}${html.slice(at)}`;
}

let index = withCsp(await readFile(resolve(out, 'index.html'), 'utf8'), 'index.html');
const entry = index.search(/<script[^>]*type="module"/);
if (entry < 0) throw new Error('index.html has no module entry');
// Classic and synchronous: window.dillaMobile exists before the module entry first reads it.
index = `${index.slice(0, entry)}<script src="/mobile-bridge.js"></script>\n    ${index.slice(entry)}`;
await writeFile(resolve(out, 'index.html'), index);

await writeFile(resolve(out, 'picker.html'), withCsp(await readFile(resolve(root, 'src/picker.html'), 'utf8'), 'picker.html'));
await cp(resolve(root, 'src/picker.css'), resolve(out, 'picker.css'));
const common = { bundle: true, platform: 'browser', target: ['es2022', 'safari17', 'chrome110'], format: 'iife', legalComments: 'none', logLevel: 'warning' };
await build({ ...common, entryPoints: [resolve(root, 'src/bridge.ts')], outfile: resolve(out, 'mobile-bridge.js') });
await build({ ...common, entryPoints: [resolve(root, 'src/picker.ts')], outfile: resolve(out, 'picker.js') });

// The web build's manifest no longer describes this tree; write it anew over every file.
execFileSync(process.execPath, [resolve(repo, 'scripts/write-manifest.mjs'), out], { stdio: 'inherit' });
