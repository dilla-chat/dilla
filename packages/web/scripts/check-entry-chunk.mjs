// The build gate for what the page loads up front and for the one core wasm (L-WEB-51, web-3 decision 5).
// - The entry chunk (index.html's module script) and every chunk it preloads must not contain livekit or rnnoise:
//   both reach the page only through call/load.ts's dynamic import.
// - dist/assets holds exactly one dilla_core_wasm_bg-*.wasm (the core worker and the media worker share it) and
//   exactly one rnnoise-*.wasm.
// - No JavaScript names a dilla_core_wasm_bg*.wasm other than that one file: a `new URL` the bundler could not
//   resolve stays in the code verbatim and becomes a second download at runtime.
import { readdirSync, readFileSync, existsSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// livekit and rnnoise in any case (a class such as LiveKitRoom). Task 11 (merge 9+10 minor): the one exemption is the exact
// shipped copy that names the product, the voice settings' noise hint (strings/signalSettings.ts), which is taken out of the
// body before the scan; any other "RNNoise" (a renamed export, a log line) still fails.
export const ALLOWED_COPY = ['Falls back to RNNoise.'];
const FORBIDDEN_IN_ENTRY = ['livekit', 'rnnoise'];
const CORE_WASM = /^dilla_core_wasm_bg-[\w-]+\.wasm$/;
const RNNOISE_WASM = /^rnnoise-[\w-]+\.wasm$/;
const CORE_WASM_REF = /dilla_core_wasm_bg[\w.-]*?\.wasm/g;

function walk(dir, rel = '') {
  const paths = [];
  for (const entry of readdirSync(join(dir, rel), { withFileTypes: true })) {
    const path = rel ? `${rel}/${entry.name}` : entry.name;
    if (entry.isDirectory()) paths.push(...walk(dir, path));
    else if (entry.isFile()) paths.push(path);
  }
  return paths;
}

/**
 * The entry script and the chunks index.html preloads, as dist-relative paths.
 * Assumption (pinned): Vite's default `build.modulePreload` lists every chunk the entry imports statically as a
 * `<link rel="modulepreload">`. With modulePreload off, or a Vite that stops emitting those links, a static
 * import of livekit would escape this scan; such a change must make this check follow the entry's static imports.
 */
function entryChunks(html) {
  const chunks = [];
  for (const m of html.matchAll(/<script\b[^>]*\btype="module"[^>]*\bsrc="\/?([^"]+)"/gi)) chunks.push(m[1]);
  if (chunks.length === 0) return null;
  for (const m of html.matchAll(/<link\b[^>]*\brel="modulepreload"[^>]*\bhref="\/?([^"]+)"/gi)) chunks.push(m[1]);
  return chunks;
}

/** The mobile app's origin, which only the mobile build's worker names (client-core worker/mobile-build.ts). */
const MOBILE_MARKER = 'capacitor://localhost';

/** @returns {string[]} One line per problem, or [] when the build keeps the call code lazy and the core wasm single.
 *  `mobile`: the mobile app's build (DILLA_MOBILE_BUILD=1). Any other build must not carry the mobile worker branch:
 *  __DILLA_MOBILE__ false folds it away (review I1, Task 6). */
export function checkEntryChunk(dir, { mobile = false } = {}) {
  const problems = [];
  const html = existsSync(join(dir, 'index.html')) ? readFileSync(join(dir, 'index.html'), 'utf8') : '';
  const entries = entryChunks(html);
  if (entries === null) problems.push('index.html: no entry <script type="module" src>');
  for (const path of entries ?? []) {
    // A src that maps to no dist file (a changed base, a CDN URL) would otherwise pass having scanned nothing.
    if (!existsSync(join(dir, path))) {
      problems.push(`index.html: entry ${path} is not in dist`);
      continue;
    }
    const body = ALLOWED_COPY.reduce((text, copy) => text.split(copy).join(''), readFileSync(join(dir, path), 'utf8'));
    const lower = body.toLowerCase();
    for (const word of FORBIDDEN_IN_ENTRY) {
      if (lower.includes(word)) problems.push(`${path}: the entry chunk contains ${word}`);
    }
  }

  if (!mobile) {
    for (const path of walk(dir).filter(p => p.endsWith('.js')).sort()) {
      if (readFileSync(join(dir, path), 'utf8').includes(MOBILE_MARKER)) problems.push(`${path}: the mobile-only worker branch is in a web or desktop build`);
    }
  }

  const assets = existsSync(join(dir, 'assets')) ? readdirSync(join(dir, 'assets')) : [];
  const core = assets.filter(name => CORE_WASM.test(name));
  const rnnoise = assets.filter(name => RNNOISE_WASM.test(name));
  if (core.length !== 1) problems.push(`assets: expected exactly 1 dilla_core_wasm_bg-*.wasm, found ${core.length}`);
  if (rnnoise.length !== 1) problems.push(`assets: expected exactly 1 rnnoise-*.wasm, found ${rnnoise.length}`);

  if (core.length !== 1) return problems;
  for (const path of walk(dir).filter(p => p.endsWith('.js')).sort()) {
    const refs = readFileSync(join(dir, path), 'utf8').match(CORE_WASM_REF) ?? [];
    if (refs.some(ref => ref !== core[0])) {
      problems.push(`${path}: names dilla_core_wasm_bg.wasm, which is not the one emitted core wasm`);
    }
  }
  return problems;
}

/** The one output a mobile build (DILLA_MOBILE_BUILD=1) may write: packages/mobile/scripts/build.mjs's .web-build. */
const MOBILE_OUTPUT = resolve(dirname(fileURLToPath(import.meta.url)), '../../mobile/.web-build');

function main(argv) {
  if (!argv[2]) {
    console.error('usage: check-entry-chunk.mjs <dist-dir>');
    return 2;
  }
  const mobile = process.env.DILLA_MOBILE_BUILD !== undefined;
  // Review round 2, item 5: a mobile client in packages/web/dist would become the web and desktop client.
  if (mobile && resolve(argv[2]) !== MOBILE_OUTPUT) {
    console.error(`${argv[2]}: DILLA_MOBILE_BUILD is set, but this output is not packages/mobile/.web-build`);
    return 1;
  }
  const problems = checkEntryChunk(argv[2], { mobile });
  for (const problem of problems) console.error(problem);
  if (problems.length) return 1;
  console.log('entry chunk: no livekit, no rnnoise; one core wasm, one rnnoise wasm');
  return 0;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) process.exit(main(process.argv));
