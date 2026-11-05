// Builds packages/mobile/dist, the webDir Capacitor copies into the iOS and Android apps:
// the @dilla/web build (as packages/desktop/scripts/build.mjs copies it), plus the mobile CSP, the bridge and the picker.
//   --client-dir=<dir>  use an existing web build instead of building @dilla/web (tests, CI reuse)
//   --out-dir=<dir>     write somewhere other than packages/mobile/dist (tests)
import { build } from 'esbuild';
import { execFileSync } from 'node:child_process';
import { cp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
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

if (!clientDir) {
  if (!process.env.npm_execpath) throw new Error('Run this build through npm run build -w @dilla/mobile');
  execFileSync(process.execPath, [process.env.npm_execpath, 'run', 'build', '-w', '@dilla/web'], { cwd: repo, stdio: 'inherit' });
}
const client = resolve(clientDir ?? resolve(repo, 'packages/web/dist'));

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
